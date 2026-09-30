// Package adoeffects provides write-only Azure DevOps PR effect providers
// (PR comments and approval votes) behind the operations.Provider seam.
package adoeffects

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/operations"
	"github.com/SofiaFlux/summa42/internal/resources"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// organizationPattern mirrors internal/adomcp's validation so both providers
// accept the same set of organization names.
var organizationPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,49}$`)

// Config mirrors internal/adomcp Config: executable command plus organization,
// with a 15s default timeout applied by the constructors.
type Config struct {
	Command      string
	Organization string
	Timeout      time.Duration
}

// callToolFunc is the seam between providers and the ADO MCP session: plain
// maps, no MCP SDK types, so fakes implement it directly and production code
// adapts the real session to it.
type callToolFunc func(ctx context.Context, tool string, args map[string]any) (map[string]any, error)

// ReadFunc is the injected read capability used by lookup (Task 2). It is
// required non-nil at construction so verification can never be skipped.
type ReadFunc func(ctx context.Context, capability string, request any) (any, error)

// CommentIntent describes a single PR thread comment carrying an idempotency
// marker in its body.
type CommentIntent struct {
	Project    string `json:"project"`
	Repository string `json:"repository"`
	PR         int64  `json:"pr"`
	Path       string `json:"path"`
	Line       int64  `json:"line"`
	Body       string `json:"body"`
	Marker     string `json:"marker"`
}

// VoteIntent describes an approval vote. Only the approve vote (10) is
// supported; anything else is rejected in CanonicalIntent.
type VoteIntent struct {
	Project    string `json:"project"`
	Repository string `json:"repository"`
	PR         int64  `json:"pr"`
	Vote       int64  `json:"vote"`
}

// approveVote is the only vote value this package will ever cast.
const approveVote = 10

func (CommentIntent) DescriptorType() string { return "ado.pr.comment" }
func (VoteIntent) DescriptorType() string    { return "ado.pr.approve" }

// CommentProvider writes PR thread comments via ADO MCP.
type CommentProvider struct {
	config Config
	dial   func(context.Context) (callToolFunc, error)
	read   ReadFunc
}

// NewCommentProvider validates config (mirroring adomcp.New: trimmed command,
// organization pattern, 15s timeout default) and requires a non-nil read.
func NewCommentProvider(config Config, read ReadFunc) (*CommentProvider, error) {
	if err := validateConfig(&config); err != nil {
		return nil, err
	}
	if read == nil {
		return nil, errors.New("ADO comment provider requires a read function")
	}
	p := &CommentProvider{config: config, read: read}
	p.dial = p.connect
	return p, nil
}

// VoteProvider casts PR approval votes via ADO MCP.
type VoteProvider struct {
	config Config
	dial   func(context.Context) (callToolFunc, error)
	read   ReadFunc
}

// NewVoteProvider validates config the same way NewCommentProvider does.
func NewVoteProvider(config Config, read ReadFunc) (*VoteProvider, error) {
	if err := validateConfig(&config); err != nil {
		return nil, err
	}
	if read == nil {
		return nil, errors.New("ADO vote provider requires a read function")
	}
	p := &VoteProvider{config: config, read: read}
	p.dial = p.connect
	return p, nil
}

func validateConfig(config *Config) error {
	config.Command = strings.TrimSpace(config.Command)
	config.Organization = strings.TrimSpace(config.Organization)
	if config.Command == "" || !organizationPattern.MatchString(config.Organization) {
		return errors.New("ADO effects requires an executable and a valid organization name")
	}
	if config.Timeout <= 0 {
		config.Timeout = 15 * time.Second
	}
	return nil
}

func (p *CommentProvider) Name() string { return "ado-pr-comment" }

func (p *CommentProvider) Capability() string { return "ado.pr.comment" }

func (p *CommentProvider) EnforcementLevel() domain.EnforcementLevel {
	return domain.EnforcementEnforced
}

func (p *CommentProvider) AdapterVersion() string { return "ado-effects-v1" }

func (p *CommentProvider) AdapterVersionSemanticallyRelevant() bool { return false }

func (p *VoteProvider) Name() string { return "ado-pr-vote" }

func (p *VoteProvider) Capability() string { return "ado.pr.approve" }

func (p *VoteProvider) EnforcementLevel() domain.EnforcementLevel {
	return domain.EnforcementEnforced
}

func (p *VoteProvider) AdapterVersion() string { return "ado-effects-v1" }

func (p *VoteProvider) AdapterVersionSemanticallyRelevant() bool { return false }

// CanonicalIntent accepts CommentIntent by value or pointer, trims and
// validates it, and returns the stable canonical JSON form.
func (p *CommentProvider) CanonicalIntent(descriptor operations.IntentDescriptor) ([]byte, error) {
	var intent CommentIntent
	switch typed := descriptor.(type) {
	case CommentIntent:
		intent = typed
	case *CommentIntent:
		if typed == nil {
			return nil, errors.New("ADO comment intent is nil")
		}
		intent = *typed
	default:
		return nil, fmt.Errorf("comment provider rejects descriptor type %T", descriptor)
	}
	intent.Project = strings.TrimSpace(intent.Project)
	intent.Repository = strings.TrimSpace(intent.Repository)
	intent.Path = strings.TrimSpace(intent.Path)
	intent.Body = strings.TrimSpace(intent.Body)
	intent.Marker = strings.TrimSpace(intent.Marker)
	if intent.Project == "" || intent.Repository == "" || intent.PR <= 0 ||
		intent.Body == "" || intent.Marker == "" {
		return nil, errors.New("ADO comment intent requires project, repository, PR, body, and marker")
	}
	return json.Marshal(intent)
}

// CanonicalIntent accepts VoteIntent by value or pointer; only the approve
// vote (10) is accepted.
func (p *VoteProvider) CanonicalIntent(descriptor operations.IntentDescriptor) ([]byte, error) {
	var intent VoteIntent
	switch typed := descriptor.(type) {
	case VoteIntent:
		intent = typed
	case *VoteIntent:
		if typed == nil {
			return nil, errors.New("ADO vote intent is nil")
		}
		intent = *typed
	default:
		return nil, fmt.Errorf("vote provider rejects descriptor type %T", descriptor)
	}
	intent.Project = strings.TrimSpace(intent.Project)
	intent.Repository = strings.TrimSpace(intent.Repository)
	if intent.Project == "" || intent.Repository == "" || intent.PR <= 0 {
		return nil, errors.New("ADO vote intent requires project, repository, and PR")
	}
	if intent.Vote != approveVote {
		return nil, fmt.Errorf("ADO vote intent rejects vote %d: only the approve vote (%d) is supported", intent.Vote, approveVote)
	}
	return json.Marshal(intent)
}

func (p *CommentProvider) CostProfile(descriptor operations.IntentDescriptor) (operations.CostProfile, error) {
	if _, err := p.CanonicalIntent(descriptor); err != nil {
		return operations.CostProfile{}, err
	}
	return operations.CostProfile{MaxExposure: 1, Enforceability: resources.Enforceability{
		CostControl: resources.CostTechnicallyCapped, RequireHardCap: false, Source: "ado pr comment",
	}}, nil
}

func (p *VoteProvider) CostProfile(descriptor operations.IntentDescriptor) (operations.CostProfile, error) {
	if _, err := p.CanonicalIntent(descriptor); err != nil {
		return operations.CostProfile{}, err
	}
	return operations.CostProfile{MaxExposure: 1, Enforceability: resources.Enforceability{
		CostControl: resources.CostTechnicallyCapped, RequireHardCap: false, Source: "ado pr vote",
	}}, nil
}

// Dispatch writes the comment, then returns CONFIRMED_EFFECT. CONFIRMED
// means transport-accepted; LookupOutcome is the source of read-back truth
// (reconciler re-checks). Outcome
// semantics mirror internal/fieldfeedback Dispatch: the service settles
// CONFIRMED_EFFECT / CONFIRMED_NO_EFFECT and marks anything else (including
// OUTCOME_UNKNOWN) unknown, so Dispatch returns the confirmed state directly
// after a successful write. Read-back verification is NOT done here — it
// lives in LookupOutcome (reconcile path), keeping Dispatch to exactly one
// write call so a retry never duplicates the effect blindly.
func (p *CommentProvider) Dispatch(ctx context.Context, request operations.ProviderDispatchRequest) (operations.ProviderOutcome, error) {
	intent, err := decodeCommentIntent(request.CanonicalIntent)
	if err != nil {
		return operations.ProviderOutcome{}, err
	}
	callCtx, cancel := context.WithTimeout(ctx, p.config.Timeout)
	defer cancel()
	call, err := p.dial(callCtx)
	if err != nil {
		return operations.ProviderOutcome{}, fmt.Errorf("connect ADO MCP: %w", err)
	}
	body := intent.Body + "\n" + intent.Marker
	result, err := call(callCtx, "repo_pull_request_thread_write", map[string]any{
		"action": "create", "project": intent.Project, "repositoryId": intent.Repository,
		"pullRequestId": intent.PR,
		"content":       body,
		"status":        "Active",
	})
	if err != nil {
		return operations.ProviderOutcome{}, err
	}
	threadID, _ := stringField(result, "threadId", "thread_id", "id")
	return operations.ProviderOutcome{
		State:             domain.OperationConfirmedEffect,
		ProviderReference: commentReference(intent, threadID),
		ActualCost:        1,
	}, nil
}

// Dispatch casts the approval vote, then returns CONFIRMED_EFFECT with the
// same outcome semantics as CommentProvider.Dispatch. CONFIRMED means
// transport-accepted; LookupOutcome is the source of read-back truth
// (reconciler re-checks).
func (p *VoteProvider) Dispatch(ctx context.Context, request operations.ProviderDispatchRequest) (operations.ProviderOutcome, error) {
	intent, err := decodeVoteIntent(request.CanonicalIntent)
	if err != nil {
		return operations.ProviderOutcome{}, err
	}
	callCtx, cancel := context.WithTimeout(ctx, p.config.Timeout)
	defer cancel()
	call, err := p.dial(callCtx)
	if err != nil {
		return operations.ProviderOutcome{}, fmt.Errorf("connect ADO MCP: %w", err)
	}
	voteValue, err := adoVoteEnum(intent.Vote)
	if err != nil {
		return operations.ProviderOutcome{}, err
	}
	result, err := call(callCtx, "repo_pull_request_write", map[string]any{
		"action": "vote", "project": intent.Project, "repositoryId": intent.Repository,
		"pullRequestId": intent.PR, "vote": voteValue,
	})
	if err != nil {
		return operations.ProviderOutcome{}, err
	}
	reference, err := stringField(result, "id", "voteId", "vote_id")
	if err != nil {
		// Deterministic fallback: the vote call returns little structured
		// identity, so derive a stable reference from the confirmed intent.
		reference = voteReference(intent)
	}
	return operations.ProviderOutcome{
		State:             domain.OperationConfirmedEffect,
		ProviderReference: reference,
		ActualCost:        1,
	}, nil
}

// LookupOutcome reconciles a comment write without repeating it: a read
// error yields OUTCOME_UNKNOWN with a nil error (the reconciler retries the
// lookup; transport failure is not a negative), a thread carrying the marker
// confirms the effect, and anything else stays unknown.
func (p *CommentProvider) LookupOutcome(ctx context.Context, request operations.ProviderDispatchRequest) (operations.ProviderOutcome, error) {
	intent, err := decodeCommentIntent(request.CanonicalIntent)
	if err != nil {
		return operations.ProviderOutcome{}, err
	}
	raw, err := p.read(ctx, "ado.pr.threads", map[string]any{"action": "list", "project": intent.Project, "repositoryId": intent.Repository, "pullRequestId": intent.PR})
	if err != nil {
		return operations.ProviderOutcome{State: domain.OperationOutcomeUnknown}, nil
	}
	if threadID, found := threadContainsMarker(raw, intent.Marker); found {
		return operations.ProviderOutcome{State: domain.OperationConfirmedEffect, ProviderReference: commentReference(intent, threadID)}, nil
	}
	return operations.ProviderOutcome{State: domain.OperationOutcomeUnknown}, nil
}

// LookupOutcome reconciles an approval vote via the existing ado.pr.get
// reviewers array: any reviewer carrying the intended vote confirms the
// effect (server-identity-agnostic: the write executes as one identity),
// otherwise unknown. A read error yields OUTCOME_UNKNOWN with nil error.
func (p *VoteProvider) LookupOutcome(ctx context.Context, request operations.ProviderDispatchRequest) (operations.ProviderOutcome, error) {
	intent, err := decodeVoteIntent(request.CanonicalIntent)
	if err != nil {
		return operations.ProviderOutcome{}, err
	}
	raw, err := p.read(ctx, "ado.pr.get", map[string]any{"action": "get", "project": intent.Project, "repositoryId": intent.Repository, "pullRequestId": intent.PR})
	if err != nil {
		return operations.ProviderOutcome{State: domain.OperationOutcomeUnknown}, nil
	}
	if reviewersContainVote(raw, intent.Vote) {
		return operations.ProviderOutcome{State: domain.OperationConfirmedEffect, ProviderReference: voteReference(intent)}, nil
	}
	return operations.ProviderOutcome{State: domain.OperationOutcomeUnknown}, nil
}

// adoVoteEnum maps Azure DevOps' numeric vote codes to the string enum the
// real repo_pull_request_write "vote" action requires (confirmed against
// the live tool schema: Approved/ApprovedWithSuggestions/NoVote/
// WaitingForAuthor/Rejected — never a raw integer). CanonicalIntent already
// only accepts approveVote (10), so this always resolves to "Approved"
// today; the full table is kept for when a future intent needs the others.
func adoVoteEnum(vote int64) (string, error) {
	switch vote {
	case 10:
		return "Approved", nil
	case 5:
		return "ApprovedWithSuggestions", nil
	case 0:
		return "NoVote", nil
	case -5:
		return "WaitingForAuthor", nil
	case -10:
		return "Rejected", nil
	default:
		return "", fmt.Errorf("ADO vote %d has no known enum mapping", vote)
	}
}

// commentReference unifies the ProviderReference for Dispatch and
// LookupOutcome (controller decision): the confirmed write carries the
// thread identity when the tool result provides one, otherwise a
// deterministic fallback (repository, PR, 8-char marker hash) so the
// reference is always stable and non-empty.
func commentReference(intent CommentIntent, threadID string) string {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		sum := sha256.Sum256([]byte(intent.Marker))
		threadID = hex.EncodeToString(sum[:4])
	}
	return fmt.Sprintf("ado-pr-comment:%s#%d:%s", intent.Repository, intent.PR, threadID)
}

func voteReference(intent VoteIntent) string {
	return fmt.Sprintf("ado-pr-vote:%s/%s/%d/%d", intent.Project, intent.Repository, intent.PR, intent.Vote)
}

// threadContainsMarker walks a threads payload ({"threads": [...]}, each
// thread holding comments[].content) for a substring match of marker. It
// returns the matched thread's identity ("" when the payload carries none)
// and whether the marker was found.
func threadContainsMarker(raw any, marker string) (string, bool) {
	payload, ok := raw.(map[string]any)
	if !ok || marker == "" {
		return "", false
	}
	for _, thread := range anySlice(payload["threads"]) {
		threadMap, ok := thread.(map[string]any)
		if !ok {
			continue
		}
		for _, comment := range anySlice(threadMap["comments"]) {
			commentMap, ok := comment.(map[string]any)
			if !ok {
				continue
			}
			content, _ := commentMap["content"].(string)
			if content != "" && strings.Contains(content, marker) {
				return threadIdentity(threadMap), true
			}
		}
	}
	return "", false
}

func anySlice(value any) []any {
	switch typed := value.(type) {
	case []any:
		return typed
	case []map[string]any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, item)
		}
		return out
	default:
		return nil
	}
}

func threadIdentity(thread map[string]any) string {
	for _, key := range []string{"threadId", "thread_id", "id"} {
		if value, ok := thread[key]; ok {
			if text := strings.TrimSpace(fmt.Sprint(value)); text != "" && text != "<nil>" {
				return text
			}
		}
	}
	return ""
}

// reviewersContainVote reports whether any reviewer in a reviewers payload
// ({"reviewers": [{"vote": N}]}) carries the intended vote.
func reviewersContainVote(raw any, vote int64) bool {
	payload, ok := raw.(map[string]any)
	if !ok {
		return false
	}
	for _, reviewer := range anySlice(payload["reviewers"]) {
		reviewerMap, ok := reviewer.(map[string]any)
		if !ok {
			continue
		}
		if voteEquals(reviewerMap["vote"], vote) {
			return true
		}
	}
	return false
}

// voteEquals compares a decoded reviewer vote against the intent vote,
// tolerating the JSON number shapes a read payload may carry.
func voteEquals(value any, want int64) bool {
	switch typed := value.(type) {
	case float64:
		return typed == float64(want)
	case float32:
		return typed == float32(want)
	case int:
		return int64(typed) == want
	case int32:
		return int64(typed) == want
	case int64:
		return typed == want
	case json.Number:
		if parsed, err := typed.Int64(); err == nil {
			return parsed == want
		}
		if parsed, err := typed.Float64(); err == nil {
			return parsed == float64(want)
		}
		return false
	default:
		return false
	}
}

func decodeCommentIntent(canonical []byte) (CommentIntent, error) {
	var intent CommentIntent
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&intent); err != nil {
		return CommentIntent{}, fmt.Errorf("decode ADO comment canonical intent: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return CommentIntent{}, errors.New("ADO comment canonical intent contains trailing JSON")
		}
		return CommentIntent{}, fmt.Errorf("decode ADO comment canonical intent trailer: %w", err)
	}
	provider := &CommentProvider{}
	if _, err := provider.CanonicalIntent(intent); err != nil {
		return CommentIntent{}, fmt.Errorf("ADO comment canonical intent failed re-validation: %w", err)
	}
	return intent, nil
}

func decodeVoteIntent(canonical []byte) (VoteIntent, error) {
	var intent VoteIntent
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&intent); err != nil {
		return VoteIntent{}, fmt.Errorf("decode ADO vote canonical intent: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return VoteIntent{}, errors.New("ADO vote canonical intent contains trailing JSON")
		}
		return VoteIntent{}, fmt.Errorf("decode ADO vote canonical intent trailer: %w", err)
	}
	provider := &VoteProvider{}
	if _, err := provider.CanonicalIntent(intent); err != nil {
		return VoteIntent{}, fmt.Errorf("ADO vote canonical intent failed re-validation: %w", err)
	}
	return intent, nil
}

func stringField(result map[string]any, keys ...string) (string, error) {
	for _, key := range keys {
		if value, ok := result[key]; ok {
			if text := strings.TrimSpace(fmt.Sprint(value)); text != "" && text != "<nil>" {
				return text, nil
			}
		}
	}
	return "", fmt.Errorf("result has none of the fields %v", keys)
}

type mcpSession interface {
	CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error)
	Close() error
}

func (p *CommentProvider) connect(ctx context.Context) (callToolFunc, error) {
	return productionDial(ctx, p.config)
}

func (p *VoteProvider) connect(ctx context.Context) (callToolFunc, error) {
	return productionDial(ctx, p.config)
}

// productionDial spawns the ADO MCP server and adapts its session to the
// callToolFunc seam, mirroring internal/adomcp connect. The session is closed
// after the single dispatched call returns.
func productionDial(ctx context.Context, config Config) (callToolFunc, error) {
	cmd := exec.CommandContext(ctx, config.Command, config.Organization)
	cmd.Env = adoEnvironment()
	client := mcp.NewClient(&mcp.Implementation{Name: "summa42-ado-effects", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return nil, fmt.Errorf("connect ADO MCP: %w", err)
	}
	return func(ctx context.Context, tool string, args map[string]any) (map[string]any, error) {
		defer session.Close()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
		if err != nil {
			return nil, fmt.Errorf("ADO MCP %s: %w", tool, err)
		}
		if result == nil || result.IsError {
			return nil, fmt.Errorf("ADO MCP %s returned a tool error", tool)
		}
		return toolResultMap(result)
	}, nil
}

// toolResultMap converts a tool result to a plain map, preferring structured
// content and falling back to JSON text content. Real Azure DevOps MCP write
// responses are wrapped in the same untrusted-content banner as reads
// (confirmed live); stripUntrustedBanner mirrors internal/adomcp's helper so
// a banner-wrapped object decodes instead of silently degrading to the
// {"text": ...} fallback below.
func toolResultMap(result *mcp.CallToolResult) (map[string]any, error) {
	if result.StructuredContent != nil {
		raw, err := json.Marshal(result.StructuredContent)
		if err != nil {
			return nil, fmt.Errorf("marshal ADO MCP structured content: %w", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return nil, fmt.Errorf("decode ADO MCP structured content: %w", err)
		}
		return decoded, nil
	}
	var texts []string
	for _, item := range result.Content {
		if text, ok := item.(*mcp.TextContent); ok {
			texts = append(texts, text.Text)
		}
	}
	joined := stripUntrustedBanner(strings.TrimSpace(strings.Join(texts, "\n")))
	if joined == "" {
		return nil, errors.New("ADO MCP returned no content")
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(joined), &decoded); err != nil {
		return map[string]any{"text": joined}, nil
	}
	return decoded, nil
}

// untrustedContentBanner mirrors internal/adomcp's banner-stripping regex
// (kept as a small, dependency-free duplicate the same way adoEnvironment
// below already mirrors internal/adomcp's copy).
var untrustedContentBanner = regexp.MustCompile(`(?s)^\s*<<([0-9a-fA-F]{8,})>>\s*\[.*?\]\s*<<[0-9a-fA-F]{8,}>>\s*\r?\n(.*?)\r?\n?\s*<<\s*/\s*[0-9a-fA-F]{8,}\s*>>\s*$`)

func stripUntrustedBanner(text string) string {
	if m := untrustedContentBanner.FindStringSubmatch(text); m != nil {
		return m[2]
	}
	return text
}

// adoEnvironment mirrors internal/adomcp adoEnvironment; export a shared
// helper as follow-on.
func adoEnvironment() []string {
	allowed := map[string]bool{
		"PATH": true, "HOME": true, "USERPROFILE": true, "APPDATA": true, "LOCALAPPDATA": true,
		"AZURE_CONFIG_DIR": true, "AZURE_DEVOPS_EXT_PAT": true,
		"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true,
		"SSL_CERT_FILE": true, "NODE_EXTRA_CA_CERTS": true,
	}
	var result []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if allowed[key] {
			result = append(result, entry)
		}
	}
	return result
}

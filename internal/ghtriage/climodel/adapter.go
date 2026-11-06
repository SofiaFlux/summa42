package climodel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/capacity"
	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/modelrun"
)

// ResolveRunAsUser is the fail-closed dedicated-user gate shared by every model
// launcher; see modelrun.
func ResolveRunAsUser(name string) (*user.User, error) { return modelrun.ResolveRunAsUser(name) }

type Config struct {
	// RunAsUser is the dedicated OS user model processes run as, through
	// sudo -n -u. Required: construction fails when it is unset, root or the
	// Box's own user.
	RunAsUser string
	Binary    string
	Args      []string
	Timeout   time.Duration
	// EnvAllowlist names the Box environment variables forwarded to the model
	// process (its own auth variables). Everything else is withheld, so an
	// agentic model cannot reuse GitHub or Box credentials.
	EnvAllowlist []string
	// CommandCodeModel selects the Command Code (cmd) backend with this model
	// id; the adapter then builds the whole argv. AllowPaid permits a model
	// that is not free; ZDR sets CMD_ZDR=1. All three are Command Code only.
	CommandCodeModel string
	AllowPaid        bool
	ZDR              bool
	// Profile selects a harness by name (see BuiltinProfiles and
	// LoadProfileDir); the adapter then builds the whole argv from the profile
	// and ProfileModel, like the Command Code backend, so Args must be empty.
	// ProfileDir holds the owner's extra profiles.
	Profile      string
	ProfileModel string
	ProfileDir   string
	// FallbackOf names the pool this reviewer stands in for when it is a
	// replacement reviewer; empty for the primary reviewer and for producers.
	FallbackOf string
}

type Adapter struct {
	runAs     *user.User
	binary    string
	args      []string
	timeout   time.Duration
	allowlist []string
	zdr       bool
	// commandCode: parse cmd's NDJSON and give it a private scratch cwd, so
	// one call cannot leave project config for the next (the reviewer's).
	commandCode bool
	// profile, when set, drives argv, prompt delivery and answer extraction.
	profile *Profile
	// ledger, when set (see WithLedger), records each call and gates the pool
	// the model draws on. harness, model and pool name that pool.
	ledger  capacity.Ledger
	harness string
	model   string
	pool    string
	stage   string
	caseID  string
}

// usage is the token count one call reported; nil when the CLI did not report it.
type usage struct{ input, output *int }

// unknownReset is how long a refusal blocks its pool when the provider prints
// no reset time.
const unknownReset = time.Hour

// WithLedger returns a copy of the adapter that records each call in ledger
// under stage and caseID, and refuses calls to a pool the ledger shows exhausted.
func (a *Adapter) WithLedger(ledger capacity.Ledger, stage, caseID string) *Adapter {
	if a == nil {
		return nil
	}
	c := *a
	c.ledger, c.stage, c.caseID = ledger, stage, caseID
	return &c
}

// New validates the model configuration at construction, so an unusable
// configuration is a startup error rather than a per-tick error.
func New(config Config) (*Adapter, error) {
	if config.Binary == "" {
		return nil, errors.New("model binary is required")
	}
	if config.Timeout <= 0 {
		return nil, errors.New("model timeout must be positive")
	}
	target, err := ResolveRunAsUser(config.RunAsUser)
	if err != nil {
		return nil, err
	}
	if err := checkCommandCode(config); err != nil {
		return nil, err
	}
	if config.CommandCodeModel != "" {
		config.Args = commandCodeArgs(config.CommandCodeModel)
	}
	var profile *Profile
	if config.Profile != "" {
		p, args, err := resolveProfile(config)
		if err != nil {
			return nil, err
		}
		profile, config.Args = &p, args
	} else if config.ProfileModel != "" {
		return nil, errors.New("--model-name needs --model-profile")
	}
	resolved, err := exec.LookPath(config.Binary)
	if err != nil {
		return nil, fmt.Errorf("model binary %q is not usable: %w", config.Binary, err)
	}
	adapter := &Adapter{runAs: target, binary: resolved, args: config.Args, timeout: config.Timeout, allowlist: config.EnvAllowlist, zdr: config.ZDR, commandCode: config.CommandCodeModel != "", profile: profile}
	switch {
	case config.CommandCodeModel != "":
		adapter.harness, adapter.model, adapter.pool = "commandcode", config.CommandCodeModel, commandCodePool(config.CommandCodeModel)
	case profile != nil:
		adapter.harness, adapter.model, adapter.pool = profile.Name, config.ProfileModel, profile.PoolFor(config.ProfileModel)
	}
	return adapter, nil
}

// Timeout is the per-call model timeout.
func (a *Adapter) Timeout() time.Duration { return a.timeout }

const maxJSONRepairs = 2

func saveRejectedJSON(caseID, stage string, attempt int, raw []byte, parseErr error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	dir := filepath.Join(home, ".summa42", "logs", "rejected")
	_ = os.MkdirAll(dir, 0700)
	filename := fmt.Sprintf("%s_%s_att%d_%d.json", stage, caseID, attempt, time.Now().UnixNano())
	content := fmt.Sprintf("// Parse error: %v\n%s\n", parseErr, string(raw))
	_ = os.WriteFile(filepath.Join(dir, filename), []byte(content), 0600)
}

func buildJSONRepairPrompt(initialPrompt, previousRaw []byte, parseErr error, schemaGuidance string) []byte {
	var sb strings.Builder
	sb.WriteString("ATTENTION: Your previous response failed validation and could not be parsed as the required JSON schema.\n\n")
	sb.WriteString("PARSING / VALIDATION ERROR:\n")
	sb.WriteString(parseErr.Error())
	sb.WriteString("\n\n")
	if schemaGuidance != "" {
		sb.WriteString("REQUIRED SCHEMA & CONSTRAINTS:\n")
		sb.WriteString(schemaGuidance)
		sb.WriteString("\n\n")
	}
	sb.WriteString("YOUR PREVIOUS REJECTED OUTPUT:\n")
	if len(previousRaw) > 24000 {
		sb.WriteString(string(previousRaw[:24000]))
		sb.WriteString("\n...[output truncated due to length]...\n")
	} else {
		sb.WriteString(string(previousRaw))
		sb.WriteString("\n")
	}
	sb.WriteString("\nORIGINAL INPUT / CONTEXT:\n")
	if len(initialPrompt) > 32000 {
		sb.WriteString(string(initialPrompt[:32000]))
		sb.WriteString("\n...[input truncated due to length]...\n")
	} else {
		sb.WriteString(string(initialPrompt))
		sb.WriteString("\n")
	}
	sb.WriteString("\nTASK:\n")
	sb.WriteString("Repair the rejected output so that it fixes the parsing/validation error and strictly complies with the schema requirements.\n")
	sb.WriteString("Do not include any commentary, prose, markdown fences, or extra unknown fields.\n")
	sb.WriteString("Respond with ONLY the valid single JSON object.\n")
	return []byte(sb.String())
}

func runWithJSONRepair[T any](
	ctx context.Context,
	a *Adapter,
	initialPrompt []byte,
	schemaGuidance string,
	parse func([]byte) (T, error),
) (T, error) {
	prompt := initialPrompt
	var zero T
	var lastErr error
	for attempt := 0; attempt <= maxJSONRepairs; attempt++ {
		raw, err := a.run(ctx, prompt)
		if err != nil {
			return zero, err
		}
		val, parseErr := parse(raw)
		if parseErr == nil {
			return val, nil
		}
		lastErr = parseErr
		if a != nil {
			saveRejectedJSON(a.caseID, a.stage, attempt+1, raw, parseErr)
		}
		if attempt == maxJSONRepairs {
			break
		}
		log.Printf("model output failed JSON schema parsing (attempt %d/%d): %v; requesting repair", attempt+1, maxJSONRepairs+1, parseErr)
		prompt = buildJSONRepairPrompt(initialPrompt, raw, parseErr, schemaGuidance)
	}
	return zero, lastErr
}

func (a *Adapter) Classify(ctx context.Context, input ghtriage.Stage2Input) (ghtriage.Stage2Output, error) {
	prompt, err := json.Marshal(input)
	if err != nil {
		return ghtriage.Stage2Output{}, fmt.Errorf("encode stage 2 input: %w", err)
	}
	return runWithJSONRepair(ctx, a, prompt, `Single JSON object with: "is_actionable" (bool), "needs_repro" (bool), "scope" ("small"|"medium"|"large"|"huge"|"unknown"), "suggested_type" ("bug"|"feature"|"question"|"other"), "rationale" (non-empty string). No other fields.`, ghtriage.ParseStage2Output)
}

func (a *Adapter) Review(ctx context.Context, input ghtriage.ReviewInput) (bool, error) {
	prompt, err := json.Marshal(input)
	if err != nil {
		return false, fmt.Errorf("encode review input: %w", err)
	}
	return runWithJSONRepair(ctx, a, prompt, `Single JSON object with: "plausible" (bool). No other fields.`, ghtriage.ParseReviewVerdict)
}

func (a *Adapter) Plan(ctx context.Context, input ghtriage.PlanInput) (ghtriage.PlanOutput, error) {
	prompt, err := json.Marshal(input)
	if err != nil {
		return ghtriage.PlanOutput{}, fmt.Errorf("encode plan input: %w", err)
	}
	return runWithJSONRepair(ctx, a, prompt, `Single JSON object with EITHER {"read_repo_file": ["path1", ...]} (at most 8 paths) OR {"summary": "...", "steps": [{"objective": "...", "done_when": "..."}], "allowed_paths": [...]}. No other fields.`, ghtriage.ParsePlanOutput)
}

func (a *Adapter) SelectFiles(ctx context.Context, input ghtriage.FileSelectionInput) (ghtriage.FileSelection, error) {
	prompt, err := json.Marshal(input)
	if err != nil {
		return ghtriage.FileSelection{}, err
	}
	return runWithJSONRepair(ctx, a, prompt, `Single JSON object with: "context_files" (array of at most 32 strings from tracked_files), "allowed_paths" (array of 1 to 64 strings under allowed_prefixes). No other fields.`, ghtriage.ParseFileSelection)
}

func (a *Adapter) ReviewPlan(ctx context.Context, input ghtriage.PlanReviewInput) (ghtriage.PlanReviewOutput, error) {
	prompt, err := json.Marshal(input)
	if err != nil {
		return ghtriage.PlanReviewOutput{}, err
	}
	return runWithJSONRepair(ctx, a, prompt, `Single JSON object with: "verdict" ("ACCEPT"|"REVISE"|"BLOCK"), "reason" (string, 1-1000 chars). No other fields.`, ghtriage.ParsePlanReviewOutput)
}

func (a *Adapter) Implement(ctx context.Context, input ghtriage.ImplementationInput) (ghtriage.ImplementationOutput, error) {
	prompt, err := json.Marshal(input)
	if err != nil {
		return ghtriage.ImplementationOutput{}, err
	}
	return runWithJSONRepair(ctx, a, prompt, `Single JSON object with: "summary" (string, 1-1000 chars), "edits" (array of objects with "path" and file content changes). No other fields.`, ghtriage.ParseImplementationOutput)
}

func (a *Adapter) ReviewCode(ctx context.Context, input ghtriage.CodeReviewInput) (ghtriage.PlanReviewOutput, error) {
	prompt, err := json.Marshal(input)
	if err != nil {
		return ghtriage.PlanReviewOutput{}, err
	}
	return runWithJSONRepair(ctx, a, prompt, `Single JSON object with: "verdict" ("ACCEPT"|"REVISE"|"BLOCK"), "reason" (string, 1-1000 chars). No other fields.`, ghtriage.ParsePlanReviewOutput)
}

func (a *Adapter) run(ctx context.Context, prompt []byte) ([]byte, error) {
	if err := a.gate(ctx); err != nil {
		return nil, err
	}
	out, err := a.attempt(ctx, prompt)
	if a != nil && a.profile != nil && errors.Is(err, errEmptyAnswer) {
		log.Printf("model profile %s answered nothing; asking once more: %v", a.profile.Name, err)
		return a.attempt(ctx, slices.Concat(prompt, []byte(profileRetryNote)))
	}
	return out, err
}

// attempt is one model call, recorded in the ledger when one is attached.
func (a *Adapter) attempt(ctx context.Context, prompt []byte) ([]byte, error) {
	started := time.Now()
	var use usage
	out, err := a.runOnce(ctx, prompt, &use)
	a.record(ctx, started, use, err)
	return out, err
}

// gate refuses a call while the ledger shows its pool exhausted, so the
// provider is not asked again before the reset.
func (a *Adapter) gate(ctx context.Context) error {
	if a == nil || a.ledger == nil || a.pool == "" {
		return nil
	}
	state, err := a.ledger.PoolState(ctx, a.pool, time.Now())
	if err != nil {
		return fmt.Errorf("capacity ledger: %w", err)
	}
	if state.Unavailable() {
		return fmt.Errorf("pool %s unavailable until %s: %w", a.pool, state.ResetAt.UTC().Format(time.RFC3339), ghtriage.ErrModelUnavailable)
	}
	return nil
}

// record appends one call to the ledger. A refusal also says when its pool
// resets: the time the provider printed, or unknownReset when it printed none.
func (a *Adapter) record(ctx context.Context, started time.Time, use usage, err error) {
	if a == nil || a.ledger == nil || a.pool == "" {
		return
	}
	now := time.Now()
	call := capacity.Call{Pool: a.pool, Harness: a.harness, Model: a.model, Stage: a.stage, CaseID: a.caseID,
		StartedAt: started, Duration: now.Sub(started), Outcome: outcomeOf(err), InputTokens: use.input, OutputTokens: use.output}
	if err != nil {
		call.Message = err.Error()
	}
	if call.Outcome == capacity.OutcomeRefused {
		if reset, ok := capacity.ParseReset(call.Message, now); ok {
			call.ResetAt, call.ResetSource = reset, "parsed"
		} else {
			call.ResetAt, call.ResetSource = now.Add(unknownReset), "default"
		}
	}
	if rerr := a.ledger.RecordCall(context.WithoutCancel(ctx), call); rerr != nil {
		log.Printf("capacity ledger: %v", rerr)
	}
}

func outcomeOf(err error) capacity.Outcome {
	switch {
	case err == nil:
		return capacity.OutcomeOK
	case errors.Is(err, errEmptyAnswer):
		return capacity.OutcomeEmpty
	case errors.Is(err, context.DeadlineExceeded):
		return capacity.OutcomeTimeout
	case errors.Is(err, ghtriage.ErrModelUnavailable):
		return capacity.OutcomeRefused
	default:
		return capacity.OutcomeError
	}
}

func (a *Adapter) runOnce(ctx context.Context, prompt []byte, use *usage) ([]byte, error) {
	if a == nil {
		return nil, errors.New("model adapter is not configured")
	}
	runCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	var stdout, stderr cappedBuffer
	args := a.args
	if a.commandCode {
		prompt = append([]byte(commandCodePreamble), prompt...)
	}
	if a.profile != nil {
		var extra []string
		var err error
		if prompt, extra, err = a.profile.encodePrompt(slices.Concat([]byte(profilePreamble), prompt, []byte(profilePostamble))); err != nil {
			return nil, err
		}
		args = slices.Concat(a.args, extra)
	}
	cmd, err := modelrun.Command(runCtx, modelrun.Spec{
		RunAs: a.runAs, Binary: a.binary, Args: args, Allowlist: a.allowlist, Timeout: a.timeout, Env: a.env(),
		Scratch: a.commandCode || a.profile != nil,
	})
	if err != nil {
		return nil, err
	}
	cmd.Stdin = bytes.NewReader(prompt)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Checked before the process error is wrapped, so a caller can tell a
		// model that hung from one that crashed: cancelling the subtree kills
		// it, and the wrapped signal-kill is indistinguishable from a crash by
		// eye. errors.Is against DeadlineExceeded is the only handle on it, so
		// the deadline is wrapped rather than returned bare.
		//
		// Either way the captured stderr rides along. A provider reporting a
		// rate limit or an auth failure and then stalling is exactly the case
		// worth diagnosing, and a bare deadline exceeded leaves no trace of it:
		// before this branch existed that text reached the caller, so dropping
		// it here would make a timeout quieter than a plain crash.
		if runCtx.Err() != nil {
			return nil, fmt.Errorf("model invocation timed out: %w: %s", runCtx.Err(), stderr.String())
		}
		if providerRefused(stderr.String() + stdout.String()) {
			return nil, fmt.Errorf("model invocation failed: %w: %w: %s", ghtriage.ErrModelUnavailable, err, stderr.String())
		}
		return nil, fmt.Errorf("model invocation failed: %w: %s", err, stderr.String())
	}
	if a.commandCode {
		answer, err := commandCodeAnswer(stdout.Bytes())
		use.input, use.output = commandCodeTokens(stdout.Bytes())
		return unfence(answer), err
	}
	if a.profile != nil {
		answer, err := a.profile.answer(stdout.Bytes())
		return extractJSONObject(unfence(answer)), err
	}
	return unfence(stdout.Bytes()), nil
}

// extractJSONObject returns the one JSON object in an answer that a sentence
// precedes. Anything that is not exactly one object is returned unchanged, so
// the strict parser still refuses it.
func extractJSONObject(answer []byte) []byte {
	trimmed := bytes.TrimSpace(answer)
	start := bytes.IndexByte(trimmed, '{')
	end := bytes.LastIndexByte(trimmed, '}')
	if start <= 0 || end <= start {
		return answer
	}
	candidate := trimmed[start : end+1]
	var obj map[string]any
	if json.Unmarshal(candidate, &obj) != nil {
		return answer
	}
	return candidate
}

// unfence returns the body of an answer that is exactly one Markdown code
// block (```json ... ```), which models add despite being told not to. The
// body still goes through the strict parser; anything else is returned as is.
func unfence(answer []byte) []byte {
	trimmed := bytes.TrimSpace(answer)
	if !bytes.HasPrefix(trimmed, []byte("```")) || !bytes.HasSuffix(trimmed, []byte("```")) || len(trimmed) < 6 {
		return answer
	}
	body := trimmed[3 : len(trimmed)-3]
	newline := bytes.IndexByte(body, '\n')
	if newline < 0 || bytes.Contains(body, []byte("```")) {
		return answer
	}
	if info := bytes.TrimSpace(body[:newline]); len(info) > 0 && !bytes.Equal(bytes.ToLower(info), []byte("json")) {
		return answer
	}
	return bytes.TrimSpace(body[newline+1:])
}

// maxModelOutput bounds what the Box buffers from a model process, so a model
// cannot exhaust the Box's memory through stdout or stderr.
const maxModelOutput = 64 << 20

// cappedBuffer fails the write past maxModelOutput, which ends the model run.
type cappedBuffer struct{ bytes.Buffer }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > maxModelOutput {
		return 0, fmt.Errorf("model output exceeds %d bytes", maxModelOutput)
	}
	return b.Buffer.Write(p)
}

func (a *Adapter) env() map[string]string {
	if a.zdr {
		return map[string]string{"CMD_ZDR": "1"}
	}
	return nil
}

// providerRefused reports a quota or rate-limit refusal, or a model the
// provider has withdrawn, in model CLI output.
// ponytail: substring list of the messages seen from Command Code and Codex;
// extend it when another CLI words its limit differently.
func providerRefused(output string) bool {
	lower := strings.ToLower(output)
	for _, marker := range []string{"reached today's limit", "rate limit", "usage limit", "quota exceeded", "quota reached", "resource_exhausted", "no longer available", "429", "too many requests"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// resolveProfile loads and checks the configured profile and renders its argv.
func resolveProfile(config Config) (Profile, []string, error) {
	if config.CommandCodeModel != "" || len(config.Args) > 0 || config.ZDR || config.AllowPaid {
		return Profile{}, nil, errors.New("--model-profile builds its own arguments; --model-arg, --model-commandcode, --model-zdr and --model-allow-paid are not accepted with it")
	}
	p, err := FindProfile(config.Profile, config.ProfileDir)
	if err != nil {
		return Profile{}, nil, err
	}
	if !slices.Contains(p.Binaries, filepath.Base(config.Binary)) {
		return Profile{}, nil, fmt.Errorf("profile %s runs %v, not %q", p.Name, p.Binaries, config.Binary)
	}
	args, err := p.argv(config.ProfileModel)
	return p, args, err
}

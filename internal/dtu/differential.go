package dtu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"time"

	"github.com/SofiaFlux/summa42/internal/replay"
)

var (
	// ErrEmptyRequests is returned when an empty slice of requests is passed to Compare.
	ErrEmptyRequests = errors.New("cannot perform differential comparison on empty request set")

	// ErrNilProvider is returned when either the live or twin provider is nil.
	ErrNilProvider = errors.New("both live and twin providers must be specified")
)

// DifferentialOptions configures comparison settings for the differential test harness.
type DifferentialOptions struct {
	KnownDivergenceActions map[string]string `json:"known_divergence_actions,omitempty"`
	ExpectErrors           bool              `json:"expect_errors,omitempty"`
}

// DivergenceRecord details a single divergence between live and twin execution.
type DivergenceRecord struct {
	Action         string `json:"action"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	DivergenceType string `json:"divergence_type"`
	LiveStatus     int    `json:"live_status"`
	TwinStatus     int    `json:"twin_status"`
	LiveSuccess    bool   `json:"live_success"`
	TwinSuccess    bool   `json:"twin_success"`
	Known          bool   `json:"known"`
	KnownReason    string `json:"known_reason,omitempty"`
	Details        string `json:"details"`
}

// DivergenceReport encapsulates the complete differential evaluation between two providers.
type DivergenceReport struct {
	ReportID              string             `json:"report_id"`
	LiveProviderID        string             `json:"live_provider_id"`
	TwinProviderID        string             `json:"twin_provider_id"`
	TotalInvocations      int                `json:"total_invocations"`
	Matches               int                `json:"matches"`
	KnownDivergences      int                `json:"known_divergences"`
	UnexpectedDivergences int                `json:"unexpected_divergences"`
	Divergences           []DivergenceRecord `json:"divergences,omitempty"`
	Timestamp             time.Time          `json:"timestamp"`
	Conforms              bool               `json:"conforms"`
}

// DifferentialHarness runs comparative differential invocations between live and twin providers.
type DifferentialHarness struct {
	liveProv EnvironmentProvider
	twinProv EnvironmentProvider
}

// NewDifferentialHarness creates a new DifferentialHarness.
func NewDifferentialHarness(live, twin EnvironmentProvider) *DifferentialHarness {
	return &DifferentialHarness{
		liveProv: live,
		twinProv: twin,
	}
}

// Compare executes a sequence of InvocationRequests against both providers and compares outcomes.
func (h *DifferentialHarness) Compare(ctx context.Context, requests []InvocationRequest, opts DifferentialOptions) (*DivergenceReport, error) {
	if h.liveProv == nil || h.twinProv == nil {
		return nil, ErrNilProvider
	}
	if len(requests) == 0 {
		return nil, ErrEmptyRequests
	}

	liveMeta := h.liveProv.Metadata()
	twinMeta := h.twinProv.Metadata()

	now := time.Now().UTC()
	report := &DivergenceReport{
		ReportID:         fmt.Sprintf("diff_%s_%s_%d", liveMeta.ProviderID, twinMeta.ProviderID, now.UnixNano()),
		LiveProviderID:   liveMeta.ProviderID,
		TwinProviderID:   twinMeta.ProviderID,
		TotalInvocations: len(requests),
		Timestamp:        now,
		Divergences:      make([]DivergenceRecord, 0),
	}

	for _, req := range requests {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		liveResp, liveErr := h.liveProv.Execute(ctx, req)
		twinResp, twinErr := h.twinProv.Execute(ctx, req)

		var div *DivergenceRecord

		// 1. Evaluate error conditions
		if liveErr != nil || twinErr != nil {
			if liveErr != nil && twinErr != nil {
				if liveErr.Error() != twinErr.Error() {
					div = &DivergenceRecord{
						Action:         req.Action,
						IdempotencyKey: req.IdempotencyKey,
						DivergenceType: "ERROR_MESSAGE_MISMATCH",
						LiveSuccess:    false,
						TwinSuccess:    false,
						LiveStatus:     liveResp.StatusCode,
						TwinStatus:     twinResp.StatusCode,
						Details:        fmt.Sprintf("liveErr=%s, twinErr=%s", liveErr.Error(), twinErr.Error()),
					}
				} else if !opts.ExpectErrors {
					// Both encountered identical errors, but errors were not explicitly expected
					div = &DivergenceRecord{
						Action:         req.Action,
						IdempotencyKey: req.IdempotencyKey,
						DivergenceType: "UNEXPECTED_DUAL_ERROR",
						LiveSuccess:    false,
						TwinSuccess:    false,
						LiveStatus:     liveResp.StatusCode,
						TwinStatus:     twinResp.StatusCode,
						Details:        fmt.Sprintf("both providers failed with: %s", liveErr.Error()),
					}
				}
			} else {
				// One succeeded, one returned an error
				div = &DivergenceRecord{
					Action:         req.Action,
					IdempotencyKey: req.IdempotencyKey,
					DivergenceType: "ERROR_DISPARITY",
					LiveSuccess:    liveResp.Success,
					TwinSuccess:    twinResp.Success,
					LiveStatus:     liveResp.StatusCode,
					TwinStatus:     twinResp.StatusCode,
					Details:        fmt.Sprintf("liveErr=%v, twinErr=%v", liveErr, twinErr),
				}
			}
		} else if liveResp.Success != twinResp.Success {
			div = &DivergenceRecord{
				Action:         req.Action,
				IdempotencyKey: req.IdempotencyKey,
				DivergenceType: "SUCCESS_MISMATCH",
				LiveSuccess:    liveResp.Success,
				TwinSuccess:    twinResp.Success,
				LiveStatus:     liveResp.StatusCode,
				TwinStatus:     twinResp.StatusCode,
				Details:        fmt.Sprintf("liveSuccess=%v (err=%s), twinSuccess=%v (err=%s)", liveResp.Success, liveResp.Error, twinResp.Success, twinResp.Error),
			}
		} else if liveResp.StatusCode != twinResp.StatusCode {
			div = &DivergenceRecord{
				Action:         req.Action,
				IdempotencyKey: req.IdempotencyKey,
				DivergenceType: "STATUS_CODE_MISMATCH",
				LiveSuccess:    liveResp.Success,
				TwinSuccess:    twinResp.Success,
				LiveStatus:     liveResp.StatusCode,
				TwinStatus:     twinResp.StatusCode,
				Details:        fmt.Sprintf("liveStatusCode=%d, twinStatusCode=%d", liveResp.StatusCode, twinResp.StatusCode),
			}
		} else if !semanticDataEqual(liveResp.Data, twinResp.Data) {
			div = &DivergenceRecord{
				Action:         req.Action,
				IdempotencyKey: req.IdempotencyKey,
				DivergenceType: "PAYLOAD_SEMANTIC_DIFF",
				LiveSuccess:    liveResp.Success,
				TwinSuccess:    twinResp.Success,
				LiveStatus:     liveResp.StatusCode,
				TwinStatus:     twinResp.StatusCode,
				Details:        fmt.Sprintf("data diff: live=%v, twin=%v", liveResp.Data, twinResp.Data),
			}
		}

		if div == nil {
			report.Matches++
		} else {
			// Check if known divergence
			knownReason, isKnown := opts.KnownDivergenceActions[req.Action]
			if isKnown {
				div.Known = true
				div.KnownReason = knownReason
				report.KnownDivergences++
			} else {
				report.UnexpectedDivergences++
			}
			report.Divergences = append(report.Divergences, *div)
		}
	}

	report.Conforms = (report.UnexpectedDivergences == 0 && report.TotalInvocations > 0)
	return report, nil
}

var tokenSanitizerPattern = regexp.MustCompile(`(?i)(token|bearer|secret|key|password)([=:\s]+(?:bearer\s+)?)[^\s,;]+`)

func sanitizeDetails(details string) string {
	return tokenSanitizerPattern.ReplaceAllString(details, "${1}=[REDACTED]")
}

// ExportReplayFixture converts a divergence finding into a validated regression fixture for replay harness.
func (r *DivergenceReport) ExportReplayFixture(div DivergenceRecord, taskClass string) (*replay.Fixture, error) {
	if taskClass == "" {
		taskClass = "differential_regression"
	}

	sanitizedDetails := sanitizeDetails(div.Details)
	hashContent := fmt.Sprintf("%s:%s:%s:%s:%s", r.LiveProviderID, r.TwinProviderID, div.Action, div.DivergenceType, sanitizedDetails)
	hash := sha256.Sum256([]byte(hashContent))
	hashHex := hex.EncodeToString(hash[:8])

	fixtureID := fmt.Sprintf("fix_diff_%s_%s_%s", div.Action, div.DivergenceType, hashHex)

	fixture := &replay.Fixture{
		Version:   replay.FixtureVersion,
		FixtureID: fixtureID,
		Task: replay.TaskSemantics{
			Objective:            fmt.Sprintf("Differential regression test for action %s [%s]: %s", div.Action, div.DivergenceType, sanitizedDetails),
			TaskClass:            taskClass,
			RequiredCapabilities: []string{"provider.invoke", "provider.reconcile"},
			AuthorityCeiling:     []string{"provider.invoke", "provider.reconcile"},
			AcceptanceCriteria:   []string{"Behavior matches baseline expectation without unhandled divergence"},
		},
		Execution: replay.ExecutionMetadata{
			ExecutorKind: "simulated_standard",
			TEBProfile:   "sandboxed",
		},
		Invariants: replay.ExpectedInvariants{
			RequireAuthorityCeilingHeld: true,
			RequireZeroAmbientNetwork:   true,
			RequireAuditTamperEvident:   true,
		},
		CostEnvelope: replay.CostEnvelope{
			MaxTokens:   5000,
			MaxExposure: 0,
		},
		Sanitization: replay.SanitizationProvenance{
			SanitizerVersion: replay.SanitizerVersion,
			PassedChecks:     []string{"REDACT_SECRETS_DIFF"},
		},
	}

	return fixture, nil
}

func semanticDataEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok {
			return false
		}
		if !reflect.DeepEqual(va, vb) {
			return false
		}
	}
	return true
}

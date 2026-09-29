package ghtriage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/runmanifest"
	"github.com/SofiaFlux/summa42/internal/verification"
	"github.com/SofiaFlux/summa42/internal/workflow"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

const (
	DriverVerifierID   = domain.ID("gh-triage-driver")
	DriverVerifierType = "DETERMINISTIC"
	SourceGitHub       = "github"

	ReasonTriageFailed   = "triage-failed"
	supersededReasonHead = "superseded-by:"

	// acceptedIndexScanLimit is the ceiling on the walk findAccepted makes over
	// the records of KindAccepted, and it is set so that the walk covers every
	// record of the kind rather than a working number of them.
	//
	// The store has no case column, so a case's own record can only be told
	// from another case's by decoding it, and the walk has to reach the record
	// to do that. The kind is never pruned and every acceptance writes a record
	// carrying a fresh decision_evidence_id, so no two records are alike and the
	// count grows by one per accepted triage for the life of the store. A
	// ceiling below that count does not bound the walk, it decides which cases
	// can be recovered at all: a case whose record fell outside it was skipped
	// silently, leaving a SUCCEEDED task on an ACTIVE case that no later tick
	// could move, reported as a healthy tick.
	//
	// What the ceiling now protects against is a bound the walk cannot reach:
	// the kind index (kind, created_at, evidence_id) makes the whole walk a
	// range scan of one kind, so what it costs is the number of accepted triage
	// records in the store and not the size of the store. What it no longer
	// protects against is a store whose accepted-index kind grows without
	// bound - a pruning or an index keyed by case is what that would need, and
	// neither exists.
	acceptedIndexScanLimit = math.MaxInt64
)

type DriverResult struct {
	Accepted   int
	Assessed   int
	Blocked    int
	Superseded int
	Failures   []string
}

type Driver struct {
	cases        *workflowcase.Service
	execution    *execution.Service
	verification *verification.Service
	manifests    *runmanifest.Service
	evidence     *evidence.Store
}

// NewDriver wires the services the driver reads and writes. It takes no clock:
// every timestamp the driver causes is stamped by the service that owns the row
// - the decision was written by the worker, the assessment is stamped by the
// case service - so the driver has no time of its own to keep.
func NewDriver(cases *workflowcase.Service, exec *execution.Service, ver *verification.Service, manifests *runmanifest.Service, store *evidence.Store, _ clock.Clock) *Driver {
	return &Driver{
		cases: cases, execution: exec, verification: ver,
		manifests: manifests, evidence: store,
	}
}

type acceptedRevision struct {
	c          workflowcase.Case
	revision   time.Time
	evidenceID domain.ID
}

// Tick advances every GitHub case of the mission. It runs after the lease has
// completed, never inside the executor, so the case never moves before the
// successful attempt is recorded.
func (d *Driver) Tick(ctx context.Context, missionID domain.ID) (DriverResult, error) {
	if d == nil || d.cases == nil || d.execution == nil || d.verification == nil {
		return DriverResult{}, errors.New("triage driver is not configured")
	}
	var result DriverResult
	active, err := d.cases.ListActive(ctx, missionID)
	if err != nil {
		return result, err
	}
	objects := make([]string, 0, len(active))
	seen := make(map[string]struct{}, len(active))
	for _, c := range active {
		if c.Source != SourceGitHub {
			continue
		}
		if _, known := seen[c.ObjectID]; known {
			continue
		}
		seen[c.ObjectID] = struct{}{}
		objects = append(objects, c.ObjectID)
	}
	sort.Strings(objects)

	for _, object := range objects {
		if err := d.tickObject(ctx, missionID, object, &result); err != nil {
			result.Failures = append(result.Failures, fmt.Sprintf("%s: %v", object, err))
		}
	}
	return result, nil
}

func (d *Driver) tickObject(ctx context.Context, missionID domain.ID, object string, result *DriverResult) error {
	cases, err := d.cases.ListByObject(ctx, missionID, SourceGitHub, object)
	if err != nil {
		return err
	}
	accepted, err := d.advanceAccepted(ctx, cases, result)
	if err != nil {
		return err
	}
	if len(accepted) == 0 {
		return nil
	}
	newest := accepted[0]
	for _, older := range accepted[1:] {
		if !older.revision.Before(newest.revision) {
			continue
		}
		if err := d.supersede(ctx, older, newest, result); err != nil {
			return err
		}
	}
	return nil
}

// advanceAccepted accepts the task of every completed revision and applies its
// disposition. It returns every ACTIVE revision of the object, newest first,
// because a revision whose triage task has not finished is still superseded by
// a newer one: its pending task is exactly what supersession has to challenge.
func (d *Driver) advanceAccepted(ctx context.Context, cases []workflowcase.Case, result *DriverResult) ([]acceptedRevision, error) {
	accepted := make([]acceptedRevision, 0, len(cases))
	for _, c := range cases {
		if c.CurrentWorkID == "" || c.State != workflowcase.Active {
			continue
		}
		task, found, err := d.execution.FindByIdempotencyKey(ctx, string(c.CurrentWorkID))
		if err != nil {
			return accepted, err
		}
		if !found {
			continue
		}
		switch task.State {
		case domain.TaskAwaitingVerification:
			decision, evidenceID, err := d.acceptDecision(ctx, c, task, result)
			if err != nil {
				return accepted, err
			}
			if decision == nil {
				continue
			}
			current, err := d.cases.Get(ctx, c.ID)
			if err != nil {
				return accepted, err
			}
			if err := d.applyDisposition(ctx, current, current.CurrentWorkID, *decision, evidenceID, result); err != nil {
				return accepted, err
			}
			accepted = append(accepted, acceptedRevision{
				c: current, revision: parseRevision(current.RevisionID), evidenceID: evidenceID,
			})
		case domain.TaskSucceeded:
			decision, evidenceID, err := d.recoverAccepted(ctx, c, task)
			if err != nil {
				return accepted, err
			}
			if decision == nil {
				continue
			}
			if err := d.applyDisposition(ctx, c, c.CurrentWorkID, *decision, evidenceID, result); err != nil {
				return accepted, err
			}
			accepted = append(accepted, acceptedRevision{
				c: c, revision: parseRevision(c.RevisionID), evidenceID: evidenceID,
			})
		case domain.TaskBlocked:
			if err := d.blockFailedTask(ctx, c, task, result); err != nil {
				return accepted, err
			}
		default:
			// A revision whose triage has not finished is still superseded by
			// a newer one, and it has no decision to carry yet. The evidence
			// that does exist for it is the snapshot intake stored: that
			// observation is what established the newer revision, so it is
			// what the supersession and the block below can be recorded
			// against. Without it the newer revision hands a blank evidence
			// ID to both, which workflow.Decide rejects, so the older case
			// is challenged, never blocked, and stays ACTIVE on every tick.
			accepted = append(accepted, acceptedRevision{
				c: c, revision: parseRevision(c.RevisionID),
				evidenceID: domain.ID(c.ObservationEvidenceID),
			})
		}
	}
	sort.SliceStable(accepted, func(i, j int) bool {
		return accepted[i].revision.After(accepted[j].revision)
	})
	return accepted, nil
}

// acceptDecision validates the single decision attached to the completed
// attempt, writes the restart index, and accepts the task. Evidence not
// attached to a completed attempt is not a decision, which is why this reads
// provenance rather than the evidence store directly.
func (d *Driver) acceptDecision(ctx context.Context, c workflowcase.Case, task domain.Task, result *DriverResult) (*Decision, domain.ID, error) {
	if task.CurrentAttemptID == "" {
		return nil, domain.ID(""), nil
	}
	provenance, err := d.manifests.Provenance(ctx, task.CurrentAttemptID)
	if err != nil {
		return nil, domain.ID(""), fmt.Errorf("read triage attempt provenance: %w", err)
	}
	decision, evidenceID, err := d.readDecision(ctx, provenance.OutputEvidence, c)
	if err != nil || decision == nil {
		return nil, domain.ID(""), err
	}
	if err := d.recordAccepted(ctx, c, evidenceID); err != nil {
		return nil, domain.ID(""), err
	}
	if _, err := d.verification.AcceptTask(ctx, task.ID, verification.AcceptanceRequest{
		VerifierID:   DriverVerifierID,
		VerifierType: DriverVerifierType,
		CriteriaMet:  true,
		EvidenceIDs:  []domain.ID{evidenceID},
	}); err != nil {
		return nil, domain.ID(""), fmt.Errorf("accept triage task %s: %w", task.ID, err)
	}
	result.Accepted++
	return decision, evidenceID, nil
}

// recoverAccepted replays a task the driver already accepted on an earlier
// tick. internal/verification has no read accessor for an acceptance record, so
// the driver's own accepted index is the replay path, and a case whose index is
// not there cannot be replayed at all: its task is already SUCCEEDED, so the
// driver will accept it a second time on every tick and never assess the case.
// That is reported rather than skipped, because a skipped case keeps a
// SUCCEEDED task on an ACTIVE case for good while every tick reports success.
func (d *Driver) recoverAccepted(ctx context.Context, c workflowcase.Case, task domain.Task) (*Decision, domain.ID, error) {
	raw, found, err := d.findAccepted(ctx, c, task)
	if err != nil {
		return nil, domain.ID(""), err
	}
	if !found {
		return nil, domain.ID(""), fmt.Errorf(
			"case %s revision %s has work %s %s with no accepted index to replay: %w",
			c.ID, c.RevisionID, c.CurrentWorkID, task.State, evidence.ErrEvidenceNotFound)
	}
	var record acceptedIndex
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, domain.ID(""), fmt.Errorf("decode accepted triage index: %w", err)
	}
	if record.Revision != c.RevisionID {
		return nil, domain.ID(""), fmt.Errorf(
			"accepted index revision %q does not match case revision %q", record.Revision, c.RevisionID)
	}
	object, decisionRaw, err := d.evidence.Get(ctx, record.DecisionEvidenceID)
	if err != nil {
		return nil, domain.ID(""), err
	}
	if object.Kind != KindDecision {
		return nil, domain.ID(""), fmt.Errorf("accepted index points at evidence of kind %q", object.Kind)
	}
	var decision Decision
	if err := json.Unmarshal(decisionRaw, &decision); err != nil {
		return nil, domain.ID(""), fmt.Errorf("decode accepted decision: %w", err)
	}
	return &decision, record.DecisionEvidenceID, nil
}

type acceptedIndex struct {
	Schema             string    `json:"schema"`
	CaseID             domain.ID `json:"case_id"`
	TaskID             domain.ID `json:"task_id"`
	Revision           string    `json:"revision"`
	DecisionEvidenceID domain.ID `json:"decision_evidence_id"`
}

// recordAccepted writes the driver's restart index before AcceptTask, so a
// crash between the two leaves a recoverable record. It carries no decision:
// the decision is already stored under its own ID, which is the field a replay
// reads. Content hashing dedupes the record across ticks.
func (d *Driver) recordAccepted(ctx context.Context, c workflowcase.Case, evidenceID domain.ID) error {
	record := acceptedIndex{
		Schema:             "github.issue.triage.accepted.v1",
		CaseID:             c.ID,
		TaskID:             c.CurrentWorkID,
		Revision:           c.RevisionID,
		DecisionEvidenceID: evidenceID,
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, _, err = d.putOnce(ctx, raw, KindAccepted)
	return err
}

// findAccepted locates the driver's own accepted index for a case. The evidence
// store has no case column and FindByContentHash needs a hash that is unknown
// before the record exists, so the lookup walks the records of the kind and
// checks the identity in each one: several cases of one mission each write a
// record of this kind, and only this case's record carries this case's
// identity, so a lookup that stopped at the first candidate it could decode
// would report "not found" for every case but one whenever another case wrote
// its record later. The walk is therefore over every record of the kind - see
// acceptedIndexScanLimit - because a candidate that is not this case's has to
// be walked past, not skipped over by a ceiling. The recorded task ID is the
// case's work ID, which is the task's idempotency key and therefore the identity
// recordAccepted wrote it under; the task row's own ID is a different value and
// would never match.
func (d *Driver) findAccepted(ctx context.Context, c workflowcase.Case, task domain.Task) ([]byte, bool, error) {
	if d.evidence == nil {
		return nil, false, errors.New("evidence store is not configured")
	}
	matches := func(_ evidence.EvidenceObject, raw []byte) bool {
		var record acceptedIndex
		if err := json.Unmarshal(raw, &record); err != nil {
			return false
		}
		return record.CaseID == c.ID && record.TaskID == domain.ID(task.IdempotencyKey)
	}
	_, raw, found, err := d.evidence.FindByKind(ctx, KindAccepted, acceptedIndexScanLimit, matches)
	if err != nil || !found {
		return nil, false, err
	}
	return raw, true, nil
}

func (d *Driver) readDecision(ctx context.Context, refs []runmanifest.EvidenceRef, c workflowcase.Case) (*Decision, domain.ID, error) {
	var found *Decision
	var foundID domain.ID
	for _, ref := range refs {
		object, raw, err := d.evidence.Get(ctx, ref.ID)
		if err != nil {
			return nil, domain.ID(""), err
		}
		if object.Kind != KindDecision {
			continue
		}
		if found != nil {
			return nil, domain.ID(""), fmt.Errorf("attempt carries more than one %s", KindDecision)
		}
		var decision Decision
		if err := json.Unmarshal(raw, &decision); err != nil {
			return nil, domain.ID(""), fmt.Errorf("decode decision %s: %w", ref.ID, err)
		}
		found = &decision
		foundID = ref.ID
	}
	if found == nil {
		return nil, domain.ID(""), nil
	}
	if err := found.Validate(); err != nil {
		return nil, domain.ID(""), fmt.Errorf("stored decision is invalid: %w", err)
	}
	if found.Revision != c.RevisionID {
		return nil, domain.ID(""), fmt.Errorf(
			"decision revision %q does not match case revision %q", found.Revision, c.RevisionID)
	}
	if found.SnapshotEvidenceID == "" {
		return nil, domain.ID(""), errors.New("decision carries no snapshot evidence id")
	}
	return found, foundID, nil
}

func (d *Driver) applyDisposition(ctx context.Context, c workflowcase.Case, taskID domain.ID, decision Decision, evidenceID domain.ID, result *DriverResult) error {
	disposition := decision.FinalDisposition()
	if disposition == DispositionReadyToPlan {
		return nil
	}
	blocked, err := d.block(ctx, c, taskID, string(disposition), evidenceID)
	if err != nil {
		return err
	}
	if blocked {
		result.Assessed++
	}
	return nil
}

// block moves an ACTIVE case to BLOCKED through Assess, which is idempotent by
// exact-request replay and already records the reason in the audit row. No new
// column on workflow_cases is required. It reports whether it moved the case, so
// a caller whose guard short-circuited does not count an assessment it did not
// perform.
func (d *Driver) block(ctx context.Context, c workflowcase.Case, taskID domain.ID, reason string, evidenceID domain.ID) (bool, error) {
	if c.State != workflowcase.Active || c.CurrentWorkID != taskID {
		return false, nil
	}
	// A blank evidence ID is refused here rather than handed to Assess:
	// workflow.Decide rejects one, and the rejection would repeat identically on
	// every tick with the case stuck ACTIVE and the task already changed.
	if strings.TrimSpace(string(evidenceID)) == "" {
		return false, fmt.Errorf("no evidence to record blocking case %s as %s", c.ID, reason)
	}
	_, err := d.cases.Assess(ctx, workflowcase.AssessmentRequest{
		CaseID: c.ID,
		WorkID: taskID,
		Assessment: workflow.Assessment{
			Verdict:     workflow.Unknown,
			Reason:      reason,
			EvidenceIDs: []string{string(evidenceID)},
		},
		RemainingBudget: c.RemainingBudget,
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// blockFailedTask handles a task that exhausted its retries and therefore has
// no decision. workflow.Decide requires non-empty evidence, so the failure
// document is what makes this transition possible at all, and it stops an
// untriaged case from looking like work to the future planner.
func (d *Driver) blockFailedTask(ctx context.Context, c workflowcase.Case, task domain.Task, result *DriverResult) error {
	raw, err := json.Marshal(map[string]any{
		"schema":  "github.issue.triage.failure.v1",
		"case_id": string(c.ID),
		"task_id": string(task.ID),
		"attempt": string(task.CurrentAttemptID),
		"state":   string(task.State),
		"reason":  ReasonTriageFailed,
	})
	if err != nil {
		return err
	}
	_, evidenceID, err := d.putOnce(ctx, raw, KindFailure)
	if err != nil {
		return err
	}
	blocked, err := d.block(ctx, c, c.CurrentWorkID, ReasonTriageFailed, evidenceID)
	if err != nil {
		return err
	}
	if blocked {
		result.Blocked++
	}
	return nil
}

// supersede stops an older revision's pending work and blocks its case. The
// challenge runs before the assessment, so a crash between them leaves an
// inert task and an ACTIVE case the next tick can finish. Every revision it
// closes records the same superseded-by reason, whether its triage had already
// produced a decision or had not run at all.
func (d *Driver) supersede(ctx context.Context, older, newest acceptedRevision, result *DriverResult) error {
	// A supersession is recorded against evidence: the challenge persists it and
	// the assessment cites it. A newer revision that has neither a decision nor
	// an observation has nothing to record, and both writes would either persist
	// a blank ID or be rejected by workflow.Decide, leaving the older case
	// ACTIVE for good. Refusing before the challenge leaves the older task
	// running and says why.
	if strings.TrimSpace(string(newest.evidenceID)) == "" {
		return fmt.Errorf("revision %s supersedes %s with no evidence to record",
			newest.c.RevisionID, older.c.RevisionID)
	}
	reason := supersededReasonHead + newest.c.RevisionID
	taskID := older.c.CurrentWorkID
	task, found, err := d.execution.FindByIdempotencyKey(ctx, string(taskID))
	if err != nil {
		return err
	}
	if found {
		switch task.State {
		case domain.TaskEligible, domain.TaskExecuting:
			if _, err := d.execution.ChallengeTaskIfInStates(ctx, task.ID,
				[]domain.TaskState{domain.TaskEligible, domain.TaskExecuting},
				domain.ChallengeTask, reason, []domain.ID{newest.evidenceID}); err != nil {
				return err
			}
		case domain.TaskAwaitingVerification:
			// The older revision's triage did run, so its task is accepted on
			// the decision it produced; only the case is left to the caller
			// below. Applying that decision's disposition here instead would
			// record the disposition as the reason for closing the case, so the
			// audit row would say not-actionable on one path and
			// superseded-by on the other for the same fact.
			if _, _, err := d.acceptDecision(ctx, older.c, task, result); err != nil {
				return err
			}
		}
	}
	current, err := d.cases.Get(ctx, older.c.ID)
	if err != nil {
		return err
	}
	if current.State != workflowcase.Active {
		return nil
	}
	blocked, err := d.block(ctx, current, current.CurrentWorkID, reason, newest.evidenceID)
	if err != nil {
		return err
	}
	if !blocked {
		return nil
	}
	result.Superseded++
	return nil
}

// putOnce stores a document and returns its evidence ID, reusing an existing
// object when the identical bytes are already stored. The lookup is by content
// hash of these bytes, which are a deterministic function of their inputs.
func (d *Driver) putOnce(ctx context.Context, raw []byte, kind string) (evidence.EvidenceObject, domain.ID, error) {
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	if existing, found, err := d.evidence.FindByContentHash(ctx, hash, kind); err != nil {
		return evidence.EvidenceObject{}, domain.ID(""), err
	} else if found {
		return existing, existing.ID, nil
	}
	object, err := d.evidence.Put(ctx, strings.NewReader(string(raw)), evidence.Metadata{
		MediaType: DecisionMediaType,
		Kind:      kind,
	})
	if err != nil {
		return evidence.EvidenceObject{}, domain.ID(""), err
	}
	return object, object.ID, nil
}

// parseRevision turns a revision into the time it names. This is defensive, not
// load-bearing: intake writes every revision as
// UpdatedAt.UTC().Format(time.RFC3339Nano) (internal/ghissue/source.go), and for
// two strings of that one shape lexicographic order provably equals
// chronological order, so comparing the strings would give the same answer for
// every revision the system produces. It is parsed because a revision is a
// timestamp, and a caller that supplies a differently shaped one - an offset, a
// fractional second - would otherwise be ordered by a byte rather than by a
// moment. An unparseable revision is the zero time and therefore the oldest,
// which leaves a broken revision to be superseded rather than to supersede.
func parseRevision(revision string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(revision))
	if err != nil {
		return time.Time{}
	}
	return parsed
}

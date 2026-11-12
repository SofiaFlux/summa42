// Package notify tells the Owner that something needs them: an approval
// waits, a case is held, a pull request is ready, the Collective is frozen or
// in safe mode, a budget is nearly used (development plan, card M3-T3).
//
// Two rules shape it.
//
// A message carries identifiers and words from a closed list only: never
// repository content and never text a model wrote. A reason written by a
// model, sent to a webhook, would be a way out of the Collective that a model
// controls. Message is the only place text is assembled, and it takes its
// words from this file.
//
// An event is delivered at most once. Events are queued under a key that names
// what happened. A row is marked SENDING before the message leaves; if the
// outcome of the send is not known (a timeout after the request went out, a
// crash), the row becomes UNKNOWN and is not sent again. Only a send that
// definitely did not arrive is retried.
package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

// Kinds of event, the closed list.
const (
	KindApprovalWaiting = "approval-waiting"
	KindCaseBlocked     = "case-blocked"
	KindCaseReady       = "case-ready"
	KindFrozen          = "frozen"
	KindSafeMode        = "safe-mode"
	KindSafeModeSoon    = "safe-mode-soon"
	KindBudgetLow       = "budget-low"
	KindContained       = "attempt-contained"
)

// SafeModeWarning is how long before the Owner's horizon the warning fires.
const SafeModeWarning = 24 * time.Hour

// BudgetLowPercent is the share of an envelope that must be used for the
// budget warning.
const BudgetLowPercent = 80

// Event is something the Owner should know. Only identifiers and codes.
type Event struct {
	Key  string // names what happened; an event with the same key is queued once
	Kind string
	// Identifiers, as recorded by the Box or given by the source system (a
	// case ID, an approval ID, "owner/name#123"). Never model text.
	Subject string
	Object  string
	// Code is a word from blockCodes for a held case; a count or a time for
	// the others, formatted by this package.
	Code string
}

// identifierText keeps an identifier to characters identifiers are made of,
// so even a hostile object ID cannot carry a sentence or a mention.
func identifierText(value string) string {
	var out strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', strings.ContainsRune("_-./#:", r):
			out.WriteRune(r)
		}
		if out.Len() >= 120 {
			break
		}
	}
	return out.String()
}

// Message is the text sent for an event.
func (e Event) Message() string {
	subject, object := identifierText(e.Subject), identifierText(e.Object)
	switch e.Kind {
	case KindApprovalWaiting:
		return fmt.Sprintf("Summa42: approval %s is waiting (%s). Run: summa42 approvals", subject, object)
	case KindCaseBlocked:
		return fmt.Sprintf("Summa42: case %s for %s is held (%s). Run: summa42 case report %s", subject, object, identifierText(e.Code), subject)
	case KindCaseReady:
		return fmt.Sprintf("Summa42: case %s for %s has a draft pull request ready for your review.", subject, object)
	case KindFrozen:
		return "Summa42: break glass is FROZEN. No work starts until: summa42 emergency resume"
	case KindSafeMode:
		return "Summa42: the dead man switch is in SAFE MODE. New work waits for: summa42 emergency heartbeat"
	case KindSafeModeSoon:
		return fmt.Sprintf("Summa42: safe mode begins at %s unless you run: summa42 emergency heartbeat", identifierText(e.Code))
	case KindBudgetLow:
		return fmt.Sprintf("Summa42: envelope %s has used %s percent of its limit. Run: summa42 envelope list", subject, identifierText(e.Code))
	case KindContained:
		return fmt.Sprintf("Summa42: attempt %s was contained (%s).", subject, identifierText(e.Code))
	}
	return "Summa42: an event of an unknown kind was recorded."
}

// blockCodes maps the head of a recorded hold reason to a word. The reason's
// free text (a reviewer's sentence, an error) is never copied.
var blockCodes = []struct{ head, code string }{
	{"plan-review:fallback:", "plan-review-by-replacement-reviewer"},
	{"plan-review:REVISE", "plan-review-revise"},
	{"plan-review:BLOCK", "plan-review-block"},
	{"plan-review:implementation-grant-missing", "no-implementation-grant"},
	{"plan-review:stale-base", "plan-on-stale-base"},
	{"plan-review", "plan-review-held"},
	{"code-review:fallback:", "code-review-by-replacement-reviewer"},
	{"code-review:REVISE", "code-review-revise"},
	{"code-review:BLOCK", "code-review-block"},
	{"code-review", "code-review-held"},
	{"implementation-failed", "implementation-failed"},
	{"maximum steps exhausted", "step-limit"},
	{"remaining budget exhausted", "budget-exhausted"},
	{"no progress detected", "no-progress"},
	{"superseded", "superseded-by-newer-revision"},
}

func blockCode(reason string) string {
	for _, entry := range blockCodes {
		if strings.HasPrefix(reason, entry.head) {
			return entry.code
		}
	}
	return "other"
}

// Collect reads what currently needs the Owner. It only reads.
func Collect(ctx context.Context, store *state.Store, now time.Time) ([]Event, error) {
	if store == nil {
		return nil, errors.New("notify: store is required")
	}
	db := store.DB()
	var events []Event

	rows, err := db.QueryContext(ctx, `SELECT approval_id, subject_kind FROM approval_requests WHERE state = 'PENDING' ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("notify: pending approvals: %w", err)
	}
	for rows.Next() {
		var id, subjectKind string
		if err := rows.Scan(&id, &subjectKind); err != nil {
			rows.Close()
			return nil, err
		}
		events = append(events, Event{Key: "approval:" + id, Kind: KindApprovalWaiting, Subject: id, Object: subjectKind})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = db.QueryContext(ctx, `
		SELECT c.case_id, c.object_id, c.state, c.updated_at,
		       COALESCE((SELECT a.result_json FROM workflow_assessments a WHERE a.case_id = c.case_id ORDER BY a.created_at DESC, a.rowid DESC LIMIT 1), '')
		FROM workflow_cases c WHERE c.state IN ('BLOCKED', 'READY_FOR_VERIFICATION') ORDER BY c.updated_at`)
	if err != nil {
		return nil, fmt.Errorf("notify: cases: %w", err)
	}
	for rows.Next() {
		var id, object, caseState, updatedAt, resultJSON string
		if err := rows.Scan(&id, &object, &caseState, &updatedAt, &resultJSON); err != nil {
			rows.Close()
			return nil, err
		}
		if caseState == "READY_FOR_VERIFICATION" {
			events = append(events, Event{Key: "case-ready:" + id, Kind: KindCaseReady, Subject: id, Object: object})
			continue
		}
		var result struct {
			Decision struct{ Reason string }
		}
		_ = json.Unmarshal([]byte(resultJSON), &result)
		code := blockCode(result.Decision.Reason)
		if code == "superseded-by-newer-revision" {
			continue // a newer revision of the same issue carries on; nothing to do
		}
		events = append(events, Event{Key: "case-blocked:" + id + ":" + updatedAt, Kind: KindCaseBlocked, Subject: id, Object: object, Code: code})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var breakGlass string
	var frozenAt sql.NullString
	err = db.QueryRowContext(ctx, `SELECT break_glass_state, frozen_at FROM emergency_controls WHERE singleton = 1`).Scan(&breakGlass, &frozenAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("notify: break glass: %w", err)
	}
	if err == nil && breakGlass == "FROZEN" {
		events = append(events, Event{Key: "frozen:" + frozenAt.String, Kind: KindFrozen})
	}

	var deadMan, lastHeartbeat string
	var timeoutSeconds int
	err = db.QueryRowContext(ctx, `SELECT state, last_heartbeat_at, heartbeat_timeout_seconds FROM dead_man_switch WHERE singleton = 1`).Scan(&deadMan, &lastHeartbeat, &timeoutSeconds)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("notify: dead man switch: %w", err)
	}
	if err == nil {
		heartbeat, parseErr := time.Parse(time.RFC3339Nano, lastHeartbeat)
		horizon := heartbeat.Add(time.Duration(timeoutSeconds) * time.Second)
		switch {
		case deadMan == "SAFE_MODE" || (parseErr == nil && !now.Before(horizon)):
			events = append(events, Event{Key: "safe-mode:" + lastHeartbeat, Kind: KindSafeMode})
		case parseErr == nil && horizon.Sub(now) <= SafeModeWarning && time.Duration(timeoutSeconds)*time.Second > SafeModeWarning:
			events = append(events, Event{Key: "safe-mode-soon:" + lastHeartbeat, Kind: KindSafeModeSoon, Code: horizon.UTC().Format("2006-01-02T15:04Z")})
		}
	}

	rows, err = db.QueryContext(ctx, `
		SELECT e.envelope_id, e.hard_limit,
		       COALESCE(SUM(CASE WHEN r.state = 'SETTLED' THEN r.settled_amount WHEN r.state IN ('HELD', 'UNRESOLVED') THEN r.reserved_amount ELSE 0 END), 0)
		FROM resource_envelopes e LEFT JOIN resource_reservations r ON r.envelope_id = e.envelope_id
		GROUP BY e.envelope_id, e.hard_limit ORDER BY e.envelope_id`)
	if err != nil {
		return nil, fmt.Errorf("notify: envelopes: %w", err)
	}
	for rows.Next() {
		var id string
		var limit, used int64
		if err := rows.Scan(&id, &limit, &used); err != nil {
			rows.Close()
			return nil, err
		}
		if limit > 0 && used*100 >= limit*BudgetLowPercent {
			events = append(events, Event{Key: fmt.Sprintf("budget-low:%s:%d", id, limit), Kind: KindBudgetLow, Subject: id, Code: fmt.Sprintf("%d", used*100/limit)})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = db.QueryContext(ctx, `SELECT attempt_id, ladder_state FROM attempt_containments WHERE ladder_state IN ('RESTRICTED', 'SUSPECTED_ROGUE') ORDER BY updated_at`)
	if err != nil {
		return nil, fmt.Errorf("notify: containments: %w", err)
	}
	for rows.Next() {
		var attempt, ladder string
		if err := rows.Scan(&attempt, &ladder); err != nil {
			rows.Close()
			return nil, err
		}
		events = append(events, Event{Key: "contained:" + attempt + ":" + ladder, Kind: KindContained, Subject: attempt, Code: ladder})
	}
	rows.Close()
	return events, rows.Err()
}

// Outcome is what a sender knows about one send.
type Outcome int

const (
	// Delivered: the receiver confirmed it.
	Delivered Outcome = iota
	// NotSent: the message definitely did not arrive; it may be sent again.
	NotSent
	// Unknown: it may or may not have arrived; it is not sent again.
	Unknown
)

// Sender delivers one message to the Owner.
type Sender interface {
	Send(ctx context.Context, message string) (Outcome, error)
}

// Outbox is the durable queue of notifications.
type Outbox struct {
	store *state.Store
	clock clock.Clock
}

func NewOutbox(store *state.Store, clk clock.Clock) *Outbox {
	return &Outbox{store: store, clock: clk}
}

// Enqueue records the events that are not queued yet and returns how many
// were new.
func (o *Outbox) Enqueue(ctx context.Context, events []Event) (int, error) {
	added := 0
	stamp := domain.FormatTime(o.clock.Now())
	for _, event := range events {
		if strings.TrimSpace(event.Key) == "" {
			return added, errors.New("notify: event without a key")
		}
		result, err := o.store.DB().ExecContext(ctx, `INSERT OR IGNORE INTO notification_outbox (event_key, kind, message, state, created_at, updated_at) VALUES (?, ?, ?, 'PENDING', ?, ?)`,
			event.Key, event.Kind, event.Message(), stamp, stamp)
		if err != nil {
			return added, fmt.Errorf("notify: enqueue %s: %w", event.Key, err)
		}
		if n, _ := result.RowsAffected(); n > 0 {
			added++
		}
	}
	return added, nil
}

// DeliveryResult counts what one Deliver did.
type DeliveryResult struct {
	Delivered, NotSent, Unknown int
}

// Deliver sends the pending messages, oldest first. A row left SENDING by an
// earlier run that did not finish is settled as UNKNOWN first: whether that
// message arrived is not known, so it is not sent again.
func (o *Outbox) Deliver(ctx context.Context, sender Sender) (DeliveryResult, error) {
	var result DeliveryResult
	if sender == nil {
		return result, errors.New("notify: sender is required")
	}
	db := o.store.DB()
	stamp := func() string { return domain.FormatTime(o.clock.Now()) }
	if _, err := db.ExecContext(ctx, `UPDATE notification_outbox SET state = 'UNKNOWN', last_error = 'interrupted while sending', updated_at = ? WHERE state = 'SENDING'`, stamp()); err != nil {
		return result, fmt.Errorf("notify: settle interrupted sends: %w", err)
	}
	type pending struct{ key, message string }
	rows, err := db.QueryContext(ctx, `SELECT event_key, message FROM notification_outbox WHERE state = 'PENDING' ORDER BY created_at, event_key LIMIT 50`)
	if err != nil {
		return result, fmt.Errorf("notify: read outbox: %w", err)
	}
	var queue []pending
	for rows.Next() {
		var item pending
		if err := rows.Scan(&item.key, &item.message); err != nil {
			rows.Close()
			return result, err
		}
		queue = append(queue, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return result, err
	}
	for _, item := range queue {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		claimed, err := db.ExecContext(ctx, `UPDATE notification_outbox SET state = 'SENDING', attempts = attempts + 1, updated_at = ? WHERE event_key = ? AND state = 'PENDING'`, stamp(), item.key)
		if err != nil {
			return result, fmt.Errorf("notify: claim %s: %w", item.key, err)
		}
		if n, _ := claimed.RowsAffected(); n == 0 {
			continue // another process took it
		}
		outcome, sendErr := sender.Send(ctx, item.message)
		next, detail := "DELIVERED", ""
		switch outcome {
		case Delivered:
			result.Delivered++
		case NotSent:
			next = "PENDING"
			result.NotSent++
		default:
			next = "UNKNOWN"
			result.Unknown++
		}
		if sendErr != nil {
			detail = oneLineError(sendErr)
		}
		// The settle must not be lost to a cancelled context: the send happened.
		settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		_, err = db.ExecContext(settleCtx, `UPDATE notification_outbox SET state = ?, last_error = ?, updated_at = ? WHERE event_key = ? AND state = 'SENDING'`, next, detail, stamp(), item.key)
		cancel()
		if err != nil {
			return result, fmt.Errorf("notify: settle %s: %w", item.key, err)
		}
		if outcome == NotSent {
			// The channel is not taking messages now; try the rest next time.
			return result, nil
		}
	}
	return result, nil
}

func oneLineError(err error) string {
	text := strings.Join(strings.Fields(err.Error()), " ")
	if len(text) > 300 {
		text = text[:300]
	}
	return text
}

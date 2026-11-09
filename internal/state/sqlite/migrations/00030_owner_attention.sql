-- +goose Up
CREATE TABLE owner_attention_budgets (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    daily_budget_units INTEGER NOT NULL CHECK (daily_budget_units > 0),
    used_budget_units INTEGER NOT NULL DEFAULT 0 CHECK (used_budget_units >= 0),
    period_start TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE owner_decision_packets (
    packet_id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    category TEXT NOT NULL CHECK (category IN (
        'CONSTITUTIONAL',
        'STRATEGIC_MISSION',
        'EXPOSURE_THRESHOLD',
        'SAFETY_OVERRIDE',
        'DELEGATED_AUTONOMOUS'
    )),
    urgency TEXT NOT NULL CHECK (urgency IN ('LOW', 'MEDIUM', 'HIGH', 'CRITICAL')),
    sovereign_required INTEGER NOT NULL CHECK (sovereign_required IN (0, 1)),
    status TEXT NOT NULL CHECK (status IN ('PENDING', 'APPROVED', 'REJECTED', 'AUTONOMOUSLY_RESOLVED', 'EXPIRED')),
    estimated_attention_units INTEGER NOT NULL CHECK (estimated_attention_units >= 0),
    summary TEXT NOT NULL,
    evidence_ids_json TEXT NOT NULL,
    sub_decisions_json TEXT NOT NULL,
    proposed_action_json TEXT NOT NULL,
    resolved_at TEXT,
    resolved_by_principal_id TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX owner_decision_packets_status ON owner_decision_packets(status, urgency, sovereign_required);

-- Seed singleton budget
INSERT INTO owner_attention_budgets (singleton, daily_budget_units, used_budget_units, period_start, updated_at)
VALUES (1, 100, 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- +goose Down
DROP TABLE owner_decision_packets;
DROP TABLE owner_attention_budgets;

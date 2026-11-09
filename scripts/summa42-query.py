#!/usr/bin/env python3
"""
summa42-query.py - read-only CLI over the canonical Summa42 SQLite database.

Only the Box writes canonical state. This script opens the database with
mode=ro and a short busy timeout, and offers read subcommands only
(status, cases, tickets, capacity, board). Paths resolve from SUMMA42_HOME (default ~/.summa42).
"""

import argparse
import json
import os
import sqlite3
import sys
from datetime import datetime, timezone
from urllib.parse import quote


def home():
    return os.environ.get("SUMMA42_HOME") or os.path.expanduser("~/.summa42")


def get_db():
    path = os.path.join(home(), "state", "summa42.db")
    if not os.path.exists(path):
        print(json.dumps({"error": "NO_DB"}))
        sys.exit(0)
    return sqlite3.connect("file:" + quote(path) + "?mode=ro", uri=True, timeout=5)


def cmd_status():
    cur = get_db().cursor()
    cur.execute("SELECT mission_id, statement FROM missions WHERE active = 1 ORDER BY created_at DESC LIMIT 1")
    m = cur.fetchone()
    cur.execute("SELECT count(*) FROM workflow_cases")
    cases = cur.fetchone()[0]
    cases_by_phase = {}
    try:
        cur.execute("SELECT phase, count(*) FROM workflow_cases GROUP BY phase")
        cases_by_phase = {r[0]: r[1] for r in cur.fetchall()}
    except sqlite3.OperationalError:
        pass
    cur.execute("SELECT count(*) FROM experience_rules WHERE state = 'ACTIVE'")
    rules = cur.fetchone()[0]
    res = {
        "mission": m[1] if m else "No active mission",
        "cases": cases,
        "rules": rules,
    }
    if cases_by_phase:
        res["cases_by_phase"] = cases_by_phase
    print(json.dumps(res))


def cmd_cases():
    cur = get_db().cursor()
    try:
        cur.execute("""
            SELECT case_id, source, object_id, state, current_work_id, completed_steps, max_steps, phase
            FROM workflow_cases
            WHERE state = 'ACTIVE'
            ORDER BY created_at DESC
            LIMIT 25
        """)
        print(json.dumps([{
            "case_id": r[0],
            "source": r[1],
            "object_id": r[2],
            "state": r[3],
            "current_work_id": r[4],
            "steps": f"{r[5]}/{r[6]}",
            "phase": r[7],
        } for r in cur.fetchall()]))
    except sqlite3.OperationalError:
        cur.execute("""
            SELECT case_id, source, object_id, state, current_work_id, completed_steps, max_steps
            FROM workflow_cases
            WHERE state = 'ACTIVE'
            ORDER BY created_at DESC
            LIMIT 25
        """)
        print(json.dumps([{
            "case_id": r[0],
            "source": r[1],
            "object_id": r[2],
            "state": r[3],
            "current_work_id": r[4],
            "steps": f"{r[5]}/{r[6]}",
        } for r in cur.fetchall()]))


def cmd_tickets():
    cur = get_db().cursor()
    cur.execute("""
        SELECT content_hash FROM evidence_objects
        WHERE kind = 'github.issue.snapshot'
        ORDER BY created_at DESC
        LIMIT 200
    """)
    evidence_root = os.path.join(home(), "evidence", "blobs", "sha256")
    items = []
    seen = set()
    for (h,) in cur.fetchall():
        p = os.path.join(evidence_root, h[:2], h)
        try:
            with open(p, "r", encoding="utf-8") as f:
                d = json.load(f)
        except (OSError, ValueError):
            continue
        num = d.get("issue")
        if not num or num in seen:
            continue
        seen.add(num)
        items.append({
            "number": num,
            "title": d.get("title", ""),
            # The canonical snapshot may carry no state; never claim OPEN.
            "state": d.get("state") or "UNKNOWN",
            "url": d.get("url", ""),
        })
    items.sort(key=lambda x: x["number"], reverse=True)
    print(json.dumps(items[:40]))


def cmd_capacity():
    """Pools from the model-call ledger: exhausted or not, the reset time, and
    today's calls. Mirrors the Box's PoolState rule so both agree on one ledger."""
    cur = get_db().cursor()
    try:
        cur.execute("SELECT 1 FROM model_calls LIMIT 1")
    except sqlite3.OperationalError:
        print(json.dumps({"pools": [], "error": "NO_LEDGER"}))
        return
    now = datetime.now(timezone.utc)
    now_s = now.strftime("%Y-%m-%dT%H:%M:%SZ")
    today_s = now.replace(hour=0, minute=0, second=0, microsecond=0).strftime("%Y-%m-%dT%H:%M:%SZ")
    pools = []
    for (pool,) in cur.execute("SELECT DISTINCT pool FROM model_calls ORDER BY pool").fetchall():
        exhausted, reset = False, None
        last = cur.execute(
            "SELECT call_id, reset_at FROM model_calls WHERE pool=? AND outcome='refused' ORDER BY call_id DESC LIMIT 1",
            (pool,)).fetchone()
        if last and last[1] and last[1] > now_s:
            ok_after = cur.execute(
                "SELECT count(*) FROM model_calls WHERE pool=? AND outcome='ok' AND call_id>?",
                (pool, last[0])).fetchone()[0]
            if ok_after == 0:
                exhausted, reset = True, last[1]
        calls = {}
        tokens_in = tokens_out = 0
        for outcome, n, tin, tout in cur.execute("""
            SELECT outcome, count(*), coalesce(sum(input_tokens), 0), coalesce(sum(output_tokens), 0)
            FROM model_calls WHERE pool=? AND started_at>=? GROUP BY outcome
        """, (pool, today_s)).fetchall():
            calls[outcome] = n
            tokens_in += tin
            tokens_out += tout
        pools.append({
            "pool": pool,
            "state": "exhausted" if exhausted else "available",
            "reset_at": reset,
            "calls_today": calls,
            "input_tokens_today": tokens_in,
            "output_tokens_today": tokens_out,
        })
    print(json.dumps({
        "now": now_s,
        "pools": pools,
        "note": "remaining quota is unknown unless the CLI reports it; 'available' means no known refusal",
    }))


def cmd_board():
    cur = get_db().cursor()
    cur.execute("SELECT mission_id FROM missions WHERE active = 1 ORDER BY created_at DESC LIMIT 1")
    m = cur.fetchone()
    if not m:
        print(json.dumps({"error": "NO_ACTIVE_MISSION"}))
        return
    mission_id = m[0]
    try:
        cur.execute("SELECT column_name, count(*) FROM board_cards WHERE mission_id = ? GROUP BY column_name", (mission_id,))
        columns = {r[0]: r[1] for r in cur.fetchall()}
        cur.execute("""
            SELECT c.card_id, c.source, c.source_id, c.title, m.reason, m.created_at
            FROM board_cards c
            JOIN board_card_moves m ON m.card_id = c.card_id
            WHERE c.mission_id = ? AND c.column_name = 'HELD' AND m.to_column = 'HELD'
            ORDER BY m.created_at DESC
        """, (mission_id,))
        seen = set()
        held = []
        for r in cur.fetchall():
            if r[0] not in seen:
                seen.add(r[0])
                held.append({
                    "card_id": r[0],
                    "source": r[1],
                    "source_id": r[2],
                    "title": r[3],
                    "reason": r[4],
                    "held_at": r[5],
                })
        print(json.dumps({
            "mission_id": mission_id,
            "columns": columns,
            "held_cards": held,
        }))
    except sqlite3.OperationalError:
        print(json.dumps({"error": "NO_BOARD_TABLE"}))


def cmd_cubes():
    cur = get_db().cursor()
    try:
        cur.execute("""
            SELECT cube_id, principal_id, state, quarantine_reason, quarantined_at, quarantined_by, quarantine_mode, created_at, updated_at
            FROM cubes
            ORDER BY created_at ASC
        """)
        cubes = []
        for r in cur.fetchall():
            cubes.append({
                "cube_id": r[0],
                "principal_id": r[1],
                "state": r[2],
                "quarantine_reason": r[3],
                "quarantined_at": r[4],
                "quarantined_by": r[5],
                "quarantine_mode": r[6],
                "created_at": r[7],
                "updated_at": r[8],
            })
        print(json.dumps({"cubes": cubes}))
    except sqlite3.OperationalError:
        print(json.dumps({"error": "NO_CUBES_TABLE"}))


def cmd_emergency():
    cur = get_db().cursor()
    try:
        cur.execute("""
            SELECT break_glass_state, frozen_at, frozen_by_principal_id, freeze_reason, restored_at, restored_by_principal_id, updated_at
            FROM emergency_controls
            WHERE singleton = 1
        """)
        bg_row = cur.fetchone()
        bg = {}
        if bg_row:
            bg = {
                "break_glass_state": bg_row[0],
                "frozen_at": bg_row[1],
                "frozen_by_principal_id": bg_row[2],
                "freeze_reason": bg_row[3],
                "restored_at": bg_row[4],
                "restored_by_principal_id": bg_row[5],
                "updated_at": bg_row[6],
            }

        cur.execute("""
            SELECT state, last_heartbeat_at, heartbeat_timeout_seconds, safe_mode_triggered_at, updated_at
            FROM dead_man_switch
            WHERE singleton = 1
        """)
        dm_row = cur.fetchone()
        dm = {}
        if dm_row:
            dm = {
                "dead_man_state": dm_row[0],
                "last_heartbeat_at": dm_row[1],
                "heartbeat_timeout_seconds": dm_row[2],
                "safe_mode_triggered_at": dm_row[3],
                "updated_at": dm_row[4],
            }

        print(json.dumps({
            "break_glass": bg,
            "dead_man_switch": dm,
        }))
    except sqlite3.OperationalError:
        print(json.dumps({"error": "NO_EMERGENCY_TABLES"}))


def cmd_containment():
    cur = get_db().cursor()
    try:
        cur.execute("""
            SELECT attempt_id, task_id, ladder_state, violation_signals_json, investigation_task_id, created_at, updated_at
            FROM attempt_containments
            ORDER BY updated_at DESC
        """)
        records = []
        for r in cur.fetchall():
            records.append({
                "attempt_id": r[0],
                "task_id": r[1],
                "ladder_state": r[2],
                "violation_signals": json.loads(r[3]) if r[3] else [],
                "investigation_task_id": r[4],
                "created_at": r[5],
                "updated_at": r[6],
            })
        print(json.dumps(records))
    except sqlite3.OperationalError:
        print(json.dumps({"error": "NO_CONTAINMENT_TABLE"}))


def cmd_attention():
    cur = get_db().cursor()
    try:
        cur.execute("""
            SELECT daily_budget_units, used_budget_units, period_start, updated_at
            FROM owner_attention_budgets
            WHERE singleton = 1
        """)
        b_row = cur.fetchone()
        budget = {}
        if b_row:
            budget = {
                "daily_budget_units": b_row[0],
                "used_budget_units": b_row[1],
                "remaining_budget_units": max(0, b_row[0] - b_row[1]),
                "period_start": b_row[2],
                "updated_at": b_row[3],
            }

        cur.execute("""
            SELECT packet_id, title, category, urgency, sovereign_required, status, estimated_attention_units, summary, evidence_ids_json, sub_decisions_json, proposed_action_json, resolved_at, resolved_by_principal_id, created_at, updated_at
            FROM owner_decision_packets
            WHERE status = 'PENDING'
            ORDER BY CASE urgency
                WHEN 'CRITICAL' THEN 1
                WHEN 'HIGH' THEN 2
                WHEN 'MEDIUM' THEN 3
                WHEN 'LOW' THEN 4
                ELSE 5
            END, created_at ASC
        """)
        packets = []
        for r in cur.fetchall():
            packets.append({
                "packet_id": r[0],
                "title": r[1],
                "category": r[2],
                "urgency": r[3],
                "sovereign_required": bool(r[4]),
                "status": r[5],
                "estimated_attention_units": r[6],
                "summary": r[7],
                "evidence_ids": json.loads(r[8]) if r[8] else [],
                "sub_decisions": json.loads(r[9]) if r[9] else [],
                "proposed_action": json.loads(r[10]) if r[10] else {},
                "resolved_at": r[11],
                "resolved_by_principal_id": r[12],
                "created_at": r[13],
                "updated_at": r[14],
            })

        print(json.dumps({
            "budget": budget,
            "pending_packets": packets,
        }))
    except sqlite3.OperationalError:
        print(json.dumps({"error": "NO_ATTENTION_TABLES"}))


def cmd_company():
    cur = get_db().cursor()
    try:
        cur.execute("""
            SELECT kind, count(*), sum(exposure_amount)
            FROM company_entities
            GROUP BY kind
        """)
        by_kind = {}
        total_entities = 0
        total_exposure = 0
        for r in cur.fetchall():
            kind = r[0]
            count = r[1]
            exp = r[2] or 0
            by_kind[kind] = count
            total_entities += count
            total_exposure += exp

        cur.execute("""
            SELECT entity_id, kind, name, status, attributes_json, exposure_amount, updated_at
            FROM company_entities
            ORDER BY updated_at DESC
            LIMIT 50
        """)
        entities = []
        for r in cur.fetchall():
            attrs = {}
            if r[4]:
                try:
                    attrs = json.loads(r[4])
                except (json.JSONDecodeError, TypeError):
                    pass
            entities.append({
                "entity_id": r[0],
                "kind": r[1],
                "name": r[2],
                "status": r[3],
                "attributes": attrs,
                "exposure_amount": r[5],
                "updated_at": r[6],
            })

        cur.execute("""
            SELECT transition_id, entity_id, task_id, evidence_id, transition_kind, recorded_at
            FROM company_state_transitions
            ORDER BY recorded_at DESC
            LIMIT 25
        """)
        transitions = []
        for r in cur.fetchall():
            transitions.append({
                "transition_id": r[0],
                "entity_id": r[1],
                "task_id": r[2],
                "evidence_id": r[3],
                "transition_kind": r[4],
                "recorded_at": r[5],
            })

        print(json.dumps({
            "total_entities": total_entities,
            "total_exposure": total_exposure,
            "entities_by_kind": by_kind,
            "recent_entities": entities,
            "recent_transitions": transitions,
        }))
    except sqlite3.OperationalError:
        print(json.dumps({"error": "NO_COMPANY_TABLES"}))


def cmd_obligations():
    cur = get_db().cursor()
    try:
        cur.execute("""
            SELECT obligation_id, source, statement, active, due_at,
                   grace_period_seconds, consequences_json, cost_of_delay_rate,
                   lead_time_seconds, lifecycle_state, synthesized_task_id,
                   entity_id, created_at, updated_at, resolved_at
            FROM obligations
            ORDER BY CASE lifecycle_state
                WHEN 'PENDING' THEN 1
                WHEN 'SCHEDULED' THEN 2
                WHEN 'ACTIVE' THEN 3
                WHEN 'BREACHED' THEN 4
                WHEN 'FULFILLED' THEN 5
                ELSE 6
            END, due_at ASC
        """)
        obligations = []
        for r in cur.fetchall():
            consequences = {}
            if r[6]:
                try:
                    consequences = json.loads(r[6])
                except (json.JSONDecodeError, TypeError):
                    pass
            obligations.append({
                "obligation_id": r[0],
                "source": r[1],
                "statement": r[2],
                "active": bool(r[3]),
                "due_at": r[4],
                "grace_period_seconds": r[5],
                "consequences": consequences,
                "cost_of_delay_rate": r[7],
                "lead_time_seconds": r[8],
                "lifecycle_state": r[9],
                "synthesized_task_id": r[10],
                "entity_id": r[11],
                "created_at": r[12],
                "updated_at": r[13],
                "resolved_at": r[14],
            })
        print(json.dumps(obligations))
    except sqlite3.OperationalError:
        print(json.dumps({"error": "NO_OBLIGATIONS_TABLE"}))


def cmd_invariants():
    cur = get_db().cursor()
    try:
        cur.execute("""
            SELECT invariant_id, name, description, target_kind, rule_type,
                   rule_config_json, severity, action_type, action_config_json,
                   enabled, created_at, updated_at
            FROM business_invariants
            ORDER BY created_at ASC
        """)
        invariants = []
        for r in cur.fetchall():
            rule_cfg = {}
            action_cfg = {}
            if r[5]:
                try:
                    rule_cfg = json.loads(r[5])
                except (json.JSONDecodeError, TypeError):
                    pass
            if r[8]:
                try:
                    action_cfg = json.loads(r[8])
                except (json.JSONDecodeError, TypeError):
                    pass
            invariants.append({
                "invariant_id": r[0],
                "name": r[1],
                "description": r[2],
                "target_kind": r[3],
                "rule_type": r[4],
                "rule_config": rule_cfg,
                "severity": r[6],
                "action_type": r[7],
                "action_config": action_cfg,
                "enabled": bool(r[9]),
                "created_at": r[10],
                "updated_at": r[11],
            })

        cur.execute("""
            SELECT evaluation_id, invariant_id, entity_id, status, observed_value_json,
                   evidence_id, triggered_task_id, triggered_packet_id, containment_signal, evaluated_at
            FROM invariant_evaluations
            ORDER BY evaluated_at DESC LIMIT 20
        """)
        evaluations = []
        for r in cur.fetchall():
            obs = {}
            if r[4]:
                try:
                    obs = json.loads(r[4])
                except (json.JSONDecodeError, TypeError):
                    pass
            evaluations.append({
                "evaluation_id": r[0],
                "invariant_id": r[1],
                "entity_id": r[2],
                "status": r[3],
                "observed_value": obs,
                "evidence_id": r[5],
                "triggered_task_id": r[6],
                "triggered_packet_id": r[7],
                "containment_signal": r[8],
                "evaluated_at": r[9],
            })

        print(json.dumps({
            "invariants": invariants,
            "recent_evaluations": evaluations,
        }))
    except sqlite3.OperationalError:
        print(json.dumps({"error": "NO_INVARIANTS_TABLE"}))


def main(argv=None):
    parser = argparse.ArgumentParser(description="Read-only Summa42 queries")
    parser.add_argument("command", choices=["status", "cases", "tickets", "capacity", "board", "cubes", "emergency", "containment", "attention", "company", "obligations", "invariants"])
    args = parser.parse_args(argv)
    {"status": cmd_status, "cases": cmd_cases, "tickets": cmd_tickets, "capacity": cmd_capacity, "board": cmd_board, "cubes": cmd_cubes, "emergency": cmd_emergency, "containment": cmd_containment, "attention": cmd_attention, "company": cmd_company, "obligations": cmd_obligations, "invariants": cmd_invariants}[args.command]()


if __name__ == "__main__":
    main()

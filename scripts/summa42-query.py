#!/usr/bin/env python3
"""
summa42-query.py - read-only CLI over the canonical Summa42 SQLite database.

Only the Box writes canonical state. This script opens the database with
mode=ro and a short busy timeout, and offers read subcommands only
(status, cases, tickets, capacity). Paths resolve from SUMMA42_HOME (default ~/.summa42).
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
    cur.execute("SELECT count(*) FROM experience_rules WHERE state = 'ACTIVE'")
    rules = cur.fetchone()[0]
    print(json.dumps({"mission": m[1] if m else "No active mission", "cases": cases, "rules": rules}))


def cmd_cases():
    cur = get_db().cursor()
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


def main(argv=None):
    parser = argparse.ArgumentParser(description="Read-only Summa42 queries")
    parser.add_argument("command", choices=["status", "cases", "tickets", "capacity"])
    args = parser.parse_args(argv)
    {"status": cmd_status, "cases": cmd_cases, "tickets": cmd_tickets, "capacity": cmd_capacity}[args.command]()


if __name__ == "__main__":
    main()

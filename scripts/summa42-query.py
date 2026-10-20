#!/usr/bin/env python3
"""
summa42-query.py - Secure, parameterized CLI interface to Summa42 SQLite database.
Replaces all dynamic string-concatenated / f-string SQL queries in the ACP bridge.
Takes arguments via CLI / stdin JSON, uses 100% parameterized SQL.
"""

import sys
import os
import sqlite3
import json
import time
import random
import hashlib

DB_PATH = os.environ.get("SUMMA42_DB", os.path.expanduser("~/.summa42/state/summa42.db"))

def get_db():
    if not os.path.exists(DB_PATH):
        print(json.dumps({"error": "NO_DB"}))
        sys.exit(0)
    return sqlite3.connect(DB_PATH)

def cmd_status():
    conn = get_db()
    cur = conn.cursor()
    cur.execute("SELECT mission_id, statement FROM missions WHERE active = 1 ORDER BY created_at DESC LIMIT 1")
    m = cur.fetchone()
    mission = m[1] if m else "No active mission"
    cur.execute("SELECT count(*) FROM workflow_cases")
    cases = cur.fetchone()[0]
    cur.execute("SELECT count(*) FROM experience_rules WHERE state = 'ACTIVE'")
    rules = cur.fetchone()[0]
    print(json.dumps({
        "mission": mission,
        "cases": cases,
        "rules": rules
    }))

def cmd_cases():
    conn = get_db()
    cur = conn.cursor()
    cur.execute("""
        SELECT case_id, source, object_id, state, current_work_id, completed_steps, max_steps
        FROM workflow_cases
        WHERE state = 'ACTIVE'
        ORDER BY created_at DESC
        LIMIT 25
    """)
    rows = cur.fetchall()
    res = []
    for r in rows:
        res.append({
            "case_id": r[0],
            "source": r[1],
            "object_id": r[2],
            "state": r[3],
            "current_work_id": r[4],
            "steps": f"{r[5]}/{r[6]}"
        })
    print(json.dumps(res))

def cmd_tickets():
    conn = get_db()
    cur = conn.cursor()
    cur.execute("""
        SELECT e.content_hash
        FROM evidence_objects e
        WHERE e.kind = 'github.issue.snapshot'
        ORDER BY e.created_at DESC
        LIMIT 200
    """)
    hashes = cur.fetchall()
    evidence_root = os.path.expanduser("~/.summa42/evidence/blobs/sha256")
    items = []
    seen = set()
    for (h,) in hashes:
        p = os.path.join(evidence_root, h[:2], h)
        if not os.path.exists(p):
            continue
        try:
            with open(p, "r", encoding="utf-8") as f:
                d = json.load(f)
            num = d.get("issue")
            if not num or num in seen:
                continue
            seen.add(num)
            items.append({
                "number": num,
                "title": d.get("title", ""),
                "state": d.get("state", ""),
                "url": d.get("url", "")
            })
        except Exception:
            continue
    items.sort(key=lambda x: x["number"], reverse=True)
    print(json.dumps(items[:40]))

def cmd_context():
    conn = get_db()
    cur = conn.cursor()
    cur.execute("SELECT mission_id, statement FROM missions WHERE active = 1 ORDER BY created_at DESC LIMIT 1")
    m = cur.fetchone()
    mission = m[1] if m else ""

    cur.execute("SELECT task_id, objective, purpose_kind FROM tasks WHERE state = 'EXECUTING'")
    exec_tasks = [f"{r[0]} ({r[1]})" for r in cur.fetchall()]

    cur.execute("SELECT task_id, objective FROM tasks WHERE purpose_kind = 'OWNER_DIRECTIVE' AND state = 'ELIGIBLE'")
    directives = [f"{r[0]}: {r[1]}" for r in cur.fetchall()]

    cur.execute("SELECT count(*) FROM workflow_cases WHERE state = 'ACTIVE'")
    active_cases = cur.fetchone()[0]

    print(json.dumps({
        "mission": mission,
        "executing_tasks": ", ".join(exec_tasks) if exec_tasks else "Brak",
        "directives": "; ".join(directives) if directives else "Brak",
        "active_cases": active_cases
    }))

def cmd_set_mission():
    payload = json.loads(sys.stdin.read())
    statement = payload.get("statement", "").strip()
    if not statement:
        print(json.dumps({"error": "Empty statement"}))
        return

    conn = get_db()
    cur = conn.cursor()
    cur.execute("UPDATE missions SET active = 0 WHERE active = 1")
    mid = "mission_" + "".join(random.choices("0123456789abcdef", k=8))
    now = time.strftime("%Y-%m-%d %H:%M:%S")
    cur.execute("INSERT INTO missions(mission_id, statement, active, created_at) VALUES (?, ?, 1, ?)", (mid, statement, now))
    conn.commit()
    print(json.dumps({"mission_id": mid, "statement": statement}))

def cmd_inject_directive():
    payload = json.loads(sys.stdin.read())
    text = payload.get("text", "").strip()
    sender_id = payload.get("sender_id", "owner_discord").strip()
    if not text:
        print(json.dumps({"error": "Empty directive"}))
        return

    conn = get_db()
    cur = conn.cursor()
    
    # Generate random hash id without any string interpolation
    now_ts = str(time.time()).encode("utf-8")
    raw_bytes = text.encode("utf-8") + now_ts
    tid = "task_dir_" + hashlib.md5(raw_bytes).hexdigest()[:12]
    cid = "case_dir_" + hashlib.md5(tid.encode("utf-8")).hexdigest()[:12]
    now = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())

    cur.execute("""
    INSERT INTO tasks(
        task_id, purpose_kind, purpose_id, state, current_fence,
        acceptance_criteria_json, required_capabilities_json, required_enforcement,
        authority_ceiling_json, resource_envelope_id, priority, created_at, updated_at,
        task_class, objective, payload_json
    ) VALUES (?, 'OWNER_DIRECTIVE', ?, 'ELIGIBLE', 0,
        '["Implemented in codebase","Tests pass"]',
        '["workspace.write","execution.run"]',
        'UNENFORCED',
        '["workspace.write","execution.run","github.pr.create"]',
        'envelope_1', 1000, ?, ?,
        'repo.feature.gateway', ?, '{}'
    )
    """, (tid, sender_id, now, now, text))

    cur.execute("SELECT mission_id FROM missions WHERE active = 1 ORDER BY created_at DESC LIMIT 1")
    m_row = cur.fetchone()
    mid = m_row[0] if m_row else "mission_owner"

    cur.execute("""
    INSERT INTO workflow_cases(
        case_id, mission_id, source, object_id, revision_id,
        observation_evidence_id, initial_request_json, grant_json,
        state, current_work_id, next_work_json, completed_steps,
        max_steps, remaining_budget, progress_signature, created_at, updated_at
    ) VALUES (?, ?, 'discord_owner', ?, 'v1',
        '', ?, ?,
        'ACTIVE', ?, '{}', 0, 5, 5, 'ready', ?, ?
    )
    """, (cid, mid, "owner_directive:" + tid, json.dumps({"directive": tid}), json.dumps({"priority": 1000}), "work_" + tid, now, now))

    conn.commit()
    print(json.dumps({"task_id": tid, "case_id": cid, "text": text}))

def main():
    if len(sys.argv) < 2:
        print("Usage: summa42-query.py <status|cases|tickets|context|set_mission|inject_directive>")
        sys.exit(1)

    cmd = sys.argv[1]
    if cmd == "status":
        cmd_status()
    elif cmd == "cases":
        cmd_cases()
    elif cmd == "tickets":
        cmd_tickets()
    elif cmd == "context":
        cmd_context()
    elif cmd == "set_mission":
        cmd_set_mission()
    elif cmd == "inject_directive":
        cmd_inject_directive()
    else:
        print(f"Unknown command: {cmd}")
        sys.exit(1)

if __name__ == "__main__":
    main()

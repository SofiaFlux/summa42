#!/usr/bin/env python3
"""
Tests for summa42-query.py: read-only access, removed write subcommands,
UNKNOWN ticket state, and SUMMA42_HOME resolution.

Run with:
    python3 -m unittest scripts/test_summa42_query.py -v

Every test builds a fresh SQLite DB in a temp SUMMA42_HOME; ~/.summa42 is never touched.
"""

import importlib.util
from datetime import datetime, timedelta, timezone
import json
import os
import sqlite3
import subprocess
import sys
import tempfile
import unittest

QUERY = os.path.join(os.path.dirname(os.path.abspath(__file__)), "summa42-query.py")


def _load_module():
    spec = importlib.util.spec_from_file_location("summa42_query", QUERY)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def _make_home(tmp, snapshot=None):
    os.makedirs(os.path.join(tmp, "state"))
    conn = sqlite3.connect(os.path.join(tmp, "state", "summa42.db"))
    conn.executescript("""
        CREATE TABLE missions(mission_id TEXT, statement TEXT, active INTEGER, created_at TEXT);
        CREATE TABLE workflow_cases(case_id TEXT, source TEXT, object_id TEXT, state TEXT,
            current_work_id TEXT, completed_steps INTEGER, max_steps INTEGER, created_at TEXT, phase TEXT);
        CREATE TABLE experience_rules(state TEXT);
        CREATE TABLE evidence_objects(kind TEXT, content_hash TEXT, created_at TEXT);
        INSERT INTO missions VALUES('m1', 'Ship it', 1, '2026-01-01');
    """)
    if snapshot is not None:
        h = "ab" + "0" * 62
        blob_dir = os.path.join(tmp, "evidence", "blobs", "sha256", h[:2])
        os.makedirs(blob_dir)
        with open(os.path.join(blob_dir, h), "w") as f:
            json.dump(snapshot, f)
        conn.execute("INSERT INTO evidence_objects VALUES('github.issue.snapshot', ?, '2026-01-01')", (h,))
    conn.commit()
    conn.close()


def _run(home, *args):
    env = {"PATH": os.environ.get("PATH", ""), "SUMMA42_HOME": home, "HOME": "/nonexistent"}
    return subprocess.run([sys.executable, QUERY, *args], env=env, capture_output=True, text=True, timeout=30)


class QueryTests(unittest.TestCase):
    def test_db_opened_read_only(self):
        with tempfile.TemporaryDirectory() as tmp:
            _make_home(tmp)
            os.environ["SUMMA42_HOME"] = tmp
            try:
                conn = _load_module().get_db()
                with self.assertRaises(sqlite3.OperationalError):
                    conn.execute("INSERT INTO missions VALUES('m2', 'x', 1, 'now')")
                conn.close()
            finally:
                del os.environ["SUMMA42_HOME"]

    def test_set_mission_and_inject_directive_removed(self):
        with tempfile.TemporaryDirectory() as tmp:
            _make_home(tmp)
            for sub in ("set_mission", "inject_directive"):
                self.assertEqual(_run(tmp, sub).returncode, 2, sub)

    def test_ticket_state_unknown_without_field(self):
        with tempfile.TemporaryDirectory() as tmp:
            _make_home(tmp, {"issue": 7, "title": "t", "url": "u"})
            res = _run(tmp, "tickets")
            self.assertEqual(res.returncode, 0, res.stderr)
            self.assertEqual(json.loads(res.stdout)[0]["state"], "UNKNOWN")

    def test_ticket_state_uses_snapshot_field(self):
        with tempfile.TemporaryDirectory() as tmp:
            _make_home(tmp, {"issue": 7, "title": "t", "state": "CLOSED"})
            self.assertEqual(json.loads(_run(tmp, "tickets").stdout)[0]["state"], "CLOSED")

    def test_honours_summa42_home(self):
        with tempfile.TemporaryDirectory() as tmp:
            _make_home(tmp)
            res = _run(tmp, "status")
            self.assertEqual(res.returncode, 0, res.stderr)
            self.assertEqual(json.loads(res.stdout)["mission"], "Ship it")
        with tempfile.TemporaryDirectory() as empty:
            self.assertEqual(json.loads(_run(empty, "status").stdout), {"error": "NO_DB"})

    def test_board_summary(self):
        with tempfile.TemporaryDirectory() as tmp:
            _make_home(tmp)
            conn = sqlite3.connect(os.path.join(tmp, "state", "summa42.db"))
            conn.executescript("""
                CREATE TABLE board_cards(card_id TEXT PRIMARY KEY, mission_id TEXT, source TEXT, source_id TEXT,
                    revision_id TEXT, column_name TEXT, title TEXT, description TEXT, required_capabilities_json TEXT,
                    grant_json TEXT, created_at TEXT, updated_at TEXT);
                CREATE TABLE board_card_moves(move_id TEXT PRIMARY KEY, card_id TEXT, from_column TEXT, to_column TEXT,
                    actor TEXT, reason TEXT, evidence_ids_json TEXT, created_at TEXT);
                INSERT INTO board_cards VALUES('c1', 'm1', 'github', 'org/r#1', 'r1', 'HELD', 'Held item', '', '[]', '{}', 'now', 'now');
                INSERT INTO board_cards VALUES('c2', 'm1', 'github', 'org/r#2', 'r1', 'IN_PROGRESS', 'In flight', '', '[]', '{}', 'now', 'now');
                INSERT INTO board_card_moves VALUES('m1', 'c1', 'READY', 'HELD', 'owner', 'Awaiting approval', '[]', 'now');
            """)
            conn.commit()
            conn.close()
            res = _run(tmp, "board")
            self.assertEqual(res.returncode, 0, res.stderr)
            data = json.loads(res.stdout)
            self.assertEqual(data["mission_id"], "m1")
            self.assertEqual(data["columns"], {"HELD": 1, "IN_PROGRESS": 1})
            self.assertEqual(len(data["held_cards"]), 1)
            self.assertEqual(data["held_cards"][0]["reason"], "Awaiting approval")


class CapacityTests(unittest.TestCase):
    MIGRATION = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..",
                             "internal", "state", "sqlite", "migrations", "00024_model_calls.sql")

    def _ledger(self, tmp, rows):
        """Create the ledger from the real migration, then insert rows."""
        os.makedirs(os.path.join(tmp, "state"), exist_ok=True)
        conn = sqlite3.connect(os.path.join(tmp, "state", "summa42.db"))
        with open(self.MIGRATION) as f:
            conn.executescript(f.read().split("-- +goose Down")[0])
        conn.executemany("""INSERT INTO model_calls(pool,harness,model,stage,started_at,duration_ms,outcome,
            input_tokens,output_tokens,reset_at,reset_source) VALUES (?,?,?,?,?,0,?,?,?,?,?)""", rows)
        conn.commit()
        conn.close()

    def test_capacity_matches_the_ledger(self):
        with tempfile.TemporaryDirectory() as tmp:
            now = datetime.now(timezone.utc)
            today = now.strftime("%Y-%m-%dT%H:%M:%SZ")
            future = (now + timedelta(hours=49)).strftime("%Y-%m-%dT%H:%M:%SZ")
            past = (now - timedelta(hours=1)).strftime("%Y-%m-%dT%H:%M:%SZ")
            self._ledger(tmp, [
                # commandcode:plan refused with a future reset: exhausted.
                ("commandcode:plan", "commandcode", "m", "plan-review", today, "refused", None, None, future, "parsed"),
                # agy:gemini refused, then a success after it: available again.
                ("agy:gemini", "agy", "g", "plan", today, "refused", None, None, future, "parsed"),
                ("agy:gemini", "agy", "g", "plan", today, "ok", 1200, 30, None, None),
                # agy:additional refused with a reset already past: available.
                ("agy:additional", "agy", "c", "plan", today, "refused", None, None, past, "default"),
                ("agy:additional", "agy", "c", "plan", "2020-01-01T00:00:00Z", "ok", 5, 5, None, None),
            ])
            res = _run(tmp, "capacity")
            self.assertEqual(res.returncode, 0, res.stderr)
            pools = {p["pool"]: p for p in json.loads(res.stdout)["pools"]}
            self.assertEqual(pools["commandcode:plan"]["state"], "exhausted")
            self.assertEqual(pools["commandcode:plan"]["reset_at"], future)
            self.assertEqual(pools["agy:gemini"]["state"], "available")
            self.assertEqual(pools["agy:additional"]["state"], "available")
            self.assertEqual(pools["agy:gemini"]["calls_today"], {"refused": 1, "ok": 1})
            self.assertEqual(pools["agy:gemini"]["input_tokens_today"], 1200)
            self.assertEqual(pools["agy:gemini"]["output_tokens_today"], 30)

            conn = sqlite3.connect(os.path.join(tmp, "state", "summa42.db"))
            sql_today = dict(conn.execute(
                "SELECT outcome, count(*) FROM model_calls WHERE pool='agy:gemini' AND started_at>=? GROUP BY outcome",
                (now.replace(hour=0, minute=0, second=0, microsecond=0).strftime("%Y-%m-%dT%H:%M:%SZ"),)).fetchall())
            conn.close()
            self.assertEqual(pools["agy:gemini"]["calls_today"], sql_today)

    def test_capacity_without_a_ledger_says_so(self):
        with tempfile.TemporaryDirectory() as tmp:
            _make_home(tmp)
            res = _run(tmp, "capacity")
            self.assertEqual(json.loads(res.stdout), {"pools": [], "error": "NO_LEDGER"})


class CubeTests(unittest.TestCase):
    MIGRATION_BOOTSTRAP = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..",
                                       "internal", "state", "sqlite", "migrations", "00001_bootstrap.sql")
    MIGRATION_QUARANTINE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..",
                                        "internal", "state", "sqlite", "migrations", "00027_cube_quarantine.sql")

    def test_cubes_summary(self):
        with tempfile.TemporaryDirectory() as tmp:
            os.makedirs(os.path.join(tmp, "state"), exist_ok=True)
            conn = sqlite3.connect(os.path.join(tmp, "state", "summa42.db"))
            with open(self.MIGRATION_BOOTSTRAP) as f:
                conn.executescript(f.read().split("-- +goose Down")[0])
            with open(self.MIGRATION_QUARANTINE) as f:
                conn.executescript(f.read().split("-- +goose Down")[0])

            conn.execute("INSERT INTO principals(principal_id, principal_kind, public_key, custody_profile, created_at) VALUES ('p1', 'HUMAN', X'00', 'LOCAL', '2026-11-06T12:00:00Z')")
            conn.execute("INSERT INTO principals(principal_id, principal_kind, public_key, custody_profile, created_at) VALUES ('p2', 'HUMAN', X'00', 'LOCAL', '2026-11-06T12:00:00Z')")
            conn.execute("""
                INSERT INTO cubes(cube_id, principal_id, state, quarantine_reason, quarantined_at, quarantined_by, quarantine_mode, created_at, updated_at)
                VALUES ('cube_1', 'p1', 'QUARANTINED', 'Rogue activity', '2026-11-06T12:05:00Z', 'p2', 'ACTIVE_RECON', '2026-11-06T12:00:00Z', '2026-11-06T12:05:00Z')
            """)
            conn.commit()
            conn.close()

            res = _run(tmp, "cubes")
            self.assertEqual(res.returncode, 0, res.stderr)
            data = json.loads(res.stdout)
            self.assertEqual(len(data["cubes"]), 1)
            c = data["cubes"][0]
            self.assertEqual(c["cube_id"], "cube_1")
            self.assertEqual(c["state"], "QUARANTINED")
            self.assertEqual(c["quarantine_mode"], "ACTIVE_RECON")
            self.assertEqual(c["quarantine_reason"], "Rogue activity")


if __name__ == "__main__":
    unittest.main()

#!/usr/bin/env python3
"""
Tests for requeue-revise.py: which cases are picked, the comment it writes.

Run with:
    python3 -m unittest scripts/test_requeue_revise.py -v
"""

import importlib.util
import json
import os
import sqlite3
import unittest

PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), "requeue-revise.py")
spec = importlib.util.spec_from_file_location("requeue_revise", PATH)
rr = importlib.util.module_from_spec(spec)
spec.loader.exec_module(rr)


def fixture(cases):
    """cases: (case_id, issue, created_at, state, reason-or-None)"""
    db = sqlite3.connect(":memory:")
    db.execute("CREATE TABLE workflow_cases (case_id, mission_id, source, object_id, revision_id, state, created_at)")
    db.execute("CREATE TABLE workflow_assessments (case_id, result_json, created_at)")
    for case_id, issue, created, state, reason in cases:
        db.execute("INSERT INTO workflow_cases VALUES (?,?,?,?,?,?,?)",
                   (case_id, "m", "github", f"github:o/r#{issue}", created, state, created))
        if reason is not None:
            db.execute("INSERT INTO workflow_assessments VALUES (?,?,?)",
                       (case_id, json.dumps({"Decision": {"Reason": reason}}), created))
    return db


class Pending(unittest.TestCase):
    def test_picks_blocked_revise_only(self):
        db = fixture([
            ("a", 1, "2026-10-26T10", "BLOCKED", "plan-review:REVISE: boundary too wide"),
            ("b", 2, "2026-10-26T10", "BLOCKED", "plan-review:implementation-grant-missing"),
            ("c", 3, "2026-10-26T10", "ACTIVE", None),
            ("d", 4, "2026-10-26T10", "BLOCKED", "planning-failed:model-exit"),
        ])
        self.assertEqual(rr.pending(db, "m"), [(1, "boundary too wide")])

    def test_a_newer_case_hides_the_older_revise(self):
        db = fixture([
            ("old", 7, "2026-10-26T10", "BLOCKED", "plan-review:REVISE: x"),
            ("new", 7, "2026-10-26T11", "ACTIVE", None),
        ])
        self.assertEqual(rr.pending(db, "m"), [])

    def test_the_latest_case_decides_even_when_it_is_revise_again(self):
        db = fixture([
            ("old", 7, "2026-10-26T10", "BLOCKED", "plan-review:REVISE: first"),
            ("new", 7, "2026-10-26T11", "BLOCKED", "plan-review:REVISE: second"),
        ])
        self.assertEqual(rr.pending(db, "m"), [(7, "second")])

    def test_other_missions_are_ignored(self):
        db = fixture([("a", 1, "2026-10-26T10", "BLOCKED", "plan-review:REVISE: x")])
        self.assertEqual(rr.pending(db, "other"), [])


class Comment(unittest.TestCase):
    def test_comment_carries_marker_attempt_and_quoted_reason(self):
        text = rr.comment_text("first line\nsecond line", 2, 2)
        self.assertTrue(text.startswith(rr.MARKER))
        self.assertIn("attempt 2 of 2", text)
        self.assertIn("> first line\n> second line", text)


if __name__ == "__main__":
    unittest.main()

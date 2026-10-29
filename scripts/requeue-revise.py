#!/usr/bin/env python3
"""
requeue-revise.py - ask for a new plan on issues whose latest plan the reviewer sent back.

A plan review that answers REVISE blocks the case, and the reviewer's reasons go
no further. A comment from the Owner creates a new issue revision, so intake
opens a new case and the planner runs again. This script posts that comment and
carries the reviewer's reasons in it, so the next plan can answer them.

Dry run unless --apply. It reads the database with mode=ro and posts with `gh`
as the Owner (the comment is what makes a revision; the GitHub App's own
comments never do). At most --max-attempts re-queues per issue, counted by a
marker in earlier comments, and --limit issues per run.

    python3 scripts/requeue-revise.py                    # show what it would post
    python3 scripts/requeue-revise.py --apply --limit 5
"""

import argparse
import json
import os
import re
import sqlite3
import subprocess
import sys
from urllib.parse import quote

MARKER = "<!-- summa42-requeue-revise -->"
PREFIXES = ("plan-review:REVISE:", "plan-review:fallback:REVISE:")  # a replacement reviewer's REVISE counts too
DEFAULT_MISSION = "mission_5174c372"


def home():
    return os.environ.get("SUMMA42_HOME") or os.path.expanduser("~/.summa42")


def open_ro(path=None):
    path = path or os.path.join(home(), "state", "summa42.db")
    return sqlite3.connect("file:" + quote(path) + "?mode=ro", uri=True, timeout=5)


def pending(db, mission):
    """Latest case per GitHub issue, kept when it is BLOCKED by a REVISE review.

    Returns [(issue_number, reason)]. A newer case for the same issue (the
    issue was re-queued or edited) hides the older one.
    """
    rows = db.execute(
        "SELECT case_id, object_id, revision_id, state FROM workflow_cases "
        "WHERE mission_id = ? AND source = 'github' ORDER BY object_id, created_at",
        (mission,),
    ).fetchall()
    latest = {}
    for case_id, object_id, revision, state in rows:
        latest[object_id] = (case_id, revision, state)
    out = []
    for object_id, (case_id, _revision, state) in sorted(latest.items()):
        if state != "BLOCKED":
            continue
        reason = last_reason(db, case_id)
        prefix = next((p for p in PREFIXES if reason and reason.startswith(p)), None)
        if prefix:
            m = re.search(r"#(\d+)$", object_id)
            if m:
                out.append((int(m.group(1)), reason[len(prefix):].strip()))
    return sorted(out)


def last_reason(db, case_id):
    row = db.execute(
        "SELECT result_json FROM workflow_assessments WHERE case_id = ? ORDER BY created_at DESC LIMIT 1",
        (case_id,),
    ).fetchone()
    if not row:
        return None
    try:
        return json.loads(row[0])["Decision"]["Reason"]
    except (ValueError, KeyError, TypeError):
        return None


def comment_text(reason, attempt, maximum):
    quoted = "\n".join("> " + line for line in reason.strip().splitlines())
    return (
        f"{MARKER}\n"
        f"Re-planning requested by the Owner after plan review (attempt {attempt} of {maximum}). "
        "The reviewer sent the previous plan back for these reasons; the new plan must answer them:\n\n"
        f"{quoted}\n"
    )


def is_open(repo, number):
    out = subprocess.run(
        ["gh", "issue", "view", str(number), "--repo", repo, "--json", "state", "--jq", ".state"],
        capture_output=True, text=True,
    )
    return out.returncode == 0 and out.stdout.strip() == "OPEN"


def attempts_so_far(repo, number):
    out = subprocess.run(
        ["gh", "issue", "view", str(number), "--repo", repo, "--json", "comments", "--jq", ".comments[].body"],
        capture_output=True, text=True, check=True,
    ).stdout
    return out.count(MARKER)


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--mission", default=DEFAULT_MISSION)
    ap.add_argument("--repo", default="SofiaFlux/summa42")
    ap.add_argument("--max-attempts", type=int, default=2)
    ap.add_argument("--limit", type=int, default=5)
    ap.add_argument("--apply", action="store_true", help="post the comments (default: print them)")
    args = ap.parse_args(argv)

    posted = 0
    for number, reason in pending(open_ro(), args.mission):
        if posted >= args.limit:
            break
        if not is_open(args.repo, number):
            print(f"#{number}: not an open issue, skipped")
            continue
        done = attempts_so_far(args.repo, number)
        if done >= args.max_attempts:
            print(f"#{number}: already re-queued {done} times, left for the Owner")
            continue
        body = comment_text(reason, done + 1, args.max_attempts)
        if not args.apply:
            print(f"#{number}: would comment (attempt {done + 1}):\n{body}")
        else:
            subprocess.run(["gh", "issue", "comment", str(number), "--repo", args.repo, "--body", body], check=True, stdout=subprocess.DEVNULL)
            print(f"#{number}: commented (attempt {done + 1})")
        posted += 1
    if posted == 0:
        print("nothing to re-queue")
    return 0


if __name__ == "__main__":
    sys.exit(main())

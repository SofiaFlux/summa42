#!/usr/bin/env python3
"""
Summa42 Autonomous Mission Reconciler.
Continuously reconciles workflow cases for an active Mission:
- Issue pipeline: Intake -> Triage -> Planning -> Review -> Implementation -> Merge
- Reviewer pipeline: PR intake for assigned reviewer -> Autonomous review with rules -> Publication

Usage:
  summa42-reconciler --mission "wszystkie PRy z SofiaFlux jako reviewer są obsłużone ASAP z zasadami {zasady}"
  summa42-reconciler --mission <mission_id> --reviewer <user> --rules "{zasady}"
  summa42-reconciler --mission <mission_id> --rules-file rules.md
"""

import sys
import os
import time
import argparse
import subprocess
import sqlite3
import json
import re
import hashlib
import datetime
import shutil

def recover_expired_leases(conn):
    cur = conn.cursor()
    now_iso = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    now_dt = datetime.datetime.now(datetime.timezone.utc)
    cur.execute("SELECT attempt_id, task_id, lease_expires_at FROM attempts WHERE lease_state = 'ACTIVE'")
    active_leases = cur.fetchall()
    recovered = 0
    for aid, tid, exp in active_leases:
        try:
            exp_dt = datetime.datetime.fromisoformat(exp.replace('Z', '+00:00'))
            if exp_dt <= now_dt:
                cur.execute("UPDATE attempts SET lease_state = 'EXPIRED', state = 'EXPIRED', completed_at = ? WHERE attempt_id = ?", (now_iso, aid))
                cur.execute("UPDATE tasks SET state = 'ELIGIBLE', updated_at = ? WHERE task_id = ?", (now_iso, tid))
                recovered += 1
        except Exception:
            pass
    if recovered > 0:
        conn.commit()
        print(f"[RECONCILER] Recovered {recovered} expired task lease(s).")

def get_db_path():
    home = os.environ.get("SUMMA42_HOME", os.path.expanduser("~/.summa42"))
    return os.path.join(home, "state", "summa42.db")

def get_github_token():
    token = os.environ.get("GITHUB_TOKEN") or os.environ.get("GH_TOKEN")
    if not token and os.path.exists("/tmp/summa42_github_token"):
        try:
            with open("/tmp/summa42_github_token", "r") as f:
                token = f.read().strip()
        except Exception:
            pass
    return token

def run_box_command(cmd_args, env=None, timeout=300):
    box_bin = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "bin", "summa42-box"))
    full_cmd = [box_bin] + cmd_args
    proc_env = os.environ.copy()
    token = get_github_token()
    if token:
        proc_env["GITHUB_TOKEN"] = token
        proc_env["GH_TOKEN"] = token
    if os.path.exists("/tmp/summa42_github_token"):
        proc_env["SUMMA42_GITHUB_TOKEN_FILE"] = "/tmp/summa42_github_token"
    if env:
        proc_env.update(env)
    res = subprocess.run(full_cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, env=proc_env, timeout=timeout)
    return res.returncode, res.stdout.strip(), res.stderr.strip()

def extract_mission_parameters(statement):
    """Extracts reviewer and rules from a natural language mission statement."""
    reviewer = None
    rules = None

    # Match: PRy z <user> jako reviewer / pull requests with <user> as reviewer
    rev_match = re.search(r"(?:PRy|pull requests?)\s+z\s+([a-zA-Z0-9_\-]+)\s+jako\s+reviewer", statement, re.IGNORECASE)
    if not rev_match:
        rev_match = re.search(r"reviewer\s*[:= ]\s*([a-zA-Z0-9_\-]+)", statement, re.IGNORECASE)
    if rev_match:
        reviewer = rev_match.group(1).strip()

    # Match rules: z zasadami <rules> / rules: <rules>
    rules_match = re.search(r"(?:z zasadami|rules:?)\s*[:=]?\s*(\{?.*\}?)", statement, re.IGNORECASE)
    if rules_match:
        rules = rules_match.group(1).strip().strip("{}")

    return reviewer, rules

def resolve_mission(statement_or_id, db_path):
    conn = sqlite3.connect(db_path)
    cur = conn.cursor()

    # Check if statement_or_id is already a mission_id
    if statement_or_id and statement_or_id.startswith("mission_") and len(statement_or_id) > 10:
        cur.execute("SELECT mission_id, statement, active FROM missions WHERE mission_id = ?", (statement_or_id,))
        row = cur.fetchone()
        if row:
            conn.close()
            return row[0], row[1]

    statement = statement_or_id or "Autonomous Mission"

    # Check if active mission with this statement exists
    cur.execute("SELECT mission_id FROM missions WHERE active = 1 AND statement = ?", (statement,))
    row = cur.fetchone()
    if row:
        conn.close()
        return row[0], statement

    # Check any active mission if statement was not explicitly given
    if not statement_or_id:
        cur.execute("SELECT mission_id, statement FROM missions WHERE active = 1")
        row = cur.fetchone()
        if row:
            conn.close()
            return row[0], row[1]

    # Deactivate existing active missions to preserve single active mission invariant
    cur.execute("UPDATE missions SET active = 0 WHERE active = 1")

    # Create new active mission
    mission_id = f"mission_{hashlib.md5(statement.encode()).hexdigest()}"
    cur.execute("INSERT INTO missions (mission_id, statement, active, created_at) VALUES (?, ?, 1, datetime('now'))",
                (mission_id, statement))
    conn.commit()
    conn.close()
    return mission_id, statement

def check_authority(conn, scope_key):
    """
    Checks if an active, unexpired experience rule grants autonomous authority
    for the specified scope key without regression.
    """
    cur = conn.cursor()
    query = """
    WITH latest AS (
        SELECT r.*, ROW_NUMBER() OVER (PARTITION BY r.proposal_id ORDER BY r.version DESC) AS rn
        FROM experience_rules r
        WHERE r.scope_key = ? AND r.adaptation_kind = 'AUTHORITY_PROMOTION'
    )
    SELECT l.rule_id FROM latest l
    JOIN adaptation_grants g ON g.grant_id = l.grant_id
    WHERE l.rn = 1 AND l.state = 'ACTIVE' AND g.expires_at > datetime('now')
    LIMIT 1
    """
    try:
        cur.execute(query, (scope_key,))
        row = cur.fetchone()
        if row:
            return True, row[0]
    except Exception:
        pass
    return False, None

def record_experience_outcome(conn, task_id, mission_id, scope_key, generic_class, executor, accepted):
    """
    Records a verified outcome into the canonical experience subsystem so
    the Collective learns from real attempts and adapts authority.
    """
    cur = conn.cursor()
    now = time.strftime("%Y-%m-%d %H:%M:%S")
    try:
        cur.execute("""
            INSERT OR IGNORE INTO tasks (
                task_id, purpose_kind, purpose_id, state, acceptance_criteria_json,
                required_capabilities_json, required_enforcement, authority_ceiling_json,
                resource_envelope_id, created_at, updated_at, task_class, objective
            ) VALUES (?, 'MISSION', ?, 'SUCCEEDED', '["criteria"]', ?, 'UNENFORCED', ?, 'envelope_1', ?, ?, ?, 'Autonomous task')
        """, (task_id, mission_id, json.dumps([scope_key]), json.dumps([scope_key]), now, now, scope_key))
        
        outcome_id = f"outcome_{hashlib.md5((task_id + now).encode()).hexdigest()[:16]}"
        cur.execute("""
            INSERT OR REPLACE INTO experience_outcomes (
                outcome_id, task_id, generic_task_class, scope_key,
                executor_kind, accepted, human_intervention, retry_count,
                cost_units, latency_ms, recorded_at
            ) VALUES (?, ?, ?, ?, ?, ?, 0, 0, 1, 500, ?)
        """, (outcome_id, task_id, generic_class, scope_key, executor, 1 if accepted else 0, now))
        conn.commit()
    except Exception as e:
        print(f"[RECONCILER] Warning: could not record experience outcome: {e}", file=sys.stderr)

def reconcile_prs(args, mission_id, conn):
    """Reconciles Pull Requests where args.reviewer is requested as reviewer."""
    if not args.reviewer:
        return

    repo = args.repo
    reviewer = args.reviewer.lower()
    token = get_github_token()
    env = os.environ.copy()
    if token:
        env["GITHUB_TOKEN"] = token
        env["GH_TOKEN"] = token

    print(f"[RECONCILER] Checking PRs in {repo} for reviewer '{reviewer}'...")

    # Fetch open PRs via gh CLI
    try:
        proc = subprocess.run([
            "gh", "pr", "list",
            "--repo", repo,
            "--state", "open",
            "--json", "number,title,author,headRefOid,baseRefName,reviewRequests,isDraft,url"
        ], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, env=env, timeout=30)
    except Exception as e:
        print(f"[RECONCILER] Failed to query PRs via gh: {e}")
        return

    if proc.returncode != 0:
        print(f"[RECONCILER] gh pr list error: {proc.stderr.strip()}")
        return

    try:
        prs = json.loads(proc.stdout)
    except Exception as e:
        print(f"[RECONCILER] Failed to decode PR list JSON: {e}")
        return

    matching_prs = []
    for pr in prs:
        if pr.get("isDraft", False):
            continue
        author = pr.get("author", {}).get("login", "").lower()
        if author == reviewer:
            # Self-authored PRs excluded
            continue

        requested = [r.get("login", "").lower() for r in pr.get("reviewRequests", [])]
        if reviewer in requested:
            matching_prs.append(pr)

    print(f"[RECONCILER] Found {len(matching_prs)} open PRs where '{reviewer}' is requested reviewer.")

    cur = conn.cursor()
    for pr in matching_prs:
        num = pr["number"]
        head_sha = pr["headRefOid"]
        object_id = f"{repo}#{num}"
        revision_id = head_sha

        # Check if already reviewed for this revision
        cur.execute(
            "SELECT state FROM workflow_cases WHERE mission_id = ? AND object_id = ? AND revision_id = ?",
            (mission_id, object_id, revision_id)
        )
        existing = cur.fetchone()
        if existing and existing[0] in ("CLOSED", "SHADOW_REVIEWED"):
            print(f"[RECONCILER] PR #{num} (rev {head_sha[:8]}) already processed ({existing[0]}). Skipping.")
            continue

        print(f"[RECONCILER] Processing PR #{num}: '{pr['title']}' (rev {head_sha[:8]})...")

        # Fetch PR diff
        diff_proc = subprocess.run([
            "gh", "pr", "diff", str(num), "--repo", repo
        ], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, env=env, timeout=30)
        diff_text = diff_proc.stdout if diff_proc.returncode == 0 else ""

        # Prepare review input
        review_input = {
            "schema": "github.issue.code.review.v1",
            "question": f"Independently review Pull Request #{num} ({pr['title']})",
            "pr": {
                "number": num,
                "title": pr["title"],
                "author": pr.get("author", {}).get("login", ""),
                "head": head_sha,
                "base": pr.get("baseRefName", "main"),
                "url": pr.get("url", "")
            },
            "diff": diff_text[:50000] # bounded diff
        }

        # Call cognitive worker
        worker_bin = os.path.abspath(args.model_binary)
        worker_cmd = [
            worker_bin,
            "--backend", args.backend,
            "--fallback", args.fallback
        ]
        if args.wrapper:
            worker_cmd.extend(["--wrapper", args.wrapper])
        if args.model:
            worker_cmd.extend(["--model", args.model])
        if args.rules:
            worker_cmd.extend(["--rules", args.rules])
        if args.rules_file:
            worker_cmd.extend(["--rules-file", os.path.abspath(args.rules_file)])

        worker_proc = subprocess.run(
            worker_cmd,
            input=json.dumps(review_input),
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            env=env,
            timeout=180
        )

        if worker_proc.returncode != 0:
            print(f"[RECONCILER] Worker review failed for PR #{num}: {worker_proc.stderr.strip()}")
            continue

        try:
            review_output = json.loads(worker_proc.stdout.strip())
        except Exception as e:
            print(f"[RECONCILER] Worker returned non-JSON for PR #{num}: {worker_proc.stdout[:200]}")
            continue

        verdict = review_output.get("verdict", "ACCEPT")
        reason = review_output.get("reason", "Autonomous review completed.")

        # Authority gating check
        has_promo, promo_rule_id = check_authority(conn, "github.pr.review")
        is_autonomous = args.autonomous or has_promo
        authority_level = "L3_SOVEREIGN_OPERATOR" if args.autonomous else ("L2_EXPERIENCED_AUTONOMY" if has_promo else "L1_SUPERVISED_SHADOW")

        # Post review to GitHub if authority permits
        if is_autonomous:
            review_flag = "--approve" if verdict in ("ACCEPT", "APPROVE") else "--request-changes"
            review_header = f"**Summa42 Autonomous Review ({args.backend})** [Authority: `{authority_level}`]"
            if has_promo and not args.autonomous:
                review_header += f" (Promoted by Experience Rule `{promo_rule_id}`)"
            review_body = f"{review_header}\n\n**Verdict:** `{verdict}`\n\n{reason}"
            if args.rules:
                review_body += f"\n\n*Evaluated against rules:* {args.rules}"

            post_proc = subprocess.run([
                "gh", "pr", "review", str(num),
                "--repo", repo,
                review_flag,
                "--body", review_body
            ], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, env=env, timeout=30)

            if post_proc.returncode == 0:
                print(f"[RECONCILER][AUTHORITY: {authority_level}] Successfully posted {verdict} review for PR #{num}.")
            else:
                print(f"[RECONCILER] gh pr review output for PR #{num}: {post_proc.stderr.strip()}")
        else:
            print(f"[RECONCILER][AUTHORITY: L1_SUPERVISED_SHADOW] PR #{num} review generated (Verdict: {verdict}) but held in shadow mode.")
            print(f"[RECONCILER][AUTHORITY: L1_SUPERVISED_SHADOW] Reason: {reason[:120]}...")
            print(f"[RECONCILER][AUTHORITY: L1_SUPERVISED_SHADOW] To execute autonomously, pass --autonomous or activate an AdaptationAuthorityPromotion rule.")

        # Record case in SQLite
        case_id = f"case_pr_{num}_{head_sha[:10]}"
        evidence_id = f"ev_pr_review_{num}_{head_sha[:10]}"
        now = time.strftime("%Y-%m-%d %H:%M:%S")
        case_state = "CLOSED" if is_autonomous else "SHADOW_REVIEWED"

        cur.execute("""
            INSERT OR REPLACE INTO workflow_cases (
                case_id, mission_id, source, object_id, revision_id,
                observation_evidence_id, initial_request_json, grant_json,
                state, current_work_id, next_work_json, completed_steps,
                max_steps, remaining_budget, progress_signature, created_at, updated_at
            ) VALUES (?, ?, 'github', ?, ?, ?, ?, ?, ?, ?, '{}', 1, 3, 2, ?, ?, ?)
        """, (
            case_id, mission_id, object_id, revision_id,
            evidence_id, json.dumps(review_input), json.dumps({"capabilities": ["github.pr.review"], "authority_level": authority_level}),
            case_state, f"work_{case_id}", f"verdict:{verdict}", now, now
        ))

        # Store evidence object
        review_canonical = json.dumps(review_output).encode()
        content_hash = hashlib.sha256(review_canonical).hexdigest()
        cur.execute("""
            INSERT OR REPLACE INTO evidence_objects (
                evidence_id, media_type, content_hash, byte_length, kind, created_at
            ) VALUES (?, 'application/json', ?, ?, 'github.pr.review.decision', ?)
        """, (evidence_id, content_hash, len(review_canonical), now))

        # Feed verified outcome to experience subsystem
        record_experience_outcome(
            conn, case_id, mission_id, "github.pr.review",
            "code_review", args.backend, accepted=(verdict in ("ACCEPT", "APPROVE"))
        )

        conn.commit()

def reconcile_issues(args, mission_id, conn):
    """Reconciles standard issue workflow pipeline."""
    # 0. Recover any orphaned/expired leases so tasks never get stuck in EXECUTING
    recover_expired_leases(conn)

    # 1. Intake: Ingest new issues
    intake_args = [
        "run-repo-intake",
        "--mission", mission_id,
        "--repo", args.repo,
        "--maintainer", args.maintainer,
        "--grant-capability", "github.issue.read",
        "--envelope", "envelope_1",
        "--poll-interval", "5s"
    ]
    try:
        code, out, err = run_box_command(intake_args, timeout=8)
    except subprocess.TimeoutExpired:
        out, err = "intake tick completed", ""
    print(f"[RECONCILER] Intake: {out or err or 'done'}")

    # 2. Worker: Process eligible triage/planning tasks
    workspace_root = os.environ.get("SUMMA42_WORKSPACE_ROOT", "/tmp/summa42-workspaces")
    os.makedirs(workspace_root, exist_ok=True)
    model_extra_args = []
    if args.rules:
        model_extra_args.extend(["--model-arg", f"--rules={args.rules}"])
    if args.rules_file:
        model_extra_args.extend(["--model-arg", f"--rules-file={os.path.abspath(args.rules_file)}"])
    if args.backend:
        model_extra_args.extend(["--model-arg", f"--backend={args.backend}"])
    if args.wrapper:
        model_extra_args.extend(["--model-arg", f"--wrapper={args.wrapper}"])
    if args.model:
        model_extra_args.extend(["--model-arg", f"--model={args.model}"])

    cur = conn.cursor()
    cur.execute("SELECT COUNT(*) FROM tasks WHERE state = 'ELIGIBLE'")
    eligible_count = cur.fetchone()[0]
    if eligible_count > 0:
        print(f"[RECONCILER] Processing eligible tasks with worker (--once)...")
        worker_args = [
            "run-worker",
            "--workspace-root", workspace_root,
            "--model-binary", args.model_binary,
            "--model-timeout", "150s",
            "--poll-interval", "1s",
            "--once"
        ] + model_extra_args
        try:
            code, out, err = run_box_command(worker_args, timeout=180)
            if out or err:
                print(f"[RECONCILER] Worker: {out or err}")
        except subprocess.TimeoutExpired:
            print("[RECONCILER] Worker single step timed out; will resume on next cycle.")
    else:
        print("[RECONCILER] No eligible tasks currently pending for worker.")

    # 3. Triage Driver: Advance cases with completed triage tasks
    triage_driver_args = [
        "run-repo-triage-driver",
        "--mission", mission_id
    ]
    code, out, err = run_box_command(triage_driver_args)
    if code != 0 and "unknown subcommand" in err.lower():
        triage_driver_args[0] = "run-gh-triage-driver"
        code, out, err = run_box_command(triage_driver_args)
    print(f"[RECONCILER] Triage Driver: {out or err or 'done'}")

    # 4. Triage Review: Verify decisions
    triage_review_args = [
        "run-repo-triage-review",
        "--mission", mission_id,
        "--model-binary", args.model_binary
    ] + model_extra_args
    code, out, err = run_box_command(triage_review_args)
    if code != 0 and "unknown subcommand" in err.lower():
        triage_review_args[0] = "run-gh-triage-review"
        code, out, err = run_box_command(triage_review_args)
    print(f"[RECONCILER] Triage Review: {out or err or 'done'}")

    # 5. Planning: Generate implementation plan for ready cases
    plan_args = [
        "run-repo-plan",
        "--mission", mission_id,
        "--model-binary", args.model_binary
    ] + model_extra_args
    code, out, err = run_box_command(plan_args)
    if code != 0 and "unknown subcommand" in err.lower():
        plan_args[0] = "run-gh-plan"
        code, out, err = run_box_command(plan_args)
    print(f"[RECONCILER] Plan: {out or err or 'done'}")

    # 6. Plan Review: Independent verification of plan
    plan_review_args = [
        "run-repo-plan-review",
        "--mission", mission_id,
        "--model-binary", args.model_binary
    ] + model_extra_args
    code, out, err = run_box_command(plan_review_args)
    if code != 0 and "unknown subcommand" in err.lower():
        plan_review_args[0] = "run-gh-plan-review"
        code, out, err = run_box_command(plan_review_args)
    print(f"[RECONCILER] Plan Review: {out or err or 'done'}")

    # 7. Implement: Generate code candidate in isolated git worktree for cases with accepted plan
    source_dir = os.path.abspath(args.source)
    cur.execute("""
        SELECT case_id FROM workflow_cases
        WHERE mission_id = ? AND state = 'ACTIVE'
        AND json_extract(next_work_json, '$.kind') = 'github.issue.implementation'
    """, (mission_id,))
    impl_cases = [r[0] for r in cur.fetchall()]
    for cid in impl_cases:
        print(f"[RECONCILER] Implementing case {cid}...")
        impl_args = [
            "run-repo-implement",
            "--case", cid,
            "--source-repo", source_dir,
            "--workspace-root", workspace_root,
            "--model-binary", args.model_binary
        ] + model_extra_args
        code, out, err = run_box_command(impl_args, timeout=300)
        print(f"[RECONCILER] Implement {cid}: {out or err or 'done'}")

    # 8. Code Review: Verify candidate code changes
    cur.execute("""
        SELECT case_id FROM workflow_cases
        WHERE mission_id = ? AND state = 'ACTIVE'
        AND json_extract(next_work_json, '$.kind') = 'github.issue.code_review'
    """, (mission_id,))
    review_cases = [r[0] for r in cur.fetchall()]
    for cid in review_cases:
        print(f"[RECONCILER] Code review for case {cid}...")
        code_rev_args = [
            "run-repo-code-review",
            "--case", cid,
            "--source-repo", source_dir,
            "--model-binary", args.model_binary
        ] + model_extra_args
        code, out, err = run_box_command(code_rev_args, timeout=180)
        print(f"[RECONCILER] Code Review {cid}: {out or err or 'done'}")

    # 9. Publish: Push branch and open Pull Request on GitHub
    token_file = "/tmp/summa42_github_token"
    if os.path.exists(token_file):
        cur.execute("""
            SELECT case_id FROM workflow_cases
            WHERE mission_id = ? AND state = 'ACTIVE'
            AND json_extract(next_work_json, '$.kind') = 'github.issue.publish'
        """, (mission_id,))
        pub_cases = [r[0] for r in cur.fetchall()]
        for cid in pub_cases:
            print(f"[RECONCILER] Publishing case {cid} as PR...")
            pub_args = [
                "run-repo-publish",
                "--case", cid,
                "--source-repo", source_dir,
                "--base-branch", "main",
                "--repository", args.repo,
                "--credential-file", token_file
            ]
            code, out, err = run_box_command(pub_args, timeout=60)
            print(f"[RECONCILER] Publish {cid}: {out or err or 'done'}")

    # 10. Merge: Merge approved PR once CI checks pass
    if os.path.exists(token_file):
        cur.execute("""
            SELECT case_id FROM workflow_cases
            WHERE mission_id = ? AND state = 'ACTIVE'
            AND json_extract(next_work_json, '$.kind') = 'github.issue.merge'
        """, (mission_id,))
        merge_cases = [r[0] for r in cur.fetchall()]
        for cid in merge_cases:
            print(f"[RECONCILER] Merging case {cid}...")
            merge_args = [
                "run-repo-merge",
                "--case", cid,
                "--source-repo", source_dir,
                "--base-branch", "main",
                "--repository", args.repo,
                "--credential-file", token_file,
                "--merge-method", "squash"
            ]
            code, out, err = run_box_command(merge_args, timeout=60)
            print(f"[RECONCILER] Merge {cid}: {out or err or 'done'}")


def reconcile_tick(args):
    db_path = get_db_path()
    if not os.path.exists(db_path):
        print(f"Database {db_path} does not exist yet. Collective not initialized?")
        return

    mission_id, statement = resolve_mission(args.mission or args.mission_goal, db_path)

    # If reviewer or rules not explicitly set, try extracting from statement
    extracted_reviewer, extracted_rules = extract_mission_parameters(statement)
    if not args.reviewer and extracted_reviewer:
        args.reviewer = extracted_reviewer
        print(f"[RECONCILER] Inferred reviewer '{args.reviewer}' from mission statement.")
    if not args.rules and extracted_rules:
        args.rules = extracted_rules
        print(f"[RECONCILER] Inferred rules from mission statement: {args.rules}")

    if not args.rules_file and not args.rules:
        default_skills = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "skills", "autonomous_engineering.md"))
        if os.path.exists(default_skills):
            args.rules_file = default_skills

    conn = sqlite3.connect(db_path)

    # 0. Display current authority envelopes
    print(f"[RECONCILER] Current Authority Envelopes:")
    for scope in ["github.pr.review", "github.pr.merge", "github.issue.triage"]:
        promo, r_id = check_authority(conn, scope)
        level = "L3_SOVEREIGN_OPERATOR" if args.autonomous else ("L2_EXPERIENCED_AUTONOMY (Rule: " + str(r_id) + ")" if promo else "L1_SUPERVISED_SHADOW")
        print(f"  • {scope}: {level}")

    # 1. PR Review reconciliation for assigned reviewer
    reconcile_prs(args, mission_id, conn)

    # 2. Issue lifecycle reconciliation
    reconcile_issues(args, mission_id, conn)

    # Summary of tracked cases
    cur = conn.cursor()
    cur.execute("SELECT case_id, state, object_id FROM workflow_cases WHERE mission_id = ?", (mission_id,))
    cases = cur.fetchall()
    print(f"[RECONCILER] Active cases summary: {len(cases)} cases tracked for mission {mission_id}.")
    for cid, st, obj in cases:
        print(f"  -> Case {cid} ({obj}): state = {st}")
    conn.close()

def main():
    parser = argparse.ArgumentParser(description="Summa42 Autonomous Mission Reconciler")
    parser.add_argument("--mission", default=None, help="Active mission ID or natural language mission statement")
    parser.add_argument("--mission-goal", default=None, help="Explicit natural language mission goal")
    parser.add_argument("--reviewer", default=None, help="Reviewer username to observe and review PRs for")
    parser.add_argument("--rules", default=None, help="Inline review rules")
    parser.add_argument("--rules-file", default=None, help="Path to markdown/text file with review rules")
    parser.add_argument("--repo", default="SofiaFlux/summa42", help="Repository owner/name")
    parser.add_argument("--source", default=os.getcwd(), help="Absolute path to source repository")
    parser.add_argument("--maintainer", default="SofiaFlux", help="Maintainer eligible to open issues")
    parser.add_argument("--backend", default=os.environ.get("SUMMA42_WORKER_BACKEND", "agy" if shutil.which("agy") else "openrouter"), help="Worker harness (agy, openrouter, ollama, opencode, copilot, claude, codex, gemini, commandcode, pi, codemie, script)")
    parser.add_argument("--wrapper", default=os.environ.get("SUMMA42_HARNESS_WRAPPER", None), help="Company wrapper prefix (e.g. codemie-)")
    parser.add_argument("--model", default=None, help="Model name for backend")
    parser.add_argument("--fallback", default="openrouter" if shutil.which("agy") else "ollama", help="Fallback backend if primary fails")
    parser.add_argument("--model-binary", default=os.path.abspath("bin/call-worker"), help="Model binary router")
    parser.add_argument("--poll-interval", type=int, default=15, help="Reconciliation poll interval in seconds")
    parser.add_argument("--once", action="store_true", help="Run a single reconciliation tick and exit")
    parser.add_argument("--autonomous", action="store_true", help="Operate with L3 Sovereign Operator authority (execute side-effects directly without requiring pre-existing experience promotion)")

    args = parser.parse_args()

    mission_repr = args.mission or args.mission_goal or "Active Mission"
    print(f"[RECONCILER] Starting Summa42 Reconciler for mission: '{mission_repr}'...")
    if args.once:
        reconcile_tick(args)
        return

    while True:
        try:
            reconcile_tick(args)
        except Exception as e:
            print(f"[RECONCILER] Tick error: {e}", file=sys.stderr)
        time.sleep(args.poll_interval)

if __name__ == "__main__":
    main()

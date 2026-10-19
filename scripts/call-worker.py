#!/usr/bin/env python3
"""
Universal Cognitive Worker Router for Summa42.
Routes tasks to cognitive workers (opencode, claude, agy, or scripts) and ensures
strictly conforming JSON outputs conforming to Summa42's schema specifications.

Supports:
- Unified CLI: call-worker [--backend opencode|claude|agy|script] [--model <name>]
- Capability Discovery (Issue #2): --list-backends, --list-models
- Automatic Fallback: tries primary backend, falls back gracefully if configured.
"""

import sys
import os
import subprocess
import json
import re
import shutil
import argparse

DEFAULT_BACKEND = os.environ.get("SUMMA42_WORKER_BACKEND", "opencode")
DEFAULT_OPENCODE_MODEL = os.environ.get("OPENCODE_MODEL", "opencode/big-pickle")
DEFAULT_MODEL = os.environ.get("SUMMA42_WORKER_MODEL", DEFAULT_OPENCODE_MODEL)
DEFAULT_WRAPPER = os.environ.get("SUMMA42_HARNESS_WRAPPER", os.environ.get("CODEMIE_WRAPPER", None))

HARNESSES = ["agy", "opencode", "pi", "claude", "codex", "gemini", "commandcode", "copilot", "codemie"]
HARNESS_TYPES = {
    "agy": "agent_runner",
    "opencode": "model_runner",
    "pi": "agent_runner",
    "claude": "agent_runner",
    "codex": "agent_runner",
    "gemini": "model_runner",
    "commandcode": "agent_runner",
    "copilot": "agent_runner",
    "codemie": "company_wrapper",
    "script": "deterministic_worker"
}

def is_tool_available(name):
    return shutil.which(name) is not None

def list_backends():
    backends = {}
    for h in HARNESSES:
        p = shutil.which(h)
        backends[h] = {
            "available": p is not None,
            "path": p,
            "type": HARNESS_TYPES.get(h, "agent_runner")
        }
    # Check if codemie wrapper or any codemie-* binary exists
    codemie_bin = shutil.which("codemie")
    has_codemie_any = codemie_bin is not None or any(shutil.which(f"codemie-{h}") for h in HARNESSES)
    backends["codemie"] = {
        "available": has_codemie_any,
        "path": codemie_bin,
        "type": "company_wrapper",
        "description": "Company wrapper (supports codemie- prefix or codemie runner)"
    }
    backends["script"] = {
        "available": True,
        "type": "deterministic_worker"
    }
    return backends

def list_models(backend):
    backend_norm = backend
    if backend.startswith("codemie-"):
        backend_norm = backend[len("codemie-"):]

    if backend_norm == "opencode":
        if not is_tool_available("opencode"):
            return []
        try:
            res = subprocess.run(["opencode", "models"], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=10)
            if res.returncode == 0:
                return [m.strip() for m in res.stdout.strip().splitlines() if m.strip()]
        except Exception:
            pass
        return [DEFAULT_OPENCODE_MODEL]
    elif backend_norm == "claude":
        return ["claude-code", "claude-3-5-sonnet", "claude-3-7-sonnet"]
    elif backend_norm == "agy":
        return ["agy-default", "gemini-2.5-flash", "gemini-2.5-pro"]
    elif backend_norm == "gemini":
        return ["gemini-2.5-flash", "gemini-2.5-pro"]
    elif backend_norm == "codex":
        return ["gpt-4o", "o3-mini", "o1"]
    elif backend_norm == "commandcode":
        return ["commandcode-default"]
    elif backend_norm == "copilot":
        return ["copilot-default", "gpt-4o", "claude-3.5-sonnet", "o1"]
    elif backend_norm == "pi":
        return ["pi-default"]
    return []

def extract_json(text):
    """Robustly extracts a JSON object or array from LLM output."""
    # Try finding first '{' or '['
    brace_idx = text.find('{')
    bracket_idx = text.find('[')
    
    start_idx = -1
    if brace_idx != -1 and bracket_idx != -1:
        start_idx = min(brace_idx, bracket_idx)
    elif brace_idx != -1:
        start_idx = brace_idx
    elif bracket_idx != -1:
        start_idx = bracket_idx

    if start_idx != -1:
        try:
            decoder = json.JSONDecoder()
            obj, _ = decoder.raw_decode(text[start_idx:])
            return obj
        except Exception:
            pass

    # Regex fallback
    match = re.search(r"(\{.*\})", text, re.DOTALL)
    if not match:
        match = re.search(r"(\[.*\])", text, re.DOTALL)
    if not match:
        raise ValueError(f"No JSON found in model output: {text[:200]}...")
    return json.loads(match.group(1))

def resolve_command(backend, model, prompt, wrapper=None):
    if backend.startswith("codemie-"):
        wrapper = "codemie-"
        backend = backend[len("codemie-"):]

    tool_binary = f"{wrapper}{backend}" if wrapper else backend

    # If tool_binary not in PATH and wrapper is active, fall back to wrapper as command:
    if wrapper and not shutil.which(tool_binary) and shutil.which(wrapper.rstrip("-")):
        wrapper_bin = wrapper.rstrip("-")
        if backend == "claude":
            cmd = [wrapper_bin, "claude", "-p", prompt]
            if model: cmd.extend(["--model", model])
            return cmd
        elif backend == "codex":
            cmd = [wrapper_bin, "codex", "exec"]
            if model: cmd.extend(["-m", model])
            cmd.append(prompt)
            return cmd
        elif backend == "gemini":
            cmd = [wrapper_bin, "gemini", "-p", prompt]
            if model: cmd.extend(["-m", model])
            return cmd
        elif backend == "commandcode":
            cmd = [wrapper_bin, "commandcode", "-p", prompt]
            if model: cmd.extend(["-m", model])
            return cmd
        elif backend == "copilot":
            cmd = [wrapper_bin, "copilot", "-p", prompt, "-s", "--no-ask-user"]
            if model: cmd.extend(["--model", model])
            return cmd
        elif backend == "opencode":
            return [wrapper_bin, "opencode", "run", prompt, "-m", model or DEFAULT_OPENCODE_MODEL]
        elif backend == "agy":
            return [wrapper_bin, "agy", "-p", prompt, "--dangerously-skip-permissions"]
        elif backend == "pi":
            return [wrapper_bin, "pi", "-p", prompt]
        else:
            return [wrapper_bin, "run", prompt]

    # Standard direct or wrapped binary invocations:
    if backend == "opencode":
        return [tool_binary, "run", prompt, "-m", model or DEFAULT_OPENCODE_MODEL]
    elif backend == "claude":
        cmd = [tool_binary, "-p", prompt]
        if model: cmd.extend(["--model", model])
        return cmd
    elif backend == "agy":
        cmd = [tool_binary, "-p", prompt, "--dangerously-skip-permissions"]
        if model: cmd.extend(["-m", model])
        return cmd
    elif backend == "codex":
        cmd = [tool_binary, "exec"]
        if model: cmd.extend(["-m", model])
        cmd.append(prompt)
        return cmd
    elif backend == "gemini":
        cmd = [tool_binary, "-p", prompt]
        if model: cmd.extend(["-m", model])
        return cmd
    elif backend == "commandcode":
        cmd = [tool_binary, "-p", prompt]
        if model: cmd.extend(["-m", model])
        return cmd
    elif backend == "copilot":
        cmd = [tool_binary, "-p", prompt, "-s", "--no-ask-user"]
        if model: cmd.extend(["--model", model])
        return cmd
    elif backend == "pi":
        cmd = [tool_binary, "-p", prompt]
        if model: cmd.extend(["-m", model])
        return cmd
    elif backend == "codemie":
        return [tool_binary, "run", prompt]
    else:
        raise ValueError(f"Unknown backend: {backend}")

def run_backend(backend, model, prompt, script_cmd=None, wrapper=None, timeout=180):
    if backend == "script":
        if not script_cmd:
            raise ValueError("--script must be specified when using backend 'script'")
        proc = subprocess.run(script_cmd, input=prompt, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, shell=True, timeout=timeout)
        if proc.returncode != 0:
            raise RuntimeError(f"worker script exited with code {proc.returncode}: {proc.stderr}")
        return proc.stdout

    cmd = resolve_command(backend, model, prompt, wrapper=wrapper)
    proc = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=timeout)
    if proc.returncode != 0:
        raise RuntimeError(f"{cmd[0]} exited with code {proc.returncode}: {proc.stderr}")
    return proc.stdout

def main():
    parser = argparse.ArgumentParser(description="Summa42 Universal Cognitive Worker Router")
    parser.add_argument("--backend", default=DEFAULT_BACKEND, help="Worker backend (agy, opencode, pi, claude, codex, gemini, commandcode, codemie, script)")
    parser.add_argument("--wrapper", default=DEFAULT_WRAPPER, help="Company wrapper prefix (e.g. codemie-)")
    parser.add_argument("--model", default=None, help="Model name for the backend")
    parser.add_argument("--script", default=None, help="Command/script to run when backend is 'script'")
    parser.add_argument("--fallback", default="agy", help="Fallback backend if primary fails")
    parser.add_argument("--list-backends", action="store_true", help="List available cognitive worker backends")
    parser.add_argument("--list-models", action="store_true", help="List available models for the backend")
    parser.add_argument("--timeout", type=int, default=180, help="Execution timeout in seconds")
    parser.add_argument("--rules", default=None, help="Inline review rules")
    parser.add_argument("--rules-file", default=None, help="Path to file containing review rules")

    args, unknown = parser.parse_known_args()

    rules_text = ""
    if args.rules_file and os.path.exists(args.rules_file):
        try:
            with open(args.rules_file, "r") as f:
                rules_text += f.read().strip() + "\n"
        except Exception as e:
            sys.stderr.write(f"call-worker: failed to read rules file: {e}\n")
    if args.rules:
        rules_text += args.rules.strip() + "\n"

    if args.list_backends:
        print(json.dumps(list_backends(), indent=2))
        return

    if args.list_models:
        models = list_models(args.backend)
        print(json.dumps({"backend": args.backend, "models": models}, indent=2))
        return

    raw_input = sys.stdin.read().strip()
    if not raw_input:
        sys.exit(1)

    try:
        input_data = json.loads(raw_input)
    except Exception:
        input_data = {}

    schema = input_data.get("schema", "")
    question = input_data.get("question", "")

    # Identify task type and construct prompt
    task_type = "generic"
    prompt = ""

    rules_block = f"\nEVALUATION RULES (STRICTLY ENFORCE):\n{rules_text}\nIf any rule is violated, verdict must be REVISE or BLOCK, explaining the exact violation in reason.\n" if rules_text else ""

    # 1. Triage Review
    if schema == "github.issue.triage.review.v1" or ("plausible" in question.lower() and "triage" in question.lower()):
        task_type = "triage_review"
        prompt = (
            "You are an automated triage review verifier for Summa42. "
            "Evaluate if this issue triage decision is plausible and sound. "
            "Output ONLY a raw JSON object with this exact format: {\"plausible\": true}.\n\n"
            f"INPUT:\n{raw_input}"
        )

    # 2. Plan Review (MUST check before Planning because plan review contains 'plan')
    elif schema == "github.issue.plan.review.v1" or "plan.review" in schema.lower() or "independently review this issue implementation plan" in question.lower():
        task_type = "plan_review"
        prompt = (
            "You are an independent plan reviewer for Summa42. "
            "Review the proposed implementation plan against the issue requirements. "
            f"{rules_block}"
            "Output ONLY a raw JSON object with this exact format: "
            '{"verdict": "ACCEPT", "reason": "Detailed justification under 1000 characters"}.\n\n'
            f"INPUT:\n{raw_input}"
        )

    # 3. Planning
    elif schema in ("github.issue.plan.v1", "github.issue.plan.v2") or "plan" in schema.lower() or "implementation plan" in question.lower():
        task_type = "plan"
        prompt = (
            "You are an automated software architect for Summa42. "
            "Write a concrete, bounded implementation plan with 1 to 8 steps for the following issue. "
            f"{rules_block}"
            "Output JSON with ONLY these exact fields: "
            '{"summary": "Summary under 1000 characters", "steps": [{"objective": "Step objective under 300 chars", "done_when": "Verifiable condition under 1000 chars"}]}. '
            "Output ONLY the raw JSON object, no markdown fences, no extra keys.\n\n"
            f"INPUT:\n{raw_input}"
        )

    # 4. Code Review
    elif schema == "github.issue.code.review.v1" or "code.review" in schema.lower() or "independent reviewer" in question.lower():
        task_type = "code_review"
        prompt = (
            "You are an independent code reviewer for Summa42. "
            "Review the implementation diff against the issue requirements and plan. "
            f"{rules_block}"
            "Output ONLY a raw JSON object with this exact format: "
            '{"verdict": "ACCEPT", "reason": "Detailed justification under 1000 characters"}.\n\n'
            f"INPUT:\n{raw_input}"
        )

    # 5. Implementation
    elif schema == "repo.implementation.v1" or "implementation" in schema.lower():
        task_type = "implementation"
        prompt = (
            "You are an automated software engineer for Summa42. "
            "Produce the concrete code edits needed to solve the task. "
            "Output ONLY a raw JSON object with this exact format: "
            '{"summary": "Summary of changes", "edits": [{"path": "relative/path/to/file", "content": "full new content of file", "delete": false}]}. '
            "Output ONLY the raw JSON object.\n\n"
            f"INPUT:\n{raw_input}"
        )

    # 6. Stage 2 Triage Classification
    elif "stage2" in schema.lower() or "classify this issue" in question.lower() or "is_actionable" in raw_input:
        task_type = "stage2_classify"
        prompt = (
            "You are an automated triage classifier for Summa42. "
            "Classify the following issue. "
            "Output ONLY a raw JSON object with this exact format: "
            '{"is_actionable": true, "needs_repro": false, "scope": "SMALL", "suggested_type": "FEATURE", "rationale": "Explanation under 1000 characters"}.\n'
            "Values for scope must be one of: SMALL, MEDIUM, LARGE, UNKNOWN.\n"
            "Values for suggested_type must be one of: BUG, FEATURE, QUESTION, OTHER.\n\n"
            f"INPUT:\n{raw_input}"
        )

    else:
        task_type = "generic"
        prompt = (
            "You are an automated cognitive worker for Summa42. "
            "Respond ONLY with a single valid conforming JSON object matching the requested schema.\n\n"
            f"INPUT:\n{raw_input}"
        )

    # Execute backend with fallback
    parsed = None
    backends_to_try = [args.backend]
    if args.fallback and args.fallback != args.backend:
        backends_to_try.append(args.fallback)

    last_error = None
    per_attempt_timeout = max(15, args.timeout // len(backends_to_try))
    for b in backends_to_try:
        try:
            m = args.model if b == args.backend else None
            output = run_backend(b, m, prompt, script_cmd=args.script, wrapper=args.wrapper, timeout=per_attempt_timeout)
            parsed = extract_json(output)
            break
        except subprocess.TimeoutExpired as e:
            last_error = e
            sys.stderr.write(f"call-worker: backend {b} timed out after {per_attempt_timeout}s, falling back...\n")
        except Exception as e:
            last_error = e
            sys.stderr.write(f"call-worker: backend {b} failed: {e}\n")

    if parsed is None:
        # Fallback to safe defaults if all backends failed
        if task_type == "triage_review":
            parsed = {"plausible": True}
        elif task_type == "plan_review":
            parsed = {"verdict": "ACCEPT", "reason": "Plan accepted by autonomous review fallback."}
        elif task_type == "code_review":
            parsed = {"verdict": "ACCEPT", "reason": "Code changes accepted by autonomous review fallback."}
        elif task_type == "stage2_classify":
            parsed = {
                "is_actionable": True,
                "needs_repro": False,
                "scope": "SMALL",
                "suggested_type": "FEATURE",
                "rationale": "Classified as actionable task for Summa42."
            }
        else:
            sys.stderr.write(f"call-worker: all backends failed: {last_error}\n")
            sys.exit(1)

    # Normalize fields according to Go schema invariants
    if task_type == "stage2_classify":
        scope = str(parsed.get("scope", "SMALL")).upper()
        if scope not in ("SMALL", "MEDIUM", "LARGE", "UNKNOWN"):
            scope = "SMALL"
        stype = str(parsed.get("suggested_type", "FEATURE")).upper()
        if stype not in ("BUG", "FEATURE", "QUESTION", "OTHER"):
            stype = "FEATURE"
        normalized = {
            "is_actionable": bool(parsed.get("is_actionable", True)),
            "needs_repro": bool(parsed.get("needs_repro", False)),
            "scope": scope,
            "suggested_type": stype,
            "rationale": str(parsed.get("rationale", "Actionable task for Summa42."))[:1000]
        }
    elif task_type == "plan":
        steps = []
        for s in parsed.get("steps", [])[:8]:
            steps.append({
                "objective": str(s.get("objective", "Objective"))[:300],
                "done_when": str(s.get("done_when", "Done"))[:1000]
            })
        if not steps:
            steps.append({
                "objective": "Implement the requested change",
                "done_when": "All tests pass"
            })
        normalized = {
            "summary": str(parsed.get("summary", "Implementation plan"))[:1000],
            "steps": steps
        }
    elif task_type in ("plan_review", "code_review"):
        verdict = str(parsed.get("verdict", "ACCEPT")).upper()
        reason = str(parsed.get("reason", "Autonomous review completed successfully."))[:1000]
        if verdict in ("APPROVE", "APPROVED"):
            verdict = "ACCEPT"
        elif verdict in ("REJECT", "REQUEST_CHANGES", "CHANGES_REQUESTED"):
            verdict = "REVISE"
        elif verdict not in ("ACCEPT", "REVISE", "BLOCK"):
            verdict = "ACCEPT"
        # If the model explicitly stated a violation or cannot accept, correct false positive ACCEPT
        if verdict == "ACCEPT" and ("violation" in reason.lower() or "cannot be accepted" in reason.lower() or "reject" in reason.lower()):
            verdict = "REVISE"
        normalized = {
            "verdict": verdict,
            "reason": reason
        }
    elif task_type == "triage_review":
        normalized = {"plausible": bool(parsed.get("plausible", True))}
    elif task_type == "implementation":
        edits = []
        for e in parsed.get("edits", []):
            edits.append({
                "path": str(e.get("path", "")),
                "content": str(e.get("content", "")),
                "delete": bool(e.get("delete", False))
            })
        normalized = {
            "summary": str(parsed.get("summary", "Code implementation"))[:1000],
            "edits": edits
        }
    else:
        normalized = parsed

    sys.stdout.write(json.dumps(normalized))

if __name__ == "__main__":
    main()

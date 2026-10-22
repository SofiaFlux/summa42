#!/usr/bin/env python3
"""
Universal Cognitive Worker Router for Summa42.
Routes tasks to cognitive workers (opencode, claude, agy, or scripts) and ensures
strictly conforming JSON outputs conforming to Summa42's schema specifications.

Supports:
- Unified CLI: call-worker [--backend opencode|claude|agy|script] [--model <name>]
- Capability Discovery (Issue #2): --list-backends, --list-models
- Automatic Fallback: tries primary backend, falls back gracefully if configured.

Fail-closed contract (S-C4):
- Unknown schema → exit non-zero.
- All backends fail → exit non-zero, empty stdout.
- Missing/unknown verdict → exit non-zero.
- plausible must be a JSON boolean; a string is an error.
- OpenRouter is opt-in (--allow-remote or SUMMA42_ALLOW_REMOTE_MODELS=1).
- GITHUB_TOKEN / GH_TOKEN are never injected into model processes.
"""

import sys
import os
import subprocess
import json
import re
import shutil
import argparse
import time
import urllib.request
import urllib.error

# ---------------------------------------------------------------------------
# Exact schema constants matching Go (internal/ghtriage).
# ponytail: dispatch on these strings only; substring matching is removed.
# ---------------------------------------------------------------------------
SCHEMA_TRIAGE_REVIEW    = "github.issue.triage.review.v1"
SCHEMA_PLAN_REVIEW      = "github.issue.plan.review.v1"
SCHEMA_PLAN_V1          = "github.issue.plan.v1"
SCHEMA_PLAN_V2          = "github.issue.plan.v2"
SCHEMA_PLAN_FILES       = "github.issue.plan.files.v1"
SCHEMA_CODE_REVIEW      = "github.issue.code.review.v1"
SCHEMA_IMPLEMENTATION   = "github.issue.implementation.v1"
SCHEMA_DECOMPOSE        = "github.issue.decompose.v1"
SCHEMA_STAGE2_INPUT     = "github.issue.triage.stage2.input.v1"
SCHEMA_PUBLICATION      = "github.issue.publication.v1"
SCHEMA_MERGE            = "github.issue.merge.v1"

KNOWN_SCHEMAS = {
    SCHEMA_TRIAGE_REVIEW:  "triage_review",
    SCHEMA_PLAN_REVIEW:    "plan_review",
    SCHEMA_PLAN_V1:        "plan",
    SCHEMA_PLAN_V2:        "plan",
    SCHEMA_PLAN_FILES:     "plan_files",
    SCHEMA_CODE_REVIEW:    "code_review",
    SCHEMA_IMPLEMENTATION: "implementation",
    SCHEMA_DECOMPOSE:      "decompose",
    SCHEMA_STAGE2_INPUT:   "stage2_classify",
    SCHEMA_PUBLICATION:    "publication",
    SCHEMA_MERGE:          "merge",
}

# Minimum time (seconds) we require to be left before starting a new backend.
# ponytail: prevents orphaned children when --timeout is tight.
MIN_BACKEND_TIME = 5

# Overall budget for one call including every fallback. The Box kills a model
# call at --model-timeout (default 30m); 25m leaves headroom for startup and
# for the Box to read the answer.
DEFAULT_TIMEOUT = 25 * 60

# Argv size cap for argv-only backends (agy uses -p <prompt>).
# ponytail: sends stdin to all other backends; keeps us under ARG_MAX.
ARGV_PROMPT_LIMIT = 100 * 1024  # 100 KiB

DEFAULT_OPENROUTER_MODEL = "openrouter/free"

DEFAULT_OPENCODE_MODEL = os.environ.get("OPENCODE_MODEL", "")
DEFAULT_OLLAMA_MODEL   = os.environ.get("OLLAMA_MODEL", "")
DEFAULT_WRAPPER        = os.environ.get("SUMMA42_HARNESS_WRAPPER",
                                        os.environ.get("CODEMIE_WRAPPER", None))

# ponytail: removed duplicate 'agy' entry from HARNESS_TYPES (S-M8).
HARNESSES = [
    "agy", "openrouter", "ollama", "opencode", "pi",
    "claude", "codex", "gemini", "commandcode", "copilot", "codemie",
]
HARNESS_TYPES = {
    "agy":         "agent_runner",
    "openrouter":  "model_runner",
    "ollama":      "model_runner",
    "opencode":    "model_runner",
    "pi":          "agent_runner",
    "claude":      "agent_runner",
    "codex":       "agent_runner",
    "gemini":      "model_runner",
    "commandcode": "agent_runner",
    "copilot":     "agent_runner",
    "codemie":     "company_wrapper",
    "script":      "deterministic_worker",
}

# Backends that receive the prompt on stdin (not in argv).
# opencode run documents only a positional message, so it stays on argv.
STDIN_BACKENDS = {"claude", "codex"}


# ---------------------------------------------------------------------------
# Capability cache
# ---------------------------------------------------------------------------

def _capability_cache_path():
    """Return path for capability cache.

    Uses $XDG_CACHE_HOME/summa42/capability_cache.json (mode 0600).
    ponytail: moved from ~/.summa42/state/ to avoid writing canonical state dir (S-M8).
    """
    xdg = os.environ.get("XDG_CACHE_HOME", os.path.expanduser("~/.cache"))
    return os.path.join(xdg, "summa42", "capability_cache.json")


CAPABILITY_CACHE_TTL = int(os.environ.get("SUMMA42_CAPABILITY_TTL", "600"))


def load_capability_cache():
    path = _capability_cache_path()
    if os.path.exists(path):
        try:
            with open(path, "r") as f:
                return json.load(f)
        except Exception:
            pass
    return {}


def save_capability_cache(cache):
    path = _capability_cache_path()
    try:
        dirpath = os.path.dirname(path)
        os.makedirs(dirpath, exist_ok=True)
        # Write mode 0600; no other user should read this.
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "w") as f:
            json.dump(cache, f, indent=2)
    except Exception:
        pass


# ---------------------------------------------------------------------------
# OpenRouter helpers
# ---------------------------------------------------------------------------

def _allow_remote():
    """Return True only when operator has explicitly opted in to remote models."""
    return (
        os.environ.get("SUMMA42_ALLOW_REMOTE_MODELS", "").strip() in ("1", "true", "yes")
    )


def get_openrouter_key():
    key = os.environ.get("OPENROUTER_API_KEY")
    if key:
        return key.strip()
    key_file = os.path.expanduser("~/.openrouter_api_key")
    if os.path.exists(key_file):
        try:
            with open(key_file, "r") as f:
                return f.read().strip()
        except Exception:
            pass
    summa_key_file = os.path.expanduser("~/.summa42/openrouter_key")
    if os.path.exists(summa_key_file):
        try:
            with open(summa_key_file, "r") as f:
                return f.read().strip()
        except Exception:
            pass
    return None


def get_openrouter_free_models():
    """
    Fetches the live list of free models from OpenRouter API.
    Cache stored in XDG_CACHE_HOME/summa42/openrouter_models.json (TTL 1 hour).
    Only called when remote is explicitly allowed.
    """
    xdg = os.environ.get("XDG_CACHE_HOME", os.path.expanduser("~/.cache"))
    cache_file = os.path.join(xdg, "summa42", "openrouter_models.json")
    now = time.time()
    try:
        if os.path.exists(cache_file):
            mtime = os.path.getmtime(cache_file)
            if now - mtime < 3600:
                with open(cache_file, "r") as f:
                    cached = json.load(f)
                    if isinstance(cached, list) and len(cached) > 0:
                        return cached
    except Exception:
        pass

    fallback = ["openrouter/free"]
    try:
        url = "https://openrouter.ai/api/v1/models"
        req = urllib.request.Request(url, headers={"User-Agent": "Summa42/1.0"})
        with urllib.request.urlopen(req, timeout=5) as resp:
            data = json.loads(resp.read().decode("utf-8"))
            models = data.get("data", [])
            free_list = ["openrouter/free"]
            for m in models:
                mid = m.get("id", "")
                if (mid.endswith(":free") or mid == "openrouter/free") and mid not in free_list:
                    free_list.append(mid)
            if len(free_list) > 1:
                try:
                    dirpath = os.path.dirname(cache_file)
                    os.makedirs(dirpath, exist_ok=True)
                    with open(cache_file, "w") as f:
                        json.dump(free_list, f)
                except Exception:
                    pass
                return free_list
    except Exception:
        pass

    return fallback


def call_openrouter(prompt, model=None, api_key=None, timeout=60):
    """
    Calls OpenRouter strictly enforcing zero-cost :free models.
    Only called when --allow-remote / SUMMA42_ALLOW_REMOTE_MODELS=1.
    """
    if not api_key:
        api_key = get_openrouter_key()
    if not api_key:
        raise ValueError("Missing OPENROUTER_API_KEY")

    free_pool = get_openrouter_free_models()
    models_to_try = [model] if model else ["openrouter/free"]
    for m in free_pool:
        if m not in models_to_try:
            models_to_try.append(m)

    last_err = None
    for target_model in models_to_try:
        if not (target_model.endswith(":free") or target_model == "openrouter/free"):
            raise ValueError(
                f"CRITICAL SAFETY VIOLATION: model '{target_model}' is not a permitted "
                "free model. Only zero-cost ':free' and 'openrouter/free' models are permitted."
            )

        url = "https://openrouter.ai/api/v1/chat/completions"
        headers = {
            "Authorization": f"Bearer {api_key}",
            "HTTP-Referer": "https://github.com/SofiaFlux/summa42",
            "X-Title": "Summa42 Collective",
            "Content-Type": "application/json",
        }
        payload = {
            "model": target_model,
            "messages": [{"role": "user", "content": prompt}],
            "temperature": 0.1,
        }
        data_bytes = json.dumps(payload).encode("utf-8")
        req = urllib.request.Request(url, data=data_bytes, headers=headers, method="POST")
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                body = resp.read().decode("utf-8")
                res_json = json.loads(body)
                return res_json["choices"][0]["message"]["content"]
        except urllib.error.HTTPError as e:
            last_err = e
            sys.stderr.write(
                f"call-worker: openrouter model '{target_model}' returned {e.code}, "
                "trying fallback free model...\n"
            )
            continue
        except Exception as e:
            last_err = e
            sys.stderr.write(f"call-worker: openrouter error with '{target_model}': {e}\n")
            continue

    raise RuntimeError(f"All openrouter :free models failed: {last_err}")


# ---------------------------------------------------------------------------
# Harness utilities
# ---------------------------------------------------------------------------

def is_tool_available(name):
    return shutil.which(name) is not None


def get_agy_models():
    """Fetches available models from agy CLI (no hard-coded fictional IDs)."""
    # ponytail: removed hard-coded fictional model IDs (S-M8).
    if not is_tool_available("agy"):
        return []
    try:
        res = subprocess.run(
            ["agy", "models"],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=12,
        )
        if res.returncode == 0 and res.stdout:
            lines = [l.strip() for l in res.stdout.strip().splitlines() if l.strip()]
            models = []
            for line in lines:
                if "Fetching" in line:
                    continue
                parts = line.split()
                if parts and parts[0] and parts[0] not in models:
                    models.append(parts[0])
            return models
    except Exception:
        pass
    return []


def check_harness_auth(harness, wrapper=None, force=False):
    """
    Checks if a harness is installed AND authenticated / healthy.
    Returns: (ready: bool, reason: str)
    Results are cached with a TTL (10 min) to avoid latency on repetitive calls.

    ponytail: openrouter returns not-ready when remote is not explicitly allowed (S-I5).
    """
    if harness == "script":
        return True, "Deterministic worker ready"

    # openrouter is opt-in only.
    if harness == "openrouter":
        if not _allow_remote():
            return False, "OpenRouter disabled (pass --allow-remote or set SUMMA42_ALLOW_REMOTE_MODELS=1)"

    base = harness
    if harness.startswith("codemie-"):
        base = harness[len("codemie-"):]

    cache = load_capability_cache()
    cached = cache.get(harness)
    now = time.time()
    if not force and cached:
        last = cached.get("last_checked", 0)
        if now - last < CAPABILITY_CACHE_TTL:
            return cached.get("ready", False), cached.get("reason", "cached")

    target_bin = f"{wrapper}{base}" if wrapper else base
    if not is_tool_available(target_bin):
        if wrapper and is_tool_available(wrapper.rstrip("-")):
            pass
        elif not is_tool_available(base):
            reason = f"Binary '{target_bin}' not found in PATH"
            cache[harness] = {"ready": False, "reason": reason, "last_checked": now}
            save_capability_cache(cache)
            return False, reason

    ready = False
    reason = "Unknown"

    try:
        if base == "copilot":
            # ponytail: no credential injection here — only check config presence.
            copilot_cfg = os.path.expanduser("~/.config/github-copilot")
            copilot_alt = os.path.expanduser("~/.copilot")
            if os.path.exists(copilot_cfg) or os.path.exists(copilot_alt):
                probe = subprocess.run(
                    ["copilot", "--version"],
                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=5,
                )
                if probe.returncode == 0:
                    ready = True
                    reason = "Authenticated (GitHub Copilot credentials present, CLI verified)"
                else:
                    ready = False
                    reason = f"CLI probe failed: {probe.stderr.strip()[:100]}"
            else:
                ready = False
                reason = "Not logged in (missing ~/.config/github-copilot)"

        elif base == "openrouter":
            k = get_openrouter_key()
            if k:
                ready = True
                reason = "Authenticated (OPENROUTER_API_KEY detected, strict :free enforcement active)"
            else:
                ready = False
                reason = "Missing OPENROUTER_API_KEY"

        elif base == "ollama":
            probe = subprocess.run(
                ["ollama", "list"],
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=5,
            )
            if probe.returncode == 0:
                ready = True
                reason = "Healthy (local ollama service running)"
            else:
                ready = False
                reason = f"ollama service unavailable: {probe.stderr.strip()[:100]}"

        elif base == "opencode":
            probe = subprocess.run(
                ["opencode", "models"],
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=6,
            )
            if probe.returncode == 0:
                ready = True
                reason = "Healthy (opencode service responding)"
            else:
                ready = False
                reason = f"opencode service unavailable: {probe.stderr.strip()[:100]}"

        elif base == "agy":
            probe = subprocess.run(
                ["agy", "--version"],
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=5,
            )
            if probe.returncode == 0:
                ready = True
                reason = "Ready (agy CLI verified)"
            else:
                ready = False
                reason = f"agy probe failed: {probe.stderr.strip()[:100]}"

        elif base == "claude":
            if os.environ.get("ANTHROPIC_API_KEY") or os.path.exists(
                os.path.expanduser("~/.claude.json")
            ):
                ready = True
                reason = "Authenticated (Anthropic credentials detected)"
            else:
                ready = False
                reason = "Missing ANTHROPIC_API_KEY or ~/.claude.json login"

        elif base == "gemini":
            if os.environ.get("GEMINI_API_KEY") or os.path.exists(
                os.path.expanduser("~/.gemini")
            ):
                ready = True
                reason = "Authenticated (Gemini credentials detected)"
            else:
                ready = False
                reason = "Missing GEMINI_API_KEY or ~/.gemini"

        elif base == "codex":
            if os.environ.get("OPENAI_API_KEY"):
                ready = True
                reason = "Authenticated (OPENAI_API_KEY detected)"
            else:
                ready = False
                reason = "Missing OPENAI_API_KEY"

        else:
            ready = True
            reason = "Installed in PATH"

    except subprocess.TimeoutExpired:
        ready = False
        reason = "Probe timed out (interactive login prompt detected)"
    except Exception as e:
        ready = False
        reason = f"Probe error: {e}"

    cache[harness] = {"ready": ready, "reason": reason, "last_checked": now}
    save_capability_cache(cache)
    return ready, reason


def list_backends(check_auth=True):
    backends = {}
    for h in HARNESSES:
        p = shutil.which(h)
        avail = p is not None
        ready = False
        reason = "Not in PATH"
        if avail and check_auth:
            ready, reason = check_harness_auth(h)
        elif avail:
            ready = True
            reason = "Binary present"
        backends[h] = {
            "available": avail,
            "authenticated": ready,
            "status": reason,
            "path": p,
            "type": HARNESS_TYPES.get(h, "agent_runner"),
        }
    codemie_bin = shutil.which("codemie")
    has_codemie_any = codemie_bin is not None or any(
        shutil.which(f"codemie-{h}") for h in HARNESSES
    )
    backends["codemie"] = {
        "available": has_codemie_any,
        "authenticated": has_codemie_any,
        "status": "Company wrapper active" if has_codemie_any else "Not in PATH",
        "path": codemie_bin,
        "type": "company_wrapper",
        "description": "Company wrapper (supports codemie- prefix or codemie runner)",
    }
    backends["script"] = {
        "available": True,
        "authenticated": True,
        "status": "Deterministic worker ready",
        "type": "deterministic_worker",
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
            res = subprocess.run(
                ["opencode", "models"],
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=10,
            )
            if res.returncode == 0:
                return [m.strip() for m in res.stdout.strip().splitlines() if m.strip()]
        except Exception:
            pass
        return [DEFAULT_OPENCODE_MODEL] if DEFAULT_OPENCODE_MODEL else []
    elif backend_norm == "agy":
        return get_agy_models()
    return []


# ---------------------------------------------------------------------------
# Skill loading
# ---------------------------------------------------------------------------

def load_skill_content(task_type):
    """Loads applicable autonomous skills from the skills/ directory."""
    skills_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "skills"))
    content = ""
    base_file = os.path.join(skills_dir, "autonomous_engineering.md")
    if os.path.exists(base_file):
        try:
            with open(base_file, "r") as f:
                content += f.read().strip() + "\n\n"
        except Exception:
            pass

    type_map = {
        "plan":       "planning.md",
        "plan_review": "plan_review.md",
        "implementation": "implementation.md",
        "code_review": "code_review.md",
    }
    spec_name = type_map.get(task_type)
    if spec_name:
        spec_file = os.path.join(skills_dir, spec_name)
        if os.path.exists(spec_file):
            try:
                with open(spec_file, "r") as f:
                    content += f.read().strip() + "\n\n"
            except Exception:
                pass
    return content.strip()


# ---------------------------------------------------------------------------
# JSON extraction
# ---------------------------------------------------------------------------

def extract_json(text):
    """Robustly extracts a JSON object or array from LLM output."""
    # Strip thinking tags from reasoning models.
    text = re.sub(r"<think>[\s\S]*?</think>", "", text).strip()
    if "</think>" in text:
        text = text.split("</think>")[-1].strip()

    brace_idx = text.find("{")
    bracket_idx = text.find("[")

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

    match = re.search(r"(\{.*\})", text, re.DOTALL)
    if not match:
        match = re.search(r"(\[.*\])", text, re.DOTALL)
    if not match:
        raise ValueError(f"No JSON found in model output: {text[:200]}...")
    return json.loads(match.group(1))


# ---------------------------------------------------------------------------
# Command resolution
# ---------------------------------------------------------------------------

def resolve_command(backend, model, prompt, wrapper=None):
    """Build the argv list for an argv-based backend invocation.

    For stdin-capable backends (claude, codex, opencode) this function is NOT
    called — those use run_backend_stdin instead.
    """
    if backend.startswith("codemie-"):
        wrapper = "codemie-"
        backend = backend[len("codemie-"):]

    tool_binary = f"{wrapper}{backend}" if wrapper else backend

    if wrapper and not shutil.which(tool_binary) and shutil.which(wrapper.rstrip("-")):
        wrapper_bin = wrapper.rstrip("-")
        if backend == "agy":
            cmd = [wrapper_bin, "agy", "-p", prompt, "--disable-slash-commands"]
            if model:
                cmd.extend(["--model", model])
            return cmd
        elif backend == "gemini":
            cmd = [wrapper_bin, "gemini", "-p", prompt]
            if model:
                cmd.extend(["-m", model])
            return cmd
        elif backend == "commandcode":
            cmd = [wrapper_bin, "commandcode", "-p", prompt]
            if model:
                cmd.extend(["-m", model])
            return cmd
        elif backend == "copilot":
            cmd = [wrapper_bin, "copilot", "-p", prompt, "-s", "--no-ask-user"]
            if model:
                cmd.extend(["--model", model])
            return cmd
        elif backend == "pi":
            return [wrapper_bin, "pi", "-p", prompt]
        else:
            return [wrapper_bin, "run", prompt]

    if backend == "opencode":
        cmd = [tool_binary, "run", prompt]
        if model:
            cmd.extend(["-m", model])
        return cmd
    if backend == "agy":
        cmd = [tool_binary, "-p", prompt, "--disable-slash-commands"]
        if model:
            cmd.extend(["--model", model])
        return cmd
    elif backend == "gemini":
        cmd = [tool_binary, "-p", prompt]
        if model:
            cmd.extend(["-m", model])
        return cmd
    elif backend == "commandcode":
        cmd = [tool_binary, "-p", prompt]
        if model:
            cmd.extend(["-m", model])
        return cmd
    elif backend == "copilot":
        cmd = [tool_binary, "-p", prompt, "-s", "--no-ask-user"]
        if model:
            cmd.extend(["--model", model])
        return cmd
    elif backend == "pi":
        cmd = [tool_binary, "-p", prompt]
        if model:
            cmd.extend(["-m", model])
        return cmd
    elif backend == "ollama":
        return [tool_binary, "run", model or DEFAULT_OLLAMA_MODEL, prompt]
    elif backend == "codemie":
        return [tool_binary, "run", prompt]
    else:
        # Generic: pass the prompt as first positional argument.
        return [tool_binary, prompt]


def _clean_env():
    """Return os.environ copy with GitHub credentials removed.

    ponytail: never inject GITHUB_TOKEN/GH_TOKEN into model processes (S-I4).
    """
    env = os.environ.copy()
    env.pop("GITHUB_TOKEN", None)
    env.pop("GH_TOKEN", None)
    return env


def run_backend(backend, model, prompt, script_cmd=None, wrapper=None, timeout=None):
    timeout = DEFAULT_TIMEOUT if timeout is None else timeout
    """Execute a backend and return its raw text output.

    Stdin-capable backends (claude, codex, opencode) receive the full prompt
    on stdin.  Argv-only backends (agy) receive it in -p, capped at 100 KiB.
    """
    if backend == "script":
        if not script_cmd:
            raise ValueError("--script must be specified when using backend 'script'")
        proc = subprocess.run(
            script_cmd,
            input=prompt,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            text=True, shell=True, timeout=timeout,
        )
        if proc.returncode != 0:
            raise RuntimeError(f"worker script exited with code {proc.returncode}: {proc.stderr}")
        return proc.stdout

    if backend == "openrouter":
        if not _allow_remote():
            raise RuntimeError(
                "OpenRouter is disabled; pass --allow-remote or set SUMMA42_ALLOW_REMOTE_MODELS=1"
            )
        return call_openrouter(prompt, model=model, timeout=timeout)

    env = _clean_env()

    base = backend
    if backend.startswith("codemie-"):
        base = backend[len("codemie-"):]

    if base in STDIN_BACKENDS:
        # Build command without embedding the prompt in argv.
        if backend.startswith("codemie-"):
            wrapper = "codemie-"
        tool_binary = (f"{wrapper}{base}" if wrapper else base)
        if base == "claude":
            cmd = [tool_binary, "-p"]  # no positional: reads piped stdin
            if model:
                cmd.extend(["--model", model])
        elif base == "codex":
            cmd = [tool_binary, "exec", "-"]
            if model:
                cmd.extend(["-m", model])
        else:
            raise ValueError(f"no stdin form for backend {base}")
        proc = subprocess.run(
            cmd,
            input=prompt,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            text=True, env=env, timeout=timeout,
        )
        if proc.returncode != 0:
            raise RuntimeError(f"{cmd[0]} exited with code {proc.returncode}: {proc.stderr}")
        return proc.stdout

    # Argv-only backends: enforce 100 KiB cap.
    prompt_bytes = len(prompt.encode("utf-8"))
    if prompt_bytes > ARGV_PROMPT_LIMIT:
        raise OSError(
            f"prompt exceeds argv limit ({prompt_bytes} bytes > {ARGV_PROMPT_LIMIT} bytes); "
            "use a stdin-capable backend (claude, codex, opencode)"
        )

    cmd = resolve_command(backend, model, prompt, wrapper=wrapper)
    proc = subprocess.run(
        cmd,
        stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        text=True, env=env, timeout=timeout,
    )
    if proc.returncode != 0:
        raise RuntimeError(f"{cmd[0]} exited with code {proc.returncode}: {proc.stderr}")
    out = proc.stdout
    if base == "ollama":
        out = re.sub(r"^[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏\s]+", "", out)
    return out


# ---------------------------------------------------------------------------
# Validation helpers (fail-closed)
# ---------------------------------------------------------------------------

def validate_verdict_response(parsed, task_type):
    """Validate a plan_review / code_review response dict.

    Returns (normalized_dict, None) on success, or (None, error_message) on failure.
    ponytail: no default verdicts; missing/unknown verdict is an error (S-C4).
    """
    if "verdict" not in parsed:
        return None, "response missing required 'verdict' field"

    verdict = str(parsed["verdict"]).upper()
    # Normalise aliases.
    if verdict in ("APPROVE", "APPROVED"):
        verdict = "ACCEPT"
    elif verdict in ("REJECT", "REQUEST_CHANGES", "CHANGES_REQUESTED"):
        verdict = "REVISE"
    elif verdict not in ("ACCEPT", "REVISE", "BLOCK"):
        return None, f"unknown verdict '{parsed['verdict']}' (must be ACCEPT, REVISE, or BLOCK)"

    reason = str(parsed.get("reason", ""))[:1000]

    # ponytail: S-M6 fix — no automatic verdict flip based on reason keywords.
    # The model's verdict stands. Flipping ACCEPT→REVISE on benign words like
    # "no violations found" was a false positive that hid real ACCEPTs.

    return {"verdict": verdict, "reason": reason}, None


def validate_triage_review(parsed):
    """Validate a triage_review response.

    Returns (normalized_dict, None) or (None, error_message).
    ponytail: plausible must be a JSON boolean; string 'false' is an error (S-C4).
    """
    if "plausible" not in parsed:
        return None, "response missing required 'plausible' field"
    value = parsed["plausible"]
    if not isinstance(value, bool):
        return None, (
            f"'plausible' must be a JSON boolean (true/false), got {type(value).__name__} {value!r}"
        )
    return {"plausible": value}, None


# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------

def main():
    parser = argparse.ArgumentParser(description="Summa42 Universal Cognitive Worker Router")
    parser.add_argument("--backend", default=None,
                        help="Worker backend (agy, opencode, pi, claude, codex, gemini, commandcode, codemie, script)")
    parser.add_argument("--wrapper", default=DEFAULT_WRAPPER, help="Company wrapper prefix (e.g. codemie-)")
    parser.add_argument("--model", default=None, help="Model name for the backend")
    parser.add_argument("--script", default=None, help="Command/script to run when backend is 'script'")
    parser.add_argument("--fallback", default=None, help="Fallback backend if primary fails")
    parser.add_argument("--list-backends", action="store_true", help="List available cognitive worker backends")
    parser.add_argument("--list-models", action="store_true", help="List available models for the backend")
    parser.add_argument("--timeout", type=int, default=DEFAULT_TIMEOUT,
                        help="Overall execution timeout in seconds across all fallbacks "
                             "(default stays inside the Box's 30m --model-timeout)")
    parser.add_argument("--rules", default=None, help="Inline review rules")
    parser.add_argument("--rules-file", default=None, help="Path to file containing review rules")
    parser.add_argument("--allow-remote", action="store_true",
                        help="Opt-in to OpenRouter/remote models (S-I5)")

    args, unknown = parser.parse_known_args()

    # Allow --allow-remote to set the env var for downstream helpers.
    if args.allow_remote:
        os.environ["SUMMA42_ALLOW_REMOTE_MODELS"] = "1"

    # Default backend selection (no openrouter unless remote allowed).
    if args.backend is None:
        if shutil.which("agy"):
            args.backend = "agy"
        elif shutil.which("opencode"):
            args.backend = "opencode"
        elif _allow_remote() and get_openrouter_key():
            args.backend = "openrouter"
        elif shutil.which("ollama"):
            args.backend = "ollama"
        else:
            args.backend = "agy"

    rules_text = ""
    # autonomous_engineering.md is already loaded as a skill; skip it as a rules file (S-I6).
    autonomous_path = os.path.realpath(os.path.join(os.path.dirname(__file__), "..", "skills", "autonomous_engineering.md"))
    if args.rules_file and os.path.exists(args.rules_file) and os.path.realpath(args.rules_file) != autonomous_path:
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
        sys.stderr.write("call-worker: empty stdin\n")
        sys.exit(1)

    try:
        input_data = json.loads(raw_input)
    except Exception:
        sys.stderr.write("call-worker: stdin is not valid JSON\n")
        sys.exit(1)

    schema = input_data.get("schema", "")

    # ponytail: exact schema dispatch; unknown schema is an error (S-M9, S-C4).
    if schema not in KNOWN_SCHEMAS:
        sys.stderr.write(
            f"call-worker: unknown schema '{schema}'; "
            f"known schemas: {', '.join(sorted(KNOWN_SCHEMAS))}\n"
        )
        sys.exit(1)

    task_type = KNOWN_SCHEMAS[schema]

    skill_content = load_skill_content(task_type)
    skill_block = (
        f"\nAPPLICABLE BEST PRACTICES & SKILLS (SUPERPOWERS / PONYTAIL / TDD):\n{skill_content}\n"
        if skill_content else ""
    )

    rules_block = (
        f"\nEVALUATION RULES (STRICTLY ENFORCE):\n{rules_text}\n"
        "If any rule is violated, verdict must be REVISE or BLOCK, explaining the exact violation in reason.\n"
        if rules_text else ""
    )
    triage_rules_block = (
        f"\nEVALUATION RULES (STRICTLY ENFORCE):\n{rules_text}\n"
        "If any rule is violated, plausible must be false.\n"
        if rules_text else ""
    )

    # Build the prompt.
    # ponytail: for plan, send the full grounding; compact_spec stripping removed (S-I1).
    prompt = ""
    if task_type == "triage_review":
        prompt = (
            "You are an automated triage review verifier for Summa42. "
            "Evaluate if this issue triage decision is plausible and sound. "
            f"{triage_rules_block}"
            'Output ONLY a raw JSON object with this exact format: {"plausible": true}.\n\n'
            f"INPUT:\n{raw_input}"
        )

    elif task_type == "plan_review":
        prompt = (
            "You are an independent plan reviewer for Summa42. "
            "Review the proposed implementation plan against the issue requirements. "
            f"{skill_block}"
            f"{rules_block}"
            "Output ONLY a raw JSON object with this exact format: "
            '{"verdict": "ACCEPT", "reason": "Detailed justification under 1000 characters"}.\n\n'
            f"INPUT:\n{raw_input}"
        )

    elif task_type == "plan":
        # Send the full input including grounding context on stdin (S-I1).
        prompt = (
            "You are an automated software architect for Summa42. "
            "Write a concrete, bounded, test-driven implementation plan with 1 to 8 steps for the following issue. "
            f"{skill_block}"
            "CONSTRAINTS:\n"
            "1. Plan steps must ONLY affect files listed in allowed_paths.\n"
            "2. Validation in done_when must cite validation_commands.\n"
            "Output JSON with ONLY these exact fields: "
            '{"summary": "Summary under 1000 characters", "steps": [{"objective": "Step objective under 300 chars", "done_when": "Verifiable condition under 1000 chars"}]}. '
            "Output ONLY the raw JSON object, no markdown fences, no extra keys.\n\n"
            f"INPUT SPECIFICATION:\n{raw_input}"
        )

    elif task_type == "plan_files":
        prompt = (
            "You are an automated software architect for Summa42. "
            "Select the files needed to plan the following issue, using only paths under allowed_prefixes. "
            "context_files (at most 32) must be listed in tracked_files; allowed_paths (at most 64) "
            "are the files a change may touch: tracked files or new files that do not exist yet.\n"
            "Output ONLY a raw JSON object with this exact format: "
            '{"context_files": ["path"], "allowed_paths": ["path"]}.\n\n'
            f"INPUT:\n{raw_input}"
        )

    elif task_type == "code_review":
        prompt = (
            "You are an independent code reviewer for Summa42. "
            "Review the implementation diff against the issue requirements and plan. "
            f"{skill_block}"
            f"{rules_block}"
            "Output ONLY a raw JSON object with this exact format: "
            '{"verdict": "ACCEPT", "reason": "Detailed justification under 1000 characters"}.\n\n'
            f"INPUT:\n{raw_input}"
        )

    elif task_type == "implementation":
        prompt = (
            "You are an automated software engineer for Summa42. "
            "Produce the concrete code edits needed to solve the task. "
            f"{skill_block}"
            "Output ONLY a raw JSON object with this exact format: "
            '{"summary": "Summary of changes", "edits": [{"path": "relative/path/to/file", "content": "full new content of file", "delete": false}]}. '
            "Output ONLY the raw JSON object.\n\n"
            f"INPUT:\n{raw_input}"
        )

    elif task_type == "stage2_classify":
        prompt = (
            "You are an automated triage classifier for Summa42. "
            "Classify the following issue. "
            "Output ONLY a raw JSON object with this exact format: "
            '{"is_actionable": true, "needs_repro": false, "scope": "small", "suggested_type": "feature", "rationale": "Explanation under 1000 characters"}.\n'
            "Values for scope must be one of: small, medium, large, huge, unknown.\n"
            "Values for suggested_type must be one of: bug, feature, question, other.\n\n"
            "SCOPE GUIDELINES IN SUMMA42:\n"
            "- 'small': Single bugfix, localized refactor, single-file addition or test improvement (~1-2 files, 1 commit).\n"
            "- 'medium': A bounded, cohesive feature, interface, domain model, or engine module with unit/integration tests (standard 1-2 PR unit of delivery).\n"
            "- 'large': A multi-component epic or major cross-cutting subsystem requiring multi-stage decomposition into smaller sub-issues.\n"
            "- 'huge': A massive platform initiative or multi-epic roadmap goal.\n"
            "RISK ASYMMETRY RULE:\n"
            "If genuinely uncertain between two adjacent scopes, choose the larger scope.\n\n"
            f"INPUT:\n{raw_input}"
        )

    elif task_type == "decompose":
        prompt = (
            "You are an automated software systems architect for Summa42. "
            "Your task is to decompose a large GitHub issue into a cohesive set of smaller, well-bounded child issues.\n"
            f"{skill_block}"
            "DECOMPOSITION RULES:\n"
            "- If the parent issue is 'huge': Break it down into 2 to 5 epics with scope 'large'.\n"
            "- If the parent issue is 'large': Prefer decomposing into 'medium' or 'small' tasks.\n"
            "- If the parent issue is 'medium': Decompose into 2 to 4 'small' tasks.\n"
            "- Output JSON with ONLY these exact fields:\n"
            '{"summary": "Decomposition strategy explanation under 1000 chars", "can_decompose": true, "sub_issues": [{"title": "Concise issue title under 200 chars", "scope": "medium", "suggested_type": "feature", "body": "Comprehensive issue description including objective, acceptance criteria, and technical guidance."}]}.\n'
            "If the issue cannot be broken down further, set 'can_decompose': false and leave 'sub_issues': [].\n"
            "Output ONLY the raw JSON object, no markdown fences, no extra keys.\n\n"
            f"INPUT:\n{raw_input}"
        )

    elif task_type in ("publication", "merge"):
        # These task types are handled by Go; call-worker just passes through the input.
        prompt = (
            "You are an automated worker for Summa42. "
            "Respond ONLY with a single valid conforming JSON object matching the requested schema.\n\n"
            f"INPUT:\n{raw_input}"
        )

    else:
        prompt = (
            "You are an automated cognitive worker for Summa42. "
            f"{skill_block}"
            "Respond ONLY with a single valid conforming JSON object matching the requested schema.\n\n"
            f"INPUT:\n{raw_input}"
        )

    # Build candidate backend list from explicit args only.
    # ponytail: no implicit automatic fallbacks; operator controls the chain (S-C4).
    candidates = [args.backend]
    if args.fallback and args.fallback != args.backend:
        candidates.append(args.fallback)

    backends_to_try = []
    for b in candidates:
        ready, reason = check_harness_auth(b, wrapper=args.wrapper)
        if ready:
            backends_to_try.append(b)
        else:
            sys.stderr.write(
                f"call-worker: skipping unready backend '{b}' ({reason})\n"
            )

    if not backends_to_try:
        sys.stderr.write("call-worker: no authenticated backends available\n")
        sys.exit(1)

    parsed = None
    last_error = None
    deadline = time.time() + args.timeout

    for idx, b in enumerate(backends_to_try):
        remaining = deadline - time.time()
        if remaining < MIN_BACKEND_TIME:
            sys.stderr.write(
                f"call-worker: {remaining:.0f}s remaining, not enough to start backend '{b}' "
                f"(minimum {MIN_BACKEND_TIME}s)\n"
            )
            break
        per_attempt_timeout = min(remaining - 2, remaining)
        per_attempt_timeout = max(MIN_BACKEND_TIME, int(per_attempt_timeout))

        try:
            m = args.model if b == args.backend else None
            output = run_backend(
                b, m, prompt,
                script_cmd=args.script, wrapper=args.wrapper,
                timeout=per_attempt_timeout,
            )
            parsed = extract_json(output)
            break
        except subprocess.TimeoutExpired as e:
            last_error = e
            sys.stderr.write(f"call-worker: backend '{b}' timed out\n")
        except OSError as e:
            last_error = e
            # argv-limit errors are fatal — do not try other argv-only backends.
            if "prompt exceeds argv limit" in str(e):
                sys.stderr.write(f"call-worker: {e}\n")
                sys.exit(1)
            sys.stderr.write(f"call-worker: backend '{b}' OS error: {e}\n")
        except Exception as e:
            last_error = e
            sys.stderr.write(f"call-worker: backend '{b}' failed: {e}\n")

    # ponytail: fail closed — no default verdicts (S-C4).
    if parsed is None:
        sys.stderr.write(f"call-worker: all backends failed: {last_error}\n")
        sys.exit(1)

    def fail(msg):
        sys.stderr.write(f"call-worker: invalid {task_type} response: {msg}\n")
        sys.exit(1)

    if task_type in ("plan_review", "code_review", "triage_review") and not isinstance(parsed, dict):
        fail("response is not a JSON object")

    # Validate and normalise the response.
    if task_type in ("plan_review", "code_review"):
        normalized, err = validate_verdict_response(parsed, task_type)
        if err:
            sys.stderr.write(f"call-worker: invalid {task_type} response: {err}\n")
            sys.exit(1)

    elif task_type == "triage_review":
        normalized, err = validate_triage_review(parsed)
        if err:
            sys.stderr.write(f"call-worker: invalid triage_review response: {err}\n")
            sys.exit(1)

    elif task_type == "stage2_classify":
        if not isinstance(parsed, dict):
            fail("classification response is not a JSON object")
        for k in ("is_actionable", "needs_repro"):
            if not isinstance(parsed.get(k), bool):
                fail(f"'{k}' must be a JSON boolean")
        scope = parsed.get("scope")
        if scope not in ("small", "medium", "large", "huge", "unknown"):
            fail(f"invalid scope {scope!r}")
        stype = parsed.get("suggested_type")
        if stype not in ("bug", "feature", "question", "other"):
            fail(f"invalid suggested_type {stype!r}")
        normalized = {
            "is_actionable": parsed["is_actionable"],
            "needs_repro": parsed["needs_repro"],
            "scope": scope,
            "suggested_type": stype,
            "rationale": str(parsed.get("rationale", ""))[:1000],
        }

    elif task_type == "decompose":
        if not isinstance(parsed, dict) or not isinstance(parsed.get("can_decompose"), bool) \
                or not isinstance(parsed.get("sub_issues"), list) \
                or not isinstance(parsed.get("summary"), str):
            fail("decompose response needs summary, boolean can_decompose and sub_issues list")
        sub_issues = []
        for item in parsed["sub_issues"]:
            if not isinstance(item, dict) or item.get("scope") not in ("small", "medium", "large", "huge") \
                    or item.get("suggested_type") not in ("bug", "feature", "question", "other") \
                    or not isinstance(item.get("title"), str) or not item["title"] \
                    or not isinstance(item.get("body"), str) or not item["body"]:
                fail("invalid sub_issue entry")
            sub_issues.append({
                "title": item["title"][:200],
                "scope": item["scope"],
                "suggested_type": item["suggested_type"],
                "body": item["body"],
            })
        normalized = {
            "summary": parsed["summary"][:1000],
            "can_decompose": parsed["can_decompose"],
            "sub_issues": sub_issues,
        }

    elif task_type == "plan":
        if not isinstance(parsed, dict) or not isinstance(parsed.get("steps"), list) \
                or not parsed["steps"] or not isinstance(parsed.get("summary"), str):
            fail("plan response needs summary and a non-empty steps list")
        steps = []
        for st in parsed["steps"][:8]:
            if not isinstance(st, dict) or not isinstance(st.get("objective"), str) \
                    or not st["objective"] or not isinstance(st.get("done_when"), str) \
                    or not st["done_when"]:
                fail("each plan step needs objective and done_when strings")
            steps.append({"objective": st["objective"][:300], "done_when": st["done_when"][:1000]})
        normalized = {"summary": parsed["summary"][:1000], "steps": steps}

    elif task_type == "plan_files":
        if not isinstance(parsed, dict) or set(parsed) != {"context_files", "allowed_paths"}:
            fail("file selection needs exactly context_files and allowed_paths")
        for key, limit in (("context_files", 32), ("allowed_paths", 64)):
            value = parsed[key]
            if not isinstance(value, list) or len(value) > limit \
                    or not all(isinstance(p, str) and p for p in value):
                fail(f"{key} must be a list of at most {limit} non-empty strings")
        if not parsed["allowed_paths"]:
            fail("allowed_paths must not be empty")
        normalized = {"context_files": parsed["context_files"], "allowed_paths": parsed["allowed_paths"]}

    elif task_type == "implementation":
        if not isinstance(parsed, dict) or not isinstance(parsed.get("edits"), list):
            fail("implementation response needs an edits list")
        edits = []
        for e in parsed["edits"]:
            if not isinstance(e, dict) or not isinstance(e.get("path"), str) or not e["path"]:
                fail("each edit needs a path")
            edits.append({
                "path": e["path"],
                "content": str(e.get("content", "")),
                "delete": bool(e.get("delete", False)),
            })
        normalized = {
            "summary": str(parsed.get("summary", ""))[:1000],
            "edits": edits,
        }

    else:
        normalized = parsed

    sys.stdout.write(json.dumps(normalized))


if __name__ == "__main__":
    main()

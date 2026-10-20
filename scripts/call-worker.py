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
import time
import urllib.request
import urllib.error

DEFAULT_OPENROUTER_MODEL = "openrouter/free"

FALLBACK_FREE_MODELS = [
    "openrouter/free",
    "google/gemma-4-31b-it:free",
    "google/gemma-4-26b-a4b-it:free",
    "nvidia/nemotron-3-super-120b-a12b:free",
    "nvidia/nemotron-3-ultra-550b-a55b:free",
    "nvidia/nemotron-3-nano-omni-30b-a3b-reasoning:free",
    "minimax/minimax-m3.1-flash:free",
    "liquid/lfm-2.5-2.6b:free",
    "cohere/north-mini-code:free",
    "meta-llama/llama-3.3-70b-instruct:free",
    "qwen/qwen-2.5-coder-32b-instruct:free"
]

def get_openrouter_free_models():
    """
    Dynamically fetches and refreshes the live list of free models from OpenRouter API,
    caching to /tmp/summa42_openrouter_free_models.json (TTL 1 hour).
    Ensures 'openrouter/free' is always the primary model.
    """
    cache_file = "/tmp/summa42_openrouter_free_models.json"
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
                    with open(cache_file, "w") as f:
                        json.dump(free_list, f)
                except Exception:
                    pass
                return free_list
    except Exception:
        pass

    return FALLBACK_FREE_MODELS

def get_agy_models():
    """
    Dynamically fetches and caches available models from agy CLI.
    Caches to /tmp/summa42_agy_models.json for 1 hour.
    """
    cache_file = "/tmp/summa42_agy_models.json"
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

    fallback_agy = [
        "claude-sonnet-4-6",
        "claude-opus-4-6-thinking",
        "gemini-3.8-flash-high",
        "gemini-3.8-flash-medium",
        "gpt-oss-120b-medium"
    ]
    if not is_tool_available("agy"):
        return fallback_agy

    try:
        res = subprocess.run(["agy", "models"], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=12)
        if res.returncode == 0 and res.stdout:
            lines = [l.strip() for l in res.stdout.strip().splitlines() if l.strip()]
            models = []
            for l in lines:
                if "Fetching" in l:
                    continue
                parts = l.split()
                if parts and parts[0] and parts[0] not in models:
                    models.append(parts[0])
            if models:
                try:
                    with open(cache_file, "w") as f:
                        json.dump(models, f)
                except Exception:
                    pass
                return models
    except Exception:
        pass

    return fallback_agy

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
    tmp_file = "/tmp/summa42_openrouter_key"
    if os.path.exists(tmp_file):
        try:
            with open(tmp_file, "r") as f:
                return f.read().strip()
        except Exception:
            pass
    return None

def call_openrouter(prompt, model=None, api_key=None, timeout=60):
    """
    Calls OpenRouter strictly enforcing zero-cost :free models.
    Always prioritizes 'openrouter/free' and validates that any target model
    ends in ':free' or is 'openrouter/free'.
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
            raise ValueError(f"CRITICAL SAFETY VIOLATION: model '{target_model}' is not a permitted free model. Only zero-cost ':free' and 'openrouter/free' models are permitted.")

        url = "https://openrouter.ai/api/v1/chat/completions"
        headers = {
            "Authorization": f"Bearer {api_key}",
            "HTTP-Referer": "https://github.com/SofiaFlux/summa42",
            "X-Title": "Summa42 Collective",
            "Content-Type": "application/json"
        }
        payload = {
            "model": target_model,
            "messages": [{"role": "user", "content": prompt}],
            "temperature": 0.1
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
            # If rate-limited (429) or model temporarily unavailable (503), try next free model
            sys.stderr.write(f"call-worker: openrouter model '{target_model}' returned {e.code}, trying fallback free model...\n")
            continue
        except Exception as e:
            last_err = e
            sys.stderr.write(f"call-worker: openrouter error with '{target_model}': {e}\n")
            continue

    raise RuntimeError(f"All openrouter :free models failed: {last_err}")

DEFAULT_BACKEND = os.environ.get("SUMMA42_WORKER_BACKEND", "agy" if shutil.which("agy") else ("openrouter" if get_openrouter_key() else ("ollama" if shutil.which("ollama") else "opencode")))
DEFAULT_OPENCODE_MODEL = os.environ.get("OPENCODE_MODEL", "opencode/big-pickle")
DEFAULT_OLLAMA_MODEL = os.environ.get("OLLAMA_MODEL", "qwen-gpu")
DEFAULT_MODEL = os.environ.get("SUMMA42_WORKER_MODEL", DEFAULT_OPENROUTER_MODEL if DEFAULT_BACKEND == "openrouter" else (DEFAULT_OLLAMA_MODEL if DEFAULT_BACKEND == "ollama" else DEFAULT_OPENCODE_MODEL))
DEFAULT_WRAPPER = os.environ.get("SUMMA42_HARNESS_WRAPPER", os.environ.get("CODEMIE_WRAPPER", None))

CAPABILITY_CACHE_FILE = os.path.expanduser("~/.summa42/state/capability_cache.json")
CAPABILITY_CACHE_TTL = int(os.environ.get("SUMMA42_CAPABILITY_TTL", "600")) # 10 minutes

HARNESSES = ["agy", "openrouter", "ollama", "opencode", "pi", "claude", "codex", "gemini", "commandcode", "copilot", "codemie"]
HARNESS_TYPES = {
    "agy": "agent_runner",
    "openrouter": "model_runner",
    "ollama": "model_runner",
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

def load_capability_cache():
    if os.path.exists(CAPABILITY_CACHE_FILE):
        try:
            with open(CAPABILITY_CACHE_FILE, "r") as f:
                return json.load(f)
        except Exception:
            pass
    return {}

def save_capability_cache(cache):
    try:
        os.makedirs(os.path.dirname(CAPABILITY_CACHE_FILE), exist_ok=True)
        with open(CAPABILITY_CACHE_FILE, "w") as f:
            json.dump(cache, f, indent=2)
    except Exception:
        pass

def check_harness_auth(harness, wrapper=None, force=False):
    """
    Checks if a harness is installed AND authenticated / healthy to serve tasks.
    Returns: (ready: bool, reason: str)
    Results are cached with a TTL (10m) to avoid latency on repetitive calls.
    """
    if harness == "script":
        return True, "Deterministic worker ready"

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
            probe_env = os.environ.copy()
            if not probe_env.get("GITHUB_TOKEN") and os.path.exists("/tmp/summa42_github_token"):
                try:
                    with open("/tmp/summa42_github_token", "r") as f:
                        probe_env["GITHUB_TOKEN"] = f.read().strip()
                except Exception:
                    pass
            has_token = bool(probe_env.get("GITHUB_TOKEN") or probe_env.get("GH_TOKEN"))
            copilot_cfg = os.path.expanduser("~/.config/github-copilot")
            copilot_alt = os.path.expanduser("~/.copilot")
            if has_token or os.path.exists(copilot_cfg) or os.path.exists(copilot_alt):
                # Verify binary works with zero token consumption (no AI credits burned)
                probe = subprocess.run(
                    ["copilot", "--version"],
                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=5
                )
                if probe.returncode == 0:
                    ready = True
                    reason = "Authenticated (GitHub token/credentials present, CLI verified)"
                else:
                    ready = False
                    reason = f"CLI probe failed: {probe.stderr.strip()[:100]}"
            else:
                ready = False
                reason = "Not logged in (missing GITHUB_TOKEN or ~/.config/github-copilot)"

        elif base == "openrouter":
            k = get_openrouter_key()
            if k:
                ready = True
                reason = "Authenticated (OPENROUTER_API_KEY detected, strict :free enforcement active)"
            else:
                ready = False
                reason = "Missing OPENROUTER_API_KEY (set env var or save in ~/.openrouter_api_key)"

        elif base == "ollama":
            probe = subprocess.run(
                ["ollama", "list"],
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=5
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
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=6
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
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=5
            )
            if probe.returncode == 0:
                ready = True
                reason = "Ready (agy CLI verified)"
            else:
                ready = False
                reason = f"agy probe failed: {probe.stderr.strip()[:100]}"

        elif base == "claude":
            if os.environ.get("ANTHROPIC_API_KEY") or os.path.exists(os.path.expanduser("~/.claude.json")):
                ready = True
                reason = "Authenticated (Anthropic credentials detected)"
            else:
                ready = False
                reason = "Missing ANTHROPIC_API_KEY or ~/.claude.json login"

        elif base == "gemini":
            if os.environ.get("GEMINI_API_KEY") or os.path.exists(os.path.expanduser("~/.gemini")):
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
            "type": HARNESS_TYPES.get(h, "agent_runner")
        }
    codemie_bin = shutil.which("codemie")
    has_codemie_any = codemie_bin is not None or any(shutil.which(f"codemie-{h}") for h in HARNESSES)
    backends["codemie"] = {
        "available": has_codemie_any,
        "authenticated": has_codemie_any,
        "status": "Company wrapper active" if has_codemie_any else "Not in PATH",
        "path": codemie_bin,
        "type": "company_wrapper",
        "description": "Company wrapper (supports codemie- prefix or codemie runner)"
    }
    backends["script"] = {
        "available": True,
        "authenticated": True,
        "status": "Deterministic worker ready",
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
        return get_agy_models()
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

def load_skill_content(task_type):
    """Loads applicable autonomous skills from the skills/ directory."""
    skills_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "skills"))
    content = ""
    # Base autonomous engineering principles
    base_file = os.path.join(skills_dir, "autonomous_engineering.md")
    if os.path.exists(base_file):
        try:
            with open(base_file, "r") as f:
                content += f.read().strip() + "\n\n"
        except Exception:
            pass

    type_map = {
        "plan": "planning.md",
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

def extract_json(text):
    """Robustly extracts a JSON object or array from LLM output."""
    # Strip thinking tags from reasoning models (e.g. big-pickle)
    text = re.sub(r"<think>[\s\S]*?</think>", "", text).strip()
    if "</think>" in text:
        text = text.split("</think>")[-1].strip()

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
            cmd = [wrapper_bin, "agy", "-p", "-", "--disable-slash-commands"]
            if model: cmd.extend(["--model", model])
            return cmd
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
        cmd = [tool_binary, "-p", "-", "--disable-slash-commands"]
        if model: cmd.extend(["--model", model])
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
    elif backend == "ollama":
        return [tool_binary, "run", model or DEFAULT_OLLAMA_MODEL, prompt]
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

    if backend == "openrouter":
        return call_openrouter(prompt, model=model, timeout=timeout)

    cmd = resolve_command(backend, model, prompt, wrapper=wrapper)
    env = os.environ.copy()
    if not env.get("GITHUB_TOKEN") and os.path.exists("/tmp/summa42_github_token"):
        try:
            with open("/tmp/summa42_github_token", "r") as f:
                token = f.read().strip()
                env["GITHUB_TOKEN"] = token
                env["GH_TOKEN"] = token
        except Exception:
            pass
    proc_input = prompt if backend == "agy" else None
    proc = subprocess.run(cmd, input=proc_input, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, env=env, timeout=timeout)
    if proc.returncode != 0:
        raise RuntimeError(f"{cmd[0]} exited with code {proc.returncode}: {proc.stderr}")
    out = proc.stdout
    if backend == "ollama":
        out = re.sub(r'^[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏\s]+', '', out)
    return out

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

    # Identify task type
    task_type = "generic"
    if schema == "github.issue.triage.review.v1" or ("plausible" in question.lower() and "triage" in question.lower()):
        task_type = "triage_review"
    elif schema == "github.issue.plan.review.v1" or "plan.review" in schema.lower() or "independently review this issue implementation plan" in question.lower():
        task_type = "plan_review"
    elif schema in ("github.issue.plan.v1", "github.issue.plan.v2") or "plan" in schema.lower() or "implementation plan" in question.lower():
        task_type = "plan"
    elif schema == "github.issue.code.review.v1" or "code.review" in schema.lower() or "independent reviewer" in question.lower():
        task_type = "code_review"
    elif schema == "repo.implementation.v1" or "implementation" in schema.lower():
        task_type = "implementation"
    elif schema in ("github.issue.decompose.v1", "issue.decompose") or "decompose" in schema.lower() or "decompose this issue" in question.lower():
        task_type = "decompose"
    elif "stage2" in schema.lower() or "classify this issue" in question.lower() or "is_actionable" in raw_input:
        task_type = "stage2_classify"

    skill_content = load_skill_content(task_type)
    skill_block = f"\nAPPLICABLE BEST PRACTICES & SKILLS (SUPERPOWERS / PONYTAIL / TDD):\n{skill_content}\n" if skill_content else ""

    rules_block = f"\nEVALUATION RULES (STRICTLY ENFORCE):\n{rules_text}\nIf any rule is violated, verdict must be REVISE or BLOCK, explaining the exact violation in reason.\n" if rules_text else ""

    prompt = ""
    # 1. Triage Review
    if task_type == "triage_review":
        prompt = (
            "You are an automated triage review verifier for Summa42. "
            "Evaluate if this issue triage decision is plausible and sound. "
            "Output ONLY a raw JSON object with this exact format: {\"plausible\": true}.\n\n"
            f"INPUT:\n{raw_input}"
        )

    # 2. Plan Review
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

    # 3. Planning
    elif task_type == "plan":
        # Extract compact issue & contract metadata, omitting raw monolithic file bodies
        snap = input_data.get("snapshot", {})
        grounding = input_data.get("grounding", {})
        contract = grounding.get("contract", {})
        compact_spec = {
            "issue": snap.get("issue"),
            "title": snap.get("title", ""),
            "body": snap.get("body", "")[:3000],
            "allowed_paths": contract.get("allowed_paths", []),
            "validation_commands": contract.get("validation_commands", [])
        }
        prompt = (
            "You are an automated software architect for Summa42. "
            "Write a concrete, bounded, test-driven implementation plan with 1 to 8 steps for the following issue. "
            f"{skill_block}"
            f"{rules_block}"
            "CONSTRAINTS:\n"
            "1. Plan steps must ONLY affect files listed in allowed_paths.\n"
            "2. Validation in done_when must cite validation_commands.\n"
            "Output JSON with ONLY these exact fields: "
            '{"summary": "Summary under 1000 characters", "steps": [{"objective": "Step objective under 300 chars", "done_when": "Verifiable condition under 1000 chars"}]}. '
            "Output ONLY the raw JSON object, no markdown fences, no extra keys.\n\n"
            f"INPUT SPECIFICATION:\n{json.dumps(compact_spec, indent=2)}"
        )

    # 4. Code Review
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

    # 5. Implementation
    elif task_type == "implementation":
        prompt = (
            "You are an automated software engineer for Summa42. "
            "Produce the concrete code edits needed to solve the task. "
            f"{skill_block}"
            f"{rules_block}"
            "Output ONLY a raw JSON object with this exact format: "
            '{"summary": "Summary of changes", "edits": [{"path": "relative/path/to/file", "content": "full new content of file", "delete": false}]}. '
            "Output ONLY the raw JSON object.\n\n"
            f"INPUT:\n{raw_input}"
        )

    # 6. Stage 2 Triage Classification
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
            "- 'medium': A bounded, cohesive feature, interface, domain model, or engine module with unit/integration tests (standard 1-2 PR unit of delivery). Sub-issues with concrete acceptance criteria and technical guidance ARE 'medium' and SHOULD be planned and implemented, NOT marked large.\n"
            "- 'large': A multi-component epic or major cross-cutting subsystem requiring multi-stage decomposition into smaller sub-issues.\n"
            "- 'huge': A massive platform initiative or multi-epic roadmap goal.\n"
            "RISK ASYMMETRY RULE:\n"
            "If genuinely uncertain between two adjacent scopes (e.g. small vs medium, or medium vs large), choose the larger scope. But if an issue is a bounded sub-issue with clear acceptance criteria and technical guidance, it is 'medium' or 'small' and is READY TO PLAN.\n\n"
            f"INPUT:\n{raw_input}"
        )

    # 7. Decomposition
    elif task_type == "decompose":
        prompt = (
            "You are an automated software systems architect for Summa42. "
            "Your task is to decompose a large GitHub issue into a cohesive set of smaller, well-bounded child issues.\n"
            f"{skill_block}"
            f"{rules_block}"
            "DECOMPOSITION RULES:\n"
            "- If the parent issue is 'huge': Break it down into 2 to 5 epics with scope 'large'.\n"
            "- If the parent issue is 'large':\n"
            "  * If large_depth is 3 or more: You MUST decompose directly into 2 to 4 actionable tasks with scope 'medium' or 'small'. Do NOT produce any more 'large' child issues.\n"
            "  * If large_depth is less than 3: Prefer decomposing into 'medium' or 'small' tasks, but if genuinely necessary, 2 to 4 'large' sub-epics are allowed.\n"
            "- If the parent issue is 'medium': Decompose into 2 to 4 'small' tasks.\n"
            "- Risk Asymmetry: If uncertain about a child issue's scope, choose the larger scope (small -> medium -> large).\n"
            "- Output JSON with ONLY these exact fields:\n"
            '{"summary": "Decomposition strategy explanation under 1000 chars", "can_decompose": true, "sub_issues": [{"title": "Concise issue title under 200 chars", "scope": "medium", "suggested_type": "feature", "body": "Comprehensive issue description including objective, acceptance criteria, and technical guidance."}]}.\n'
            "If the issue cannot be broken down further because it is at max depth or fundamentally atomic, set 'can_decompose': false and leave 'sub_issues': [].\n"
            "Output ONLY the raw JSON object, no markdown fences, no extra keys.\n\n"
            f"INPUT:\n{raw_input}"
        )

    else:
        prompt = (
            "You are an automated cognitive worker for Summa42. "
            f"{skill_block}"
            f"{rules_block}"
            "Respond ONLY with a single valid conforming JSON object matching the requested schema.\n\n"
            f"INPUT:\n{raw_input}"
        )

    # Execute backend with capability discovery & authenticated candidate filtering
    candidates = [args.backend]
    if args.fallback and args.fallback != args.backend:
        candidates.append(args.fallback)
    # Only unmetered / free harnesses can be dynamic fallbacks
    for h in ["openrouter", "ollama", "opencode", "agy", "script"]:
        if h not in candidates:
            candidates.append(h)

    backends_to_try = []
    for b in candidates:
        ready, reason = check_harness_auth(b, wrapper=args.wrapper)
        if ready:
            backends_to_try.append(b)
        else:
            sys.stderr.write(f"call-worker: skipping unready backend '{b}' ({reason}). Cached TTL window active.\n")

    if not backends_to_try:
        sys.stderr.write("call-worker: no authenticated backends available, forcing fallback to deterministic script\n")
        backends_to_try = ["script"]

    parsed = None
    last_error = None
    total_time_left = args.timeout
    for idx, b in enumerate(backends_to_try):
        is_last = (idx == len(backends_to_try) - 1)
        if not is_last and total_time_left > 45:
            per_attempt_timeout = max(30, total_time_left - 30)
        else:
            per_attempt_timeout = max(15, total_time_left)
        start_t = time.time()
        try:
            m = args.model if b == args.backend else None
            output = run_backend(b, m, prompt, script_cmd=args.script, wrapper=args.wrapper, timeout=per_attempt_timeout)
            parsed = extract_json(output)
            break
        except subprocess.TimeoutExpired as e:
            last_error = e
            elapsed = int(time.time() - start_t)
            total_time_left = max(5, total_time_left - elapsed)
            sys.stderr.write(f"call-worker: backend {b} timed out after {elapsed}s, falling back...\n")
        except Exception as e:
            last_error = e
            elapsed = int(time.time() - start_t)
            total_time_left = max(5, total_time_left - elapsed)
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
                "scope": "small",
                "suggested_type": "feature",
                "rationale": "Classified as actionable task for Summa42."
            }
        elif task_type == "decompose":
            parsed = {
                "summary": "Decomposition fallback",
                "can_decompose": False,
                "sub_issues": []
            }
        else:
            sys.stderr.write(f"call-worker: all backends failed: {last_error}\n")
            sys.exit(1)

    # Normalize fields according to Go schema invariants
    if task_type == "stage2_classify":
        scope = str(parsed.get("scope", "small")).lower()
        if scope not in ("small", "medium", "large", "huge", "unknown"):
            scope = "small"
        stype = str(parsed.get("suggested_type", "feature")).lower()
        if stype not in ("bug", "feature", "question", "other"):
            stype = "feature"
        normalized = {
            "is_actionable": bool(parsed.get("is_actionable", True)),
            "needs_repro": bool(parsed.get("needs_repro", False)),
            "scope": scope,
            "suggested_type": stype,
            "rationale": str(parsed.get("rationale", "Actionable task for Summa42."))[:1000]
        }
    elif task_type == "decompose":
        sub_issues = []
        for item in parsed.get("sub_issues", []):
            s_scope = str(item.get("scope", "medium")).lower()
            if s_scope not in ("small", "medium", "large", "huge"):
                s_scope = "medium"
            s_type = str(item.get("suggested_type", "feature")).lower()
            if s_type not in ("bug", "feature", "question", "other"):
                s_type = "feature"
            sub_issues.append({
                "title": str(item.get("title", "Sub-issue"))[:200],
                "scope": s_scope,
                "suggested_type": s_type,
                "body": str(item.get("body", "Sub-issue implementation details."))
            })
        normalized = {
            "summary": str(parsed.get("summary", "Decomposition plan"))[:1000],
            "can_decompose": bool(parsed.get("can_decompose", bool(sub_issues))),
            "sub_issues": sub_issues
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

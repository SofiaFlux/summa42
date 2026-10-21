#!/usr/bin/env python3
"""
Tests for call-worker.py: fail-closed behaviour, stdin prompts, credential
isolation, opt-in remote models, and exact schema dispatch.

Run with:
    python3 -m unittest scripts/test_call_worker.py -v

Fake backends are tiny shell scripts written to a temp directory; they never
touch GitHub, Box state, or any network resource.
"""

import json
import os
import stat
import subprocess
import sys
import tempfile
import textwrap
import unittest

# Path to the script under test.
CALL_WORKER = os.path.join(os.path.dirname(__file__), "call-worker.py")

# Minimal PATH that provides /bin/sh builtins but hides real agy/opencode/ollama.
# We prepend tmpdir per-test so fake backends shadow everything.
_SAFE_PATH = "/bin:/usr/bin:/usr/local/bin"


def _run(stdin_json, env=None, extra_args=None, timeout=30):
    """Run call-worker with given stdin JSON dict; return CompletedProcess."""
    cmd = [sys.executable, CALL_WORKER]
    if extra_args:
        cmd.extend(extra_args)
    # Always pass a short --timeout so call-worker exits quickly in tests.
    if "--timeout" not in (extra_args or []):
        cmd.extend(["--timeout", "10"])
    # Start with a minimal hermetic env: no real credentials, no real backends.
    merged_env = {
        "HOME": os.environ.get("HOME", "/tmp"),
        "PATH": _SAFE_PATH,
        "XDG_CACHE_HOME": "/tmp/cw-test-cache",
    }
    if env:
        merged_env.update(env)
    return subprocess.run(
        cmd,
        input=json.dumps(stdin_json),
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        env=merged_env,
        timeout=timeout,
    )


def _make_backend(tmpdir, name, script_body):
    """Write a tiny shell-script fake backend into tmpdir and return its path."""
    path = os.path.join(tmpdir, name)
    with open(path, "w") as f:
        f.write("#!/bin/sh\n")
        f.write(script_body)
    os.chmod(path, stat.S_IRWXU | stat.S_IRGRP | stat.S_IXGRP |
             stat.S_IROTH | stat.S_IXOTH)
    return path


def _path_with(tmpdir):
    """Return PATH that includes tmpdir first, then the safe system directories."""
    return tmpdir + ":" + _SAFE_PATH


class TestFailClosed(unittest.TestCase):
    """S-C4: When every backend fails, call-worker must exit non-zero."""

    def test_all_backends_fail_exits_nonzero(self):
        """Every backend exits 1. Assert return code != 0 and empty stdout."""
        with tempfile.TemporaryDirectory() as tmpdir:
            _make_backend(tmpdir, "fakecog", "exit 1\n")
            result = _run(
                {"schema": "github.issue.plan.review.v1", "question": "review"},
                env={"PATH": _path_with(tmpdir), "XDG_CACHE_HOME": tmpdir},
                extra_args=["--backend", "fakecog", "--fallback", "fakecog"],
            )
        self.assertNotEqual(result.returncode, 0,
                            f"Expected non-zero exit when all backends fail. "
                            f"stderr={result.stderr}")
        self.assertEqual(result.stdout.strip(), "",
                         "Expected empty stdout when all backends fail")

    def test_missing_verdict_is_error(self):
        """Backend returns {} (missing verdict). Assert non-zero exit."""
        with tempfile.TemporaryDirectory() as tmpdir:
            _make_backend(tmpdir, "fakecog", 'echo "{}"\n')
            result = _run(
                {"schema": "github.issue.plan.review.v1"},
                env={"PATH": _path_with(tmpdir), "XDG_CACHE_HOME": tmpdir},
                extra_args=["--backend", "fakecog", "--fallback", "fakecog"],
            )
        self.assertNotEqual(result.returncode, 0,
                            f"Expected non-zero when verdict is missing. "
                            f"stderr={result.stderr}")

    def test_unknown_verdict_is_error(self):
        """Backend returns {"verdict":"MAYBE"}. Assert non-zero exit."""
        with tempfile.TemporaryDirectory() as tmpdir:
            _make_backend(tmpdir, "fakecog",
                          'echo \'{"verdict":"MAYBE"}\'\n')
            result = _run(
                {"schema": "github.issue.plan.review.v1"},
                env={"PATH": _path_with(tmpdir), "XDG_CACHE_HOME": tmpdir},
                extra_args=["--backend", "fakecog", "--fallback", "fakecog"],
            )
        self.assertNotEqual(result.returncode, 0,
                            f"Expected non-zero when verdict is unknown. "
                            f"stderr={result.stderr}")

    def test_plausible_requires_json_bool_string_false_is_error(self):
        """{\"plausible\":\"false\"} (string) gives non-zero exit."""
        with tempfile.TemporaryDirectory() as tmpdir:
            _make_backend(tmpdir, "fakecog",
                          'echo \'{"plausible":"false"}\'\n')
            result = _run(
                {"schema": "github.issue.triage.review.v1"},
                env={"PATH": _path_with(tmpdir), "XDG_CACHE_HOME": tmpdir},
                extra_args=["--backend", "fakecog", "--fallback", "fakecog"],
            )
        self.assertNotEqual(result.returncode, 0,
                            f"String 'false' for plausible must be an error. "
                            f"stderr={result.stderr}")

    def test_plausible_requires_json_bool_true_passes(self):
        """{\"plausible\":false} (bool) passes through unchanged (rc 0)."""
        with tempfile.TemporaryDirectory() as tmpdir:
            _make_backend(tmpdir, "fakecog",
                          'echo \'{"plausible":false}\'\n')
            result = _run(
                {"schema": "github.issue.triage.review.v1"},
                env={"PATH": _path_with(tmpdir), "XDG_CACHE_HOME": tmpdir},
                extra_args=["--backend", "fakecog", "--fallback", "fakecog"],
            )
        self.assertEqual(result.returncode, 0,
                         f"Bool false for plausible must succeed. "
                         f"stderr={result.stderr}")
        out = json.loads(result.stdout)
        self.assertEqual(out, {"plausible": False})

    def test_accept_reason_with_violation_word_stays_accept(self):
        """S-M6 fix: ACCEPT with reason "no violations found" must stay ACCEPT."""
        with tempfile.TemporaryDirectory() as tmpdir:
            _make_backend(tmpdir, "fakecog",
                          'echo \'{"verdict":"ACCEPT","reason":"no violations found"}\'\n')
            result = _run(
                {"schema": "github.issue.plan.review.v1"},
                env={"PATH": _path_with(tmpdir), "XDG_CACHE_HOME": tmpdir},
                extra_args=["--backend", "fakecog", "--fallback", "fakecog"],
            )
        self.assertEqual(result.returncode, 0,
                         f"ACCEPT with benign reason should pass. "
                         f"stderr={result.stderr}")
        out = json.loads(result.stdout)
        self.assertEqual(out["verdict"], "ACCEPT",
                         "ACCEPT with 'no violations found' must remain ACCEPT")


class TestArgvLimit(unittest.TestCase):
    """S-C4/S-I1: argv-only backends must be capped at 100 KiB."""

    def test_argv_backend_rejects_oversized_prompt(self):
        """An argv-only backend with a 200 KiB prompt gives non-zero exit,
        and stderr contains 'prompt exceeds argv limit'."""
        big_prompt = "x" * (200 * 1024)
        with tempfile.TemporaryDirectory() as tmpdir:
            # agy is the argv-only backend; fake it so capability check passes.
            _make_backend(tmpdir, "agy",
                          'echo \'{"verdict":"ACCEPT","reason":"ok"}\'\n')
            result = _run(
                {"schema": "github.issue.plan.review.v1",
                 "question": big_prompt},
                env={"PATH": _path_with(tmpdir), "XDG_CACHE_HOME": tmpdir},
                extra_args=["--backend", "agy", "--fallback", "agy"],
            )
        self.assertNotEqual(result.returncode, 0,
                            f"200 KiB prompt to argv-only backend must fail. "
                            f"stderr={result.stderr}")
        self.assertIn("prompt exceeds argv limit", result.stderr,
                      "stderr must contain 'prompt exceeds argv limit'")


class TestStdinPrompt(unittest.TestCase):
    """S-C4/S-I1: stdin-capable backends receive full prompt on stdin."""

    def test_stdin_backend_receives_full_prompt(self):
        """The fake backend writes its stdin to a file. Assert that the
        grounding context appears verbatim in that file."""
        with tempfile.TemporaryDirectory() as tmpdir:
            capture_file = os.path.join(tmpdir, "captured_stdin.txt")
            # claude is a stdin backend; it receives the prompt on stdin.
            script = textwrap.dedent(f"""\
                cat > {capture_file}
                echo '{{"verdict":"ACCEPT","reason":"ok"}}'
            """)
            _make_backend(tmpdir, "claude", script)
            grounding_text = "UNIQUE_GROUNDING_CONTEXT_12345"
            result = _run(
                {"schema": "github.issue.plan.review.v1",
                 "question": "review this plan",
                 "grounding": {"context": grounding_text}},
                env={
                    "PATH": _path_with(tmpdir),
                    "XDG_CACHE_HOME": tmpdir,
                    # Provide fake credential so check_harness_auth("claude") passes.
                    "ANTHROPIC_API_KEY": "test-key",
                },
                extra_args=["--backend", "claude", "--fallback", "claude"],
            )
            self.assertEqual(result.returncode, 0,
                             f"stdin backend should succeed. stderr={result.stderr}")
            self.assertTrue(os.path.exists(capture_file),
                            "Backend did not write stdin capture file")
            with open(capture_file) as f:
                captured = f.read()
            self.assertIn(grounding_text, captured,
                          "Grounding context must appear verbatim in stdin sent to backend")


class TestCredentialIsolation(unittest.TestCase):
    """S-I4: GITHUB_TOKEN / GH_TOKEN must never reach backend processes."""

    def test_no_github_credentials_in_backend_env(self):
        """Run with GITHUB_TOKEN=x GH_TOKEN=y. The fake backend dumps env.
        Assert neither variable is present."""
        with tempfile.TemporaryDirectory() as tmpdir:
            capture_file = os.path.join(tmpdir, "env_dump.txt")
            script = textwrap.dedent(f"""\
                env > {capture_file}
                echo '{{"verdict":"ACCEPT","reason":"ok"}}'
            """)
            _make_backend(tmpdir, "claude", script)
            result = _run(
                {"schema": "github.issue.plan.review.v1", "question": "review"},
                env={
                    "PATH": _path_with(tmpdir),
                    "XDG_CACHE_HOME": tmpdir,
                    # Provide fake credential so check_harness_auth passes.
                    "ANTHROPIC_API_KEY": "test-key",
                    # These must NOT reach the backend.
                    "GITHUB_TOKEN": "x",
                    "GH_TOKEN": "y",
                },
                extra_args=["--backend", "claude", "--fallback", "claude"],
            )
            self.assertEqual(result.returncode, 0,
                             f"Credential test backend should succeed. "
                             f"stderr={result.stderr}")
            self.assertTrue(os.path.exists(capture_file),
                            "Backend did not write env dump file")
            with open(capture_file) as f:
                env_dump = f.read()
            self.assertNotIn("GITHUB_TOKEN", env_dump,
                             "GITHUB_TOKEN must not be in backend environment")
            self.assertNotIn("GH_TOKEN", env_dump,
                             "GH_TOKEN must not be in backend environment")


class TestOpenRouterOptIn(unittest.TestCase):
    """S-I5: OpenRouter must be opt-in via --allow-remote or env var."""

    def test_openrouter_off_by_default(self):
        """An OpenRouter key is present but --allow-remote is NOT set.
        Assert OpenRouter is never invoked (fake openrouter backend writes a
        marker file; assert the file does NOT exist)."""
        with tempfile.TemporaryDirectory() as tmpdir:
            # Primary backend always fails so fallback logic is exercised.
            _make_backend(tmpdir, "fakeprimary", "exit 1\n")
            result = _run(
                {"schema": "github.issue.plan.review.v1", "question": "review"},
                env={
                    "PATH": _path_with(tmpdir),
                    "XDG_CACHE_HOME": tmpdir,
                    "OPENROUTER_API_KEY": "sk-test-key",
                    # Explicitly NOT setting SUMMA42_ALLOW_REMOTE_MODELS.
                },
                extra_args=["--backend", "fakeprimary",
                            "--fallback", "openrouter"],
                timeout=20,
            )
                # Gate must reject openrouter before any HTTP call is attempted.
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("OpenRouter disabled", result.stderr)
            self.assertNotIn("openrouter error", result.stderr)


class TestSchemaDispatch(unittest.TestCase):
    """S-M9: Unknown schema must be an error (non-zero exit)."""

    def test_unknown_schema_is_error(self):
        """An unknown schema string causes non-zero exit."""
        with tempfile.TemporaryDirectory() as tmpdir:
            _make_backend(tmpdir, "fakecog",
                          'echo \'{"verdict":"ACCEPT","reason":"ok"}\'\n')
            result = _run(
                {"schema": "unknown.schema.totally.made.up.v99"},
                env={"PATH": _path_with(tmpdir), "XDG_CACHE_HOME": tmpdir},
                extra_args=["--backend", "fakecog", "--fallback", "fakecog"],
            )
        self.assertNotEqual(result.returncode, 0,
                            "Unknown schema must cause non-zero exit")


def _run_backend_output(tmpdir, schema, payload, extra=None):
    _make_backend(tmpdir, "fakecog", "echo '%s'\n" % json.dumps(payload))
    return _run({"schema": schema},
                env={"PATH": _path_with(tmpdir), "XDG_CACHE_HOME": tmpdir},
                extra_args=["--backend", "fakecog", "--fallback", "fakecog"])


class TestStrictNormalizers(unittest.TestCase):
    STAGE2 = "github.issue.triage.stage2.input.v1"
    PLAN = "github.issue.plan.v2"
    DECOMP = "github.issue.decompose.v1"

    def _rc(self, schema, payload):
        with tempfile.TemporaryDirectory() as d:
            return _run_backend_output(d, schema, payload)

    def test_stage2_requires_real_booleans(self):
        ok = {"is_actionable": True, "needs_repro": False, "scope": "small",
              "suggested_type": "bug", "rationale": "r"}
        self.assertEqual(self._rc(self.STAGE2, ok).returncode, 0)
        self.assertNotEqual(self._rc(self.STAGE2, dict(ok, is_actionable="false")).returncode, 0)
        missing = dict(ok); del missing["needs_repro"]
        self.assertNotEqual(self._rc(self.STAGE2, missing).returncode, 0)

    def test_non_object_response_is_error(self):
        for schema in (self.STAGE2, self.PLAN, self.DECOMP):
            self.assertNotEqual(self._rc(schema, [1]).returncode, 0, schema)

    def test_plan_requires_fields(self):
        good = {"summary": "s", "steps": [{"objective": "o", "done_when": "d"}]}
        self.assertEqual(self._rc(self.PLAN, good).returncode, 0)
        for bad in ({"summary": "s", "steps": [{"objective": "o"}]},
                    {"summary": "s", "steps": [{"done_when": "d"}]},
                    {"summary": "s", "steps": ["x"]},
                    {"summary": "s", "steps": []}):
            self.assertNotEqual(self._rc(self.PLAN, bad).returncode, 0, bad)

    def test_decompose_requires_fields(self):
        sub = {"title": "t", "scope": "small", "suggested_type": "bug", "body": "b"}
        good = {"summary": "s", "can_decompose": True, "sub_issues": [sub]}
        self.assertEqual(self._rc(self.DECOMP, good).returncode, 0)
        bad_sub = dict(sub); del bad_sub["scope"]
        self.assertNotEqual(self._rc(self.DECOMP, dict(good, sub_issues=[bad_sub])).returncode, 0)


class TestPlanFilesSchema(unittest.TestCase):
    FILES = "github.issue.plan.files.v1"

    def _run(self, payload):
        with tempfile.TemporaryDirectory() as d:
            return _run_backend_output(d, self.FILES, payload)

    def test_plan_files_strict(self):
        good = {"context_files": ["a.go"], "allowed_paths": ["a.go", "a_test.go"]}
        result = self._run(good)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout), good)
        for bad in ({"allowed_paths": ["a.go"]},
                    {"context_files": [], "allowed_paths": []},
                    {"context_files": "a.go", "allowed_paths": ["a.go"]},
                    {"context_files": [1], "allowed_paths": ["a.go"]},
                    {"context_files": ["x%d" % i for i in range(33)], "allowed_paths": ["a.go"]},
                    {"context_files": [], "allowed_paths": ["x%d" % i for i in range(65)]},
                    {"context_files": [], "allowed_paths": ["a.go"], "extra": 1},
                    [1]):
            result = self._run(bad)
            self.assertNotEqual(result.returncode, 0, bad)
            self.assertEqual(result.stdout, "", bad)
            self.assertEqual(len(result.stderr.strip().splitlines()), 1, result.stderr)


class TestCodemieWrapper(unittest.TestCase):
    def test_codemie_prefix_uses_wrapper_binary_on_stdin(self):
        with tempfile.TemporaryDirectory() as d:
            marker = os.path.join(d, "wrapper_used")
            _make_backend(d, "claude", "exit 1\n")
            _make_backend(d, "codemie-claude",
                          "touch %s\ncat >/dev/null\n"
                          "echo '{\"verdict\":\"ACCEPT\",\"reason\":\"ok\"}'\n" % marker)
            r = _run({"schema": "github.issue.plan.review.v1"},
                     env={"PATH": _path_with(d), "XDG_CACHE_HOME": d, "ANTHROPIC_API_KEY": "k"},
                     extra_args=["--backend", "codemie-claude", "--fallback", "codemie-claude"])
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertTrue(os.path.exists(marker))


class TestRulesBlockScope(unittest.TestCase):
    """S-I6: rules_block is appended only to review schemas, and
    autonomous_engineering.md reaches a review prompt exactly once."""

    RULES_SENTINEL = "UNIQUE_RULE_SENTINEL_98765"
    AUTONOMOUS = os.path.realpath(os.path.join(os.path.dirname(__file__), "..", "skills", "autonomous_engineering.md"))
    AUTONOMOUS_HEADING = "# Autonomous Engineering Principles"

    def _captured_prompt(self, schema):
        with tempfile.TemporaryDirectory() as tmpdir:
            capture_file = os.path.join(tmpdir, "captured_stdin.txt")
            _make_backend(tmpdir, "claude",
                          f"cat > {capture_file}\n"
                          "echo '{\"verdict\":\"ACCEPT\",\"reason\":\"ok\"}'\n")
            _run({"schema": schema, "question": "q"},
                 env={"PATH": _path_with(tmpdir), "XDG_CACHE_HOME": tmpdir,
                      "ANTHROPIC_API_KEY": "test-key"},
                 extra_args=["--backend", "claude", "--fallback", "claude",
                             "--rules", self.RULES_SENTINEL,
                             "--rules-file", self.AUTONOMOUS])
            self.assertTrue(os.path.exists(capture_file),
                            f"Backend did not receive a prompt for {schema}")
            with open(capture_file) as f:
                return f.read()

    def test_rules_block_only_on_review_schemas(self):
        review = ["github.issue.plan.review.v1", "github.issue.code.review.v1",
                  "github.issue.triage.review.v1"]
        non_review = ["github.issue.plan.v2", "github.issue.implementation.v1",
                      "github.issue.decompose.v1"]
        for schema in review:
            with self.subTest(schema=schema):
                self.assertIn(self.RULES_SENTINEL, self._captured_prompt(schema))
        for schema in non_review:
            with self.subTest(schema=schema):
                self.assertNotIn(self.RULES_SENTINEL, self._captured_prompt(schema))
        for schema in review[:2]:
            with self.subTest(schema=schema, check="autonomous once"):
                prompt = self._captured_prompt(schema)
                self.assertEqual(prompt.count(self.AUTONOMOUS_HEADING), 1)
        # Verdict wording: triage returns {"plausible": bool}, so it must not
        # mention REVISE/BLOCK; plan and code review keep the REVISE/BLOCK sentence.
        triage = self._captured_prompt("github.issue.triage.review.v1")
        self.assertIn("plausible must be false", triage)
        self.assertNotIn("REVISE or BLOCK", triage)
        for schema in review[:2]:
            with self.subTest(schema=schema, check="verdict wording"):
                self.assertIn("verdict must be REVISE or BLOCK", self._captured_prompt(schema))


if __name__ == "__main__":
    unittest.main()

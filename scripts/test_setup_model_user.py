"""Tests for the pure functions of setup-model-user.sh, run in a temp sandbox.

The script is sourced (its main() only runs when executed), never run: it needs
root, useradd and sudoers. run_as_model is replaced by a function that runs the
command as the current user inside a fake HOME.
"""
import os
import shutil
import stat
import subprocess
import tempfile
import textwrap
import unittest
from pathlib import Path

SCRIPT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "setup-model-user.sh")


def bash(body, env=None, cwd=None):
    full = dict(os.environ)
    full.update(env or {})
    return subprocess.run(["bash", "-c", f"source {SCRIPT}\n{body}"], capture_output=True, text=True, env=full, cwd=cwd)


class Pure(unittest.TestCase):
    def test_user_name_validation(self):
        for good in ("summa42-model", "_x", "a1"):
            self.assertEqual(bash('valid_model_user "$NAME"', {"NAME": good}).returncode, 0, good)
        for bad in ("", "Root", "1abc", "a b", "a;ALL", "ALL", "a" * 40, "x\nroot"):
            self.assertNotEqual(bash('valid_model_user "$NAME"', {"NAME": bad}).returncode, 0, bad)

    def test_sudoers_content(self):
        out = bash('sudoers_content alice summa42-model').stdout
        self.assertEqual(out.splitlines(), [
            "alice ALL=(summa42-model) NOPASSWD: SETENV: ALL",
            "Defaults>summa42-model !log_allowed"])

    def test_sudoers_content_for_the_validation_user(self):
        out = bash('sudoers_content alice summa42-validate').stdout
        self.assertEqual(out.splitlines(), [
            "alice ALL=(summa42-validate) NOPASSWD: SETENV: ALL",
            "Defaults>summa42-validate !log_allowed"])
        visudo = shutil.which("visudo") or "/usr/sbin/visudo"
        if os.path.exists(visudo):
            with tempfile.TemporaryDirectory() as d:
                frag = os.path.join(d, "f")
                Path(frag).write_text(out)
                r = subprocess.run([visudo, "-cf", frag], capture_output=True, text=True)
                if "Permission denied" not in r.stderr:
                    self.assertEqual(r.returncode, 0, r.stderr)

    def test_wsl_detection(self):
        with tempfile.TemporaryDirectory() as d:
            ver = os.path.join(d, "version")
            Path(ver).write_text("Linux version 6.6 microsoft-standard-WSL2\n")
            self.assertEqual(bash("is_wsl", {"WSL_INTEROP_FILE": d + "/none", "PROC_VERSION_FILE": ver}).returncode, 0)
            Path(ver).write_text("Linux version 6.6 generic\n")
            self.assertNotEqual(bash("is_wsl", {"WSL_INTEROP_FILE": d + "/none", "PROC_VERSION_FILE": ver}).returncode, 0)
            Path(d + "/interop").write_text("1")
            self.assertEqual(bash("is_wsl", {"WSL_INTEROP_FILE": d + "/interop", "PROC_VERSION_FILE": ver}).returncode, 0)

    def test_wsl_warning_names_the_settings(self):
        err = bash("wsl_warning").stderr
        for want in ("WARNING", "INCOMPLETE", "[interop]", "enabled=false", "/mnt/c", "[automount]", "does not change /etc/wsl.conf"):
            self.assertIn(want, err)

    def test_secret_scan(self):
        with tempfile.TemporaryDirectory() as d:
            files = {
                "gh": 'x ghp_' + "a" * 30, "pat": "github_pat_" + "B" * 30, "oauth": "gho_" + "c" * 30,
                "ado": '{"mcpServers":{"ado":{"env":{"X":"' + "a2" * 26 + '"}}}}', "clean": '{"access_token":"abc"}',
                "clean2": '{"selectedType":"oauth-personal","note":"' + "a" * 70 + ' not a token"}',
            }
            for name, content in files.items():
                Path(os.path.join(d, name)).write_text(content)
            hits = lambda mode, *names: bash(f'scan_secrets {mode} ' + " ".join(os.path.join(d, n) for n in names))
            for bad in ("gh", "pat", "oauth", "ado"):
                r = hits("default", bad)
                self.assertEqual(r.returncode, 0, bad)
                self.assertIn(bad, r.stdout)
            self.assertNotEqual(hits("default", "clean").returncode, 0)
            self.assertNotEqual(hits("default", "clean2").returncode, 0)
            # Copilot's own OAuth token is what is being copied there; a PAT is still refused.
            self.assertNotEqual(hits("copilot", "oauth").returncode, 0)
            self.assertEqual(hits("copilot", "gh").returncode, 0)

    def test_scan_catches_the_four_known_misses(self):
        """ADO PAT new format, unquoted PAT, Bearer header, binary-flagged file."""
        with tempfile.TemporaryDirectory() as d:
            ado84 = "".join("AbCdEf0123456789"[i % 16] for i in range(84))
            files = {
                "ado84": '{"mcpServers":{"ado":{"env":{"AZURE_DEVOPS_ORG_PAT":"' + ado84 + '"}}}}',
                "ado84bare": '{"headers":{"X":"' + ado84 + '"}}',
                "unquoted52": "token=" + "a2" * 26 + "\n",
                "bearer": '{"headers":{"Authorization":"Bearer ' + "x" * 40 + '"}}',
                "bearer2": "Authorization: Bearer " + "Ab1." * 10 + "\n",
            }
            for name, content in files.items():
                Path(os.path.join(d, name)).write_text(content)
            Path(os.path.join(d, "binary")).write_bytes(b"\x00\x01 ghp_" + b"a" * 30 + b"\x00")
            for name in list(files) + ["binary"]:
                r = bash(f"scan_secrets default {d}/{name}")
                self.assertEqual(r.returncode, 0, name)
                self.assertIn(name, r.stdout)

    def test_gemini_settings_keep_only_the_auth_section(self):
        with tempfile.TemporaryDirectory() as d:
            src, dst = os.path.join(d, "in.json"), os.path.join(d, "out.json")
            Path(src).write_text('{"security":{"auth":{"selectedType":"oauth-personal","useExternal":true,"extra":{"t":"x"}},"folderTrust":{"enabled":true}},'
                                 '"mcpServers":{"ado":{"env":{"AZURE_DEVOPS_EXT_PAT":"x"},"headers":{"Authorization":"Bearer y"}}},"hooks":{"a":"rm -rf"},"tools":{},"ui":{}}')
            self.assertEqual(bash(f"filter_gemini_settings {src} {dst}").returncode, 0)
            import json
            self.assertEqual(json.loads(Path(dst).read_text()), {"security": {"auth": {"selectedType": "oauth-personal", "useExternal": True}}})
            Path(src).write_text("{not json")
            self.assertNotEqual(bash(f"filter_gemini_settings {src} {dst}").returncode, 0)

    def test_claude_json_is_filtered_to_login_metadata(self):
        with tempfile.TemporaryDirectory() as d:
            src, dst = os.path.join(d, "in.json"), os.path.join(d, "out.json")
            Path(src).write_text('{"hasCompletedOnboarding":true,"oauthAccount":{"a":1},"userID":"u","mcpServers":{"x":{"env":{"T":"ghp_zzz"}}},"projects":{"/p":{}}}')
            self.assertEqual(bash(f"filter_claude_json {src} {dst}").returncode, 0)
            out = Path(dst).read_text()
            self.assertIn("oauthAccount", out)
            self.assertNotIn("mcpServers", out)
            self.assertNotIn("projects", out)

    def test_group_files_are_auth_only(self):
        listing = bash('for g in claude codex commandcode gemini copilot; do group_files $g; done').stdout
        for forbidden in ("projects", "history", "sessions", "session-state", "plugins"):
            self.assertNotIn(forbidden, listing)
        self.assertIn(".claude/.credentials.json", listing)
        self.assertIn(".codex/auth.json", listing)
        self.assertIn(".commandcode/auth.json", listing)

    def test_chmod_owner_dir_does_not_follow_symlinks(self):
        with tempfile.TemporaryDirectory() as d:
            target, link, real = (os.path.join(d, n) for n in ("target", "link", "real"))
            os.mkdir(target, 0o755); os.mkdir(real, 0o755)
            os.symlink(target, link)
            r = bash(f"chmod_owner_dir {link}; chmod_owner_dir {real}; chmod_owner_dir {d}/absent")
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertEqual(stat.S_IMODE(os.stat(target).st_mode), 0o755)
            self.assertEqual(stat.S_IMODE(os.stat(real).st_mode), 0o700)


class ValidationUser(unittest.TestCase):
    """The validation user must hold no credentials and must run the toolchain."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.home = os.path.join(self.tmp.name, "validate-home")
        os.makedirs(self.home + "/go-cache")
        os.makedirs(self.home + "/tmp")

    def tearDown(self):
        self.tmp.cleanup()

    def clean(self):
        return bash(f"""
            VALIDATE_HOME={self.home}; VALIDATE_USER=summa42-validate
            run_as_validate() {{ env HOME="$VALIDATE_HOME" "$@"; }}
            validate_home_clean
        """)

    def test_clean_home_passes(self):
        Path(self.home + "/go-cache/x").write_text("ghp_" + "a" * 30)  # compiled output is not scanned
        Path(self.home + "/tmp/y").write_text("ghp_" + "a" * 30)
        self.assertEqual(self.clean().returncode, 0)

    def test_known_login_files_are_refused(self):
        for rel in (".claude/.credentials.json", ".codex/auth.json", ".gemini/oauth_creds.json",
                    ".config/github-copilot/hosts.json", ".config/gh/hosts.yml", ".git-credentials"):
            path = os.path.join(self.home, rel)
            os.makedirs(os.path.dirname(path), exist_ok=True)
            Path(path).write_text("{}")
            r = self.clean()
            self.assertNotEqual(r.returncode, 0, rel)
            self.assertIn(rel, r.stderr)
            os.remove(path)
        self.assertEqual(self.clean().returncode, 0)

    def test_token_content_anywhere_in_the_home_is_refused(self):
        Path(self.home + "/notes.txt").write_text("token ghp_" + "b" * 30)
        r = self.clean()
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("notes.txt", r.stderr)

    def toolchain(self, dirs, go_body):
        gobin = os.path.join(self.tmp.name, "tc", "bin")
        os.makedirs(gobin, exist_ok=True)
        go = os.path.join(gobin, "go")
        Path(go).write_text("#!/bin/sh\n" + go_body)
        os.chmod(go, 0o755)
        return bash(f"""
            VALIDATE_HOME={self.home}; VALIDATE_USER=summa42-validate; OWNER_HOME=/home/owner
            TOOLCHAIN_DIRS='{dirs.replace("@", gobin)}'
            run_as_validate() {{ env HOME="$VALIDATE_HOME" "$@"; }}
            check_toolchain
        """)

    def test_toolchain_must_run_as_the_validation_user(self):
        r = self.toolchain("@", 'echo "go version go1.27.1 linux/amd64"')
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("go1.27.1", r.stdout)
        r = self.toolchain("@", "echo permission denied >&2; exit 126")
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("go version", r.stderr)
        self.assertNotEqual(self.toolchain("relative/bin", "true").returncode, 0)

    def test_toolchain_check_is_skipped_when_unset(self):
        r = bash("TOOLCHAIN_DIRS=''; check_toolchain")
        self.assertEqual(r.returncode, 0)
        self.assertEqual(r.stdout, "")

    def test_require_unreadable_checks_the_named_user(self):
        secret = os.path.join(self.tmp.name, "secret")
        Path(secret).write_text("x")
        r = bash(f"""
            fail=0
            run_as_model() {{ return 1; }}
            run_as_validate() {{ return 0; }}
            require_unreadable {secret} required model
            echo "model fail=$fail"
            require_unreadable {secret} required validate
            echo "validate fail=$fail"
        """)
        self.assertIn("model fail=0", r.stdout)
        self.assertIn("validate fail=1", r.stdout)
        self.assertIn("READABLE by validate", r.stderr)

    def test_model_home_login_files_are_listed_once(self):
        listed = bash("model_home_login_files").stdout.split()
        self.assertEqual(len(set(listed)), len(listed))
        self.assertIn(".claude.json", listed)


class Executed(unittest.TestCase):
    def run_script(self, *args, user="summa42-model"):
        env = dict(os.environ, SUMMA42_MODEL_USER=user)
        return subprocess.run(["bash", SCRIPT, *args], capture_output=True, text=True, env=env)

    def test_bad_user_name_is_refused_before_anything_else(self):
        r = self.run_script(user="x;ALL")
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("not a valid system user name", r.stderr)

    def test_validation_user_must_be_valid_and_differ_from_the_model_user(self):
        env = dict(os.environ, SUMMA42_VALIDATION_USER="x;ALL")
        r = subprocess.run(["bash", SCRIPT], capture_output=True, text=True, env=env)
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("SUMMA42_VALIDATION_USER", r.stderr)
        env = dict(os.environ, SUMMA42_MODEL_USER="same-user", SUMMA42_VALIDATION_USER="same-user")
        r = subprocess.run(["bash", SCRIPT], capture_output=True, text=True, env=env)
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("must differ from the model user", r.stderr)

    def test_unknown_flag_is_a_usage_error(self):
        self.assertEqual(self.run_script("--nope").returncode, 2)

    def test_refuses_to_run_unprivileged(self):
        if os.geteuid() == 0:
            self.skipTest("running as root")
        self.assertIn("run with sudo", self.run_script().stderr)


class Copy(unittest.TestCase):
    """import_cli end to end with run_as_model running as the current user."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        d = self.tmp.name
        self.owner, self.model, self.summa = (os.path.join(d, n) for n in ("owner", "model", "owner/.summa42"))
        for p in (self.owner + "/.claude/projects/p", self.owner + "/.codex", self.model, self.summa):
            os.makedirs(p)
        Path(self.owner + "/.claude/.credentials.json").write_text('{"claudeAiOauth":{"accessToken":"sk-ant-oat01-x"}}')
        Path(self.owner + "/.claude/projects/p/session.jsonl").write_text("transcript")
        Path(self.owner + "/.claude/history.jsonl").write_text("history")
        Path(self.owner + "/.claude.json").write_text('{"hasCompletedOnboarding":true,"mcpServers":{"a":{}}}')
        Path(self.owner + "/.codex/auth.json").write_text('{"tokens":"x"}')

    def tearDown(self):
        self.tmp.cleanup()

    def run_import(self, cli, yes=1, extra=""):
        return bash(f"""
            OWNER_HOME={self.owner}; MODEL_HOME={self.model}; SUMMA_HOME={self.summa}; ASSUME_YES={yes}
            copied=(); skipped=()
            run_as_model() {{ env HOME="$MODEL_HOME" "$@"; }}
            {extra}
            import_cli {cli}
        """)

    def test_commandcode_copies_only_auth_json(self):
        cc = self.owner + "/.commandcode"
        os.makedirs(cc + "/projects")
        Path(cc + "/auth.json").write_text('{"apiKey":"user_x"}')
        Path(cc + "/history.jsonl").write_text("history")
        Path(cc + "/mcp.json").write_text("{}")
        r = self.run_import("commandcode")
        self.assertEqual(r.returncode, 0, r.stderr + r.stdout)
        self.assertEqual(sorted(os.listdir(self.model + "/.commandcode")), ["auth.json"])
        self.assertEqual(stat.S_IMODE(os.stat(self.model + "/.commandcode/auth.json").st_mode), 0o600)

    def test_claude_copies_only_credentials_and_filtered_json(self):
        r = self.run_import("claude")
        self.assertEqual(r.returncode, 0, r.stderr + r.stdout)
        self.assertTrue(os.path.exists(self.model + "/.claude/.credentials.json"))
        self.assertEqual(stat.S_IMODE(os.stat(self.model + "/.claude/.credentials.json").st_mode), 0o600)
        self.assertFalse(os.path.exists(self.model + "/.claude/projects"))
        self.assertFalse(os.path.exists(self.model + "/.claude/history.jsonl"))
        self.assertNotIn("mcpServers", Path(self.model + "/.claude.json").read_text())
        self.assertEqual([n for n in os.listdir(self.model) if n.startswith(".stage")], [])

    def test_token_in_content_refuses_and_names_the_file(self):
        Path(self.owner + "/.codex/auth.json").write_text("ghp_" + "a" * 30)
        r = self.run_import("codex")
        self.assertEqual(r.returncode, 2)
        self.assertIn(self.owner + "/.codex/auth.json", r.stderr)
        self.assertFalse(os.path.exists(self.model + "/.codex"))

    def test_symlinked_source_is_not_copied(self):
        os.remove(self.owner + "/.codex/auth.json")
        secret = self.summa + "/secret"
        Path(secret).write_text("x")
        os.symlink(secret, self.owner + "/.codex/auth.json")
        self.assertEqual(self.run_import("codex").returncode, 0)
        self.assertFalse(os.path.exists(self.model + "/.codex"))

    def test_copilot_is_skipped_under_yes(self):
        os.makedirs(self.owner + "/.copilot")
        Path(self.owner + "/.copilot/config.json").write_text('{"login":"u"}')
        r = self.run_import("copilot", yes=1)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertFalse(os.path.exists(self.model + "/.copilot"))
        self.assertIn("never copied under --yes", r.stdout)

    def test_failed_move_is_not_reported_as_done(self):
        """Review N4: set -e is off inside a function called from `||`."""
        os.makedirs(self.model + "/.codex/auth.json")  # mv -T onto a directory fails
        r = self.run_import("codex")
        self.assertNotEqual(r.returncode, 0, r.stdout)
        self.assertNotIn("done:", r.stdout)

    def test_malformed_claude_json_is_not_reported_as_done(self):
        Path(self.owner + "/.claude.json").write_text("{not json")
        r = self.run_import("claude")
        self.assertNotEqual(r.returncode, 0, r.stdout)
        self.assertNotIn("done:", r.stdout)
        self.assertFalse(os.path.exists(self.model + "/.claude/.credentials.json"))

    def test_failed_mktemp_writes_nothing_and_fails(self):
        r = self.run_import("claude", extra="mktemp() { return 1; }")
        self.assertNotEqual(r.returncode, 0, r.stdout)
        self.assertNotIn("done:", r.stdout)
        self.assertFalse(os.path.exists("/.claude.json"))

    def test_filtered_file_is_never_copied_unfiltered_without_python(self):
        r = self.run_import("claude", extra="command() { if [ \"$2\" = python3 ]; then return 1; fi; builtin command \"$@\"; }")
        self.assertEqual(r.returncode, 0, r.stderr + r.stdout)
        self.assertFalse(os.path.exists(self.model + "/.claude.json"))
        self.assertTrue(os.path.exists(self.model + "/.claude/.credentials.json"))

    def test_gemini_copies_only_the_auth_section_of_settings(self):
        os.makedirs(self.owner + "/.gemini")
        Path(self.owner + "/.gemini/oauth_creds.json").write_text('{"refresh_token":"x"}')
        Path(self.owner + "/.gemini/settings.json").write_text(
            '{"security":{"auth":{"selectedType":"oauth-personal"}},"mcpServers":{"a":{"env":{"T":"v"}}}}')
        r = self.run_import("gemini")
        self.assertEqual(r.returncode, 0, r.stderr + r.stdout)
        out = Path(self.model + "/.gemini/settings.json").read_text()
        self.assertIn("oauth-personal", out)
        self.assertNotIn("mcpServers", out)

    def test_copilot_mcp_config_is_offered_with_the_same_scan(self):
        os.makedirs(self.owner + "/.copilot")
        Path(self.owner + "/.copilot/config.json").write_text('{"login":"u"}')
        Path(self.owner + "/.copilot/mcp-config.json").write_text('{"mcpServers":{"ado":{"env":{"T":"ghp_' + "a" * 30 + '"}}}}')
        r = self.run_import("copilot", yes=0, extra="ask() { return 0; }")
        self.assertEqual(r.returncode, 2, r.stdout + r.stderr)
        self.assertIn("mcp-config.json", r.stderr)
        Path(self.owner + "/.copilot/mcp-config.json").write_text('{"mcpServers":{"ado":{"command":"/usr/bin/ado-mcp"}}}')
        r = self.run_import("copilot", yes=0, extra="ask() { return 0; }")
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertTrue(os.path.exists(self.model + "/.copilot/mcp-config.json"))

    def test_declined_copy_leaves_existing_files(self):
        os.makedirs(self.model + "/.codex")
        Path(self.model + "/.codex/auth.json").write_text("old")
        self.assertEqual(self.run_import("codex", yes=0).returncode, 0)
        self.assertEqual(Path(self.model + "/.codex/auth.json").read_text(), "old")


if __name__ == "__main__":
    unittest.main()

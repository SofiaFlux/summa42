"""Tests for the pure functions of setup-model-user.sh, run in a temp sandbox.

The script is sourced (its main() only runs when executed), never run: it needs
root, useradd and sudoers. run_as_model is replaced by a function that runs the
command as the current user inside a fake HOME.
"""
import os
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
            }
            for name, content in files.items():
                Path(os.path.join(d, name)).write_text(content)
            hits = lambda mode, *names: bash(f'scan_secrets {mode} ' + " ".join(os.path.join(d, n) for n in names))
            for bad in ("gh", "pat", "oauth", "ado"):
                r = hits("default", bad)
                self.assertEqual(r.returncode, 0, bad)
                self.assertIn(bad, r.stdout)
            self.assertNotEqual(hits("default", "clean").returncode, 0)
            # Copilot's own OAuth token is what is being copied there; a PAT is still refused.
            self.assertNotEqual(hits("copilot", "oauth").returncode, 0)
            self.assertEqual(hits("copilot", "gh").returncode, 0)

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
        listing = bash('for g in claude codex gemini copilot; do group_files $g; done').stdout
        for forbidden in ("projects", "history", "sessions", "session-state", "plugins"):
            self.assertNotIn(forbidden, listing)
        self.assertIn(".claude/.credentials.json", listing)
        self.assertIn(".codex/auth.json", listing)

    def test_chmod_owner_dir_does_not_follow_symlinks(self):
        with tempfile.TemporaryDirectory() as d:
            target, link, real = (os.path.join(d, n) for n in ("target", "link", "real"))
            os.mkdir(target, 0o755); os.mkdir(real, 0o755)
            os.symlink(target, link)
            r = bash(f"chmod_owner_dir {link}; chmod_owner_dir {real}; chmod_owner_dir {d}/absent")
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertEqual(stat.S_IMODE(os.stat(target).st_mode), 0o755)
            self.assertEqual(stat.S_IMODE(os.stat(real).st_mode), 0o700)


class Executed(unittest.TestCase):
    def run_script(self, *args, user="summa42-model"):
        env = dict(os.environ, SUMMA42_MODEL_USER=user)
        return subprocess.run(["bash", SCRIPT, *args], capture_output=True, text=True, env=env)

    def test_bad_user_name_is_refused_before_anything_else(self):
        r = self.run_script(user="x;ALL")
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("not a valid system user name", r.stderr)

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

    def test_declined_copy_leaves_existing_files(self):
        os.makedirs(self.model + "/.codex")
        Path(self.model + "/.codex/auth.json").write_text("old")
        self.assertEqual(self.run_import("codex", yes=0).returncode, 0)
        self.assertEqual(Path(self.model + "/.codex/auth.json").read_text(), "old")


if __name__ == "__main__":
    unittest.main()

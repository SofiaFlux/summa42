import contextlib
import io
import os
import subprocess
import tempfile
import unittest
from pathlib import Path

import check_protected_paths as gate

REPO_ROOT = Path(__file__).resolve().parent.parent

GIT_ENV = {
    "GIT_AUTHOR_NAME": "t",
    "GIT_AUTHOR_EMAIL": "t@example.invalid",
    "GIT_COMMITTER_NAME": "t",
    "GIT_COMMITTER_EMAIL": "t@example.invalid",
    "GIT_CONFIG_GLOBAL": os.devnull,
    "GIT_CONFIG_SYSTEM": os.devnull,
    "PATH": os.environ.get("PATH", ""),
}

BASE_FILES = {
    "README.md": "readme\n",
    "LICENSE": "license\n",
    "docs/VISION.md": "vision\n",
    "internal/emergency/freeze.go": "package emergency\n",
    "internal/emergencyroom/x.go": "package emergencyroom\n",
    ".github/workflows/ci.yml": "name: CI\n",
    "scripts/check_protected_paths.py": "# gate\n",
}


class GateTestCase(unittest.TestCase):
    """Each test builds a real Git repository: a base commit and a branch."""

    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        self.repo = Path(self._tmp.name)
        self.git("init", "-q", "-b", "main")
        for name, content in BASE_FILES.items():
            self.write(name, content)
        self.commit("base")
        self.git("checkout", "-q", "-b", "change")

    def git(self, *arguments):
        return subprocess.run(
            ["git", *arguments], cwd=self.repo, env=GIT_ENV, check=True, capture_output=True, text=True
        ).stdout

    def write(self, name, content):
        path = self.repo / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content, encoding="utf-8")

    def commit(self, message):
        self.git("add", "-A")
        self.git("commit", "-q", "-m", message)

    def run_gate(self, *extra, labels=None, base="main"):
        arguments = ["--repo", str(self.repo), "--base", base, *extra]
        environment = dict(GIT_ENV)
        if labels is not None:
            environment["PR_LABELS"] = labels
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            code = gate.main(arguments, environment)
        return code, out.getvalue() + err.getvalue()


class ProtectedPathGateTests(GateTestCase):
    def test_changed_protected_file_fails(self):
        self.write("docs/VISION.md", "a different vision\n")
        self.commit("change vision")
        code, output = self.run_gate()
        self.assertEqual(code, 1)
        self.assertIn("docs/VISION.md", output)

    def test_changed_protected_file_with_label_passes(self):
        self.write("docs/VISION.md", "a different vision\n")
        self.commit("change vision")
        code, output = self.run_gate(labels='["bug", "owner-approved-protected"]')
        self.assertEqual(code, 0)
        self.assertIn("docs/VISION.md", output)

    def test_label_argument_overrides_environment(self):
        self.write("LICENSE", "other\n")
        self.commit("change license")
        code, _ = self.run_gate("--labels", "owner-approved-protected", labels="[]")
        self.assertEqual(code, 0)
        code, _ = self.run_gate("--labels", "bug", labels='["owner-approved-protected"]')
        self.assertEqual(code, 1)

    def test_similar_label_does_not_pass(self):
        self.write("LICENSE", "other\n")
        self.commit("change license")
        for labels in ('["owner-approved-protected-not"]', "owner-approved", "Owner-Approved-Protected", ""):
            code, _ = self.run_gate(labels=labels)
            self.assertEqual(code, 1, labels)

    def test_unrelated_file_passes(self):
        self.write("README.md", "new readme\n")
        self.write("internal/emergencyroom/x.go", "package emergencyroom // changed\n")
        self.write("docs/VISION.md.notes", "not the vision\n")
        self.commit("unrelated")
        code, output = self.run_gate()
        self.assertEqual(code, 0, output)

    def test_file_in_protected_directory_fails(self):
        self.write("internal/emergency/deep/new.go", "package deep\n")
        self.commit("new file under a protected directory")
        code, output = self.run_gate()
        self.assertEqual(code, 1)
        self.assertIn("internal/emergency/deep/new.go", output)

    def test_renamed_protected_file_fails(self):
        self.git("mv", "docs/VISION.md", "docs/OLD_VISION.md")
        self.commit("rename the vision away")
        code, output = self.run_gate()
        self.assertEqual(code, 1)
        self.assertIn("docs/VISION.md", output)

    def test_deleted_protected_file_fails(self):
        self.git("rm", "-q", "scripts/check_protected_paths.py")
        self.commit("delete the gate")
        code, output = self.run_gate()
        self.assertEqual(code, 1)
        self.assertIn("scripts/check_protected_paths.py", output)

    def test_azure_pipeline_file_in_subdirectory_fails(self):
        self.write("services/api/azure-pipelines.yml", "trigger: none\n")
        self.write("deploy/azure-pipelines-release.yaml", "trigger: none\n")
        self.commit("pipeline definitions")
        code, output = self.run_gate()
        self.assertEqual(code, 1)
        self.assertIn("services/api/azure-pipelines.yml", output)
        self.assertIn("deploy/azure-pipelines-release.yaml", output)

    def test_every_changed_protected_file_is_named(self):
        self.write("LICENSE", "other\n")
        self.write(".github/workflows/extra.yml", "name: extra\n")
        self.write("README.md", "new readme\n")
        self.commit("several")
        code, output = self.run_gate()
        self.assertEqual(code, 1)
        self.assertIn("LICENSE", output)
        self.assertIn(".github/workflows/extra.yml", output)
        self.assertNotIn("README.md", output)

    def test_only_the_branch_side_counts(self):
        # A protected file that changed on the base after the branch was cut
        # is not a change of the pull request.
        self.write("README.md", "branch readme\n")
        self.commit("branch change")
        self.git("checkout", "-q", "main")
        self.write("LICENSE", "base moved on\n")
        self.commit("base change")
        self.git("checkout", "-q", "change")
        code, output = self.run_gate()
        self.assertEqual(code, 0, output)

    def test_unusual_file_name_cannot_hide_a_change(self):
        self.write("internal/emergency/a\nb.go", "package emergency\n")
        self.commit("newline in a file name")
        code, _ = self.run_gate()
        self.assertEqual(code, 1)

    def test_unknown_base_fails_closed(self):
        self.write("README.md", "new readme\n")
        self.commit("unrelated")
        code, _ = self.run_gate(base="no-such-ref")
        self.assertEqual(code, 2)

    def test_malformed_labels_fail_closed(self):
        self.write("README.md", "new readme\n")
        self.commit("unrelated")
        for labels in ('["owner-approved-protected"', '{"a": 1}', "[1]"):
            code, _ = self.run_gate(labels=labels)
            self.assertEqual(code, 2, labels)


class ProtectedListTests(unittest.TestCase):
    def test_list_is_rule_r5(self):
        for path in (
            "docs/VISION.md",
            "internal/policy/default_profile.go",
            "internal/emergency/x.go",
            "internal/approvals/x.go",
            "internal/identity/x.go",
            "internal/modelrun/modelruntest/x.go",
            "scripts/setup-model-user.sh",
            ".github/workflows/ci.yml",
            ".github/CODEOWNERS",
            "LICENSE",
            "SECURITY.md",
            "scripts/check_protected_paths.py",
            "Makefile",
            "tests/acceptance/architecture_test.go",
            "internal/securityregression/fabrication_test.go",
            "internal/ghtriage/climodel/profile.go",
            "AGENTS.md",
            "go.mod",
            "docs/superpowers/plans/2026-11-11-development-plan.md",
            "azure-pipelines.yml",
            "a/b/azure-pipelines.prod.yml",
        ):
            self.assertTrue(gate.is_protected(path), path)
        for path in (
            "go.sum",
            "internal/policy/engine.go",
            "internal/ghtriage/climodel/profile_test.go",
            "docs/LICENSE",
            "sub/Makefile",
            "scripts/check_protected_paths.pyc",
            "azure-pipelines.yml/readme.md",
            "docs/azure-pipelines.md",
        ):
            self.assertFalse(gate.is_protected(path), path)

    def test_codeowners_covers_every_protected_entry(self):
        patterns = set()
        for line in (REPO_ROOT / ".github" / "CODEOWNERS").read_text(encoding="utf-8").splitlines():
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            pattern, *owners = line.split()
            self.assertTrue(owners, f"no owner for {pattern}")
            patterns.add(pattern)
        self.assertEqual(patterns, set(gate.codeowners_patterns()))


if __name__ == "__main__":
    unittest.main()

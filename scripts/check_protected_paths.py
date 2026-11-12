#!/usr/bin/env python3
"""Fail when a pull request changes a protected path without the Owner's label.

The protected paths are the list of rule R5 in
docs/superpowers/plans/2026-11-11-development-plan.md, plus the CODEOWNERS file
that assigns them. This file is the one place the list lives.

The label is a convenience, not the control: any identity with write access can
set a label. The control is required code-owner review for the same paths
(.github/CODEOWNERS). CI must run the base branch's copy of this script, never
the pull request's own copy.

Exit status: 0 no protected path changed, or the label is present;
1 a protected path changed without the label; 2 the check could not be made.
"""

from __future__ import annotations

import argparse
import fnmatch
import json
import os
import subprocess
import sys

LABEL = "owner-approved-protected"

# Exact repository-relative files.
PROTECTED_FILES = (
    "docs/VISION.md",
    "internal/policy/default_profile.go",
    "scripts/setup-model-user.sh",
    "LICENSE",
    "SECURITY.md",
    "scripts/check_protected_paths.py",
    "Makefile",
    "tests/acceptance/architecture_test.go",
    "internal/ghtriage/climodel/profile.go",
    "AGENTS.md",
    "go.mod",
    "docs/superpowers/plans/2026-11-11-development-plan.md",
    ".github/CODEOWNERS",
)

# Directories; everything below them is protected. Each ends with "/".
PROTECTED_DIRECTORIES = (
    "internal/emergency/",
    "internal/approvals/",
    "internal/identity/",
    "internal/modelrun/",
    "internal/securityregression/",
    ".github/workflows/",
)

# File names protected in any directory (Azure Pipelines definitions).
PROTECTED_BASENAMES = (
    "azure-pipelines*.yml",
    "azure-pipelines*.yaml",
)


def is_protected(path: str) -> bool:
    """Report whether a repository-relative path is protected."""
    if path in PROTECTED_FILES:
        return True
    if any(path.startswith(directory) for directory in PROTECTED_DIRECTORIES):
        return True
    basename = path.rsplit("/", 1)[-1].lower()
    return any(fnmatch.fnmatchcase(basename, pattern) for pattern in PROTECTED_BASENAMES)


def codeowners_patterns() -> list[str]:
    """Return the CODEOWNERS patterns that cover exactly the protected list."""
    patterns = [f"/{path}" for path in PROTECTED_FILES]
    patterns += [f"/{directory}" for directory in PROTECTED_DIRECTORIES]
    patterns += [f"**/{pattern}" for pattern in PROTECTED_BASENAMES]
    return patterns


class CheckError(Exception):
    """Raised when the check cannot be made; the gate then fails closed."""


def changed_files(repo: str, base: str, head: str) -> list[str]:
    """List files the head changed since it left the base, renames as delete plus add."""
    command = [
        "git",
        "-C",
        repo,
        "diff",
        "--name-only",
        "--no-renames",
        "-z",
        f"{base}...{head}",
        "--",
    ]
    try:
        result = subprocess.run(command, capture_output=True, check=False)
    except OSError as error:
        raise CheckError(f"cannot run git: {error}") from error
    if result.returncode != 0:
        detail = result.stderr.decode("utf-8", "replace").strip()
        raise CheckError(f"git diff {base}...{head} failed: {detail}")
    names = result.stdout.decode("utf-8", "surrogateescape").split("\0")
    return [name for name in names if name]


def parse_labels(text: str) -> set[str]:
    """Read labels from a JSON array of strings or a comma or newline separated list."""
    text = text.strip()
    if not text:
        return set()
    if text[0] in "[{":
        try:
            value = json.loads(text)
        except ValueError as error:
            raise CheckError(f"labels are not valid JSON: {error}") from error
        if not isinstance(value, list) or not all(isinstance(item, str) for item in value):
            raise CheckError("labels JSON must be an array of strings")
        return set(value)
    return {part.strip() for part in text.replace("\n", ",").split(",") if part.strip()}


def main(arguments: list[str], environment: dict[str, str] | None = None) -> int:
    environment = dict(os.environ) if environment is None else environment
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--base", required=True, help="base ref or commit of the pull request")
    parser.add_argument("--head", default="HEAD", help="head ref or commit (default HEAD)")
    parser.add_argument("--repo", default=".", help="checkout that contains both commits")
    parser.add_argument(
        "--labels",
        default=None,
        help="pull request labels, JSON array or comma separated (default: PR_LABELS)",
    )
    options = parser.parse_args(arguments)
    label_text = options.labels if options.labels is not None else environment.get("PR_LABELS", "")

    try:
        labels = parse_labels(label_text)
        changed = changed_files(options.repo, options.base, options.head)
    except CheckError as error:
        print(f"protected paths: {error}", file=sys.stderr)
        return 2

    protected = sorted(path for path in changed if is_protected(path))
    if not protected:
        print(f"protected paths: none of the {len(changed)} changed files is protected")
        return 0

    approved = LABEL in labels
    stream = sys.stdout if approved else sys.stderr
    print("protected paths changed:", file=stream)
    for path in protected:
        print(f"  {path!r}" if not path.isprintable() else f"  {path}", file=stream)
    if approved:
        print(f"label {LABEL} is present; the Owner's code-owner review is still required")
        return 0
    print(
        f"these paths need the Owner (rule R5). The Owner sets the label {LABEL} to pass this check.",
        file=sys.stderr,
    )
    return 1


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))

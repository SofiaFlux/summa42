#!/usr/bin/env python3
"""Validate local Markdown links and limited GitHub issue-template metadata."""

from __future__ import annotations

import re
import sys
from pathlib import Path
from urllib.parse import unquote, urlparse


FENCE_RE = re.compile(r"^\s{0,3}(`{3,}|~{3,})(.*)$")
HEADING_RE = re.compile(r"^\s{0,3}#{1,6}\s+(.+?)\s*#*\s*$")
LINK_RE = re.compile(r"\[[^]]+\]\(([^)]+)\)")
YAML_KEY_RE = re.compile(r"^(\s*)([A-Za-z_][A-Za-z0-9_-]*):\s*(.*?)\s*$")


class ValidationError(Exception):
    """Raised when a documentation file violates a supported invariant."""


def github_anchor(heading: str) -> str:
    """Return the lowercase, hyphenated anchor GitHub derives from a heading."""
    normalized = heading.strip().lower()
    normalized = re.sub(r"[^a-z0-9_\s-]", "", normalized)
    normalized = re.sub(r"\s+", "-", normalized)
    return normalized


def markdown_anchors(path: Path) -> set[str]:
    """Validate fences and return the unique anchors declared by Markdown headings."""
    fence: str | None = None
    anchors: set[str] = set()
    for line_number, line in enumerate(path.read_text(encoding="utf-8").splitlines(), start=1):
        fence_match = FENCE_RE.match(line)
        if fence_match:
            delimiter = fence_match.group(1)
            if fence is None:
                fence = delimiter
            elif delimiter[0] == fence[0] and len(delimiter) >= len(fence) and not fence_match.group(2).strip():
                fence = None
            continue
        if fence is not None:
            continue
        heading_match = HEADING_RE.match(line)
        if not heading_match:
            continue
        anchor = github_anchor(heading_match.group(1))
        if anchor in anchors:
            raise ValidationError(f"{path}:{line_number}: duplicate heading anchor #{anchor}")
        anchors.add(anchor)
    if fence is not None:
        raise ValidationError(f"{path}: unmatched fenced-code delimiter")
    return anchors


def repo_root(source: Path) -> Path:
    """Return the nearest ancestor holding .git or go.mod, else the source's directory."""
    for candidate in source.resolve().parents:
        if (candidate / ".git").exists() or (candidate / "go.mod").exists():
            return candidate
    return source.resolve().parent


def local_link_target(source: Path, destination: str) -> tuple[Path, str | None] | None:
    """Return a local target and optional fragment, ignoring external links."""
    destination = destination.strip()
    destination = re.split(r"\s+(?=[\"'])", destination, maxsplit=1)[0]
    if re.match(r"(?:https?|mailto):", destination, flags=re.IGNORECASE):
        return None
    path_text, separator, fragment = destination.partition("#")
    if not path_text:
        target = source
    elif path_text.startswith("/"):
        target = repo_root(source) / unquote(path_text).lstrip("/")
    else:
        target = source.parent / unquote(path_text)
    return target, unquote(fragment) if separator else None


def validate_markdown(path: Path) -> None:
    anchors = markdown_anchors(path)
    fence: str | None = None
    for line in path.read_text(encoding="utf-8").splitlines():
        fence_match = FENCE_RE.match(line)
        if fence_match:
            delimiter = fence_match.group(1)
            if fence is None:
                fence = delimiter
            elif delimiter[0] == fence[0] and len(delimiter) >= len(fence) and not fence_match.group(2).strip():
                fence = None
            continue
        if fence is not None:
            continue
        for destination in LINK_RE.findall(line):
            resolved = local_link_target(path, destination)
            if resolved is None:
                continue
            target, fragment = resolved
            if not target.exists():
                raise ValidationError(f"{path}: local link target does not exist: {destination}")
            if fragment is None:
                continue
            if target.suffix.lower() not in {".md", ".markdown"}:
                raise ValidationError(f"{path}: fragment target is not Markdown: {destination}")
            target_anchors = anchors if target.resolve() == path.resolve() else markdown_anchors(target)
            if fragment not in target_anchors:
                raise ValidationError(f"{path}: missing fragment #{fragment} in {target}")


def unquote_yaml(value: str) -> str:
    value = value.strip()
    if len(value) >= 2 and value[0] == value[-1] and value[0] in {"'", '"'}:
        return value[1:-1]
    return value


def contact_links(path: Path) -> list[dict[str, str]]:
    """Extract the simple contact_links mappings used by GitHub issue templates."""
    links: list[dict[str, str]] = []
    in_links = False
    found_links = False
    current: dict[str, str] | None = None
    links_indent = 0
    for line in path.read_text(encoding="utf-8").splitlines():
        key_match = YAML_KEY_RE.match(line)
        if key_match and not key_match.group(1) and key_match.group(2) == "contact_links":
            if key_match.group(3):
                raise ValidationError(f"{path}: contact_links must be a list")
            in_links = True
            found_links = True
            links_indent = len(key_match.group(1))
            current = None
            continue
        if not in_links:
            continue
        if line.strip() and len(line) - len(line.lstrip()) <= links_indent:
            break
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        if stripped.startswith("-"):
            if current is not None:
                links.append(current)
            current = {}
            stripped = stripped[1:].strip()
            if not stripped:
                continue
        if current is None:
            raise ValidationError(f"{path}: contact_links must be a list")
        entry_match = YAML_KEY_RE.match(stripped)
        if entry_match:
            current[entry_match.group(2)] = unquote_yaml(entry_match.group(3))
    if not found_links:
        raise ValidationError(f"{path}: missing top-level contact_links list")
    if in_links and current is not None:
        links.append(current)
    return links


def validate_yaml(path: Path) -> None:
    for index, link in enumerate(contact_links(path), start=1):
        for field in ("name", "about"):
            if not link.get(field, "").strip():
                raise ValidationError(f"{path}: contact_links entry {index} has empty {field}")
        parsed_url = urlparse(link.get("url", ""))
        if parsed_url.scheme != "https" or not parsed_url.hostname:
            raise ValidationError(f"{path}: contact_links entry {index} needs an absolute https:// URL")


def validate(path: Path) -> None:
    if not path.exists():
        raise ValidationError(f"{path}: file does not exist")
    if path.suffix.lower() in {".md", ".markdown"}:
        validate_markdown(path)
    elif path.suffix.lower() in {".yml", ".yaml"}:
        validate_yaml(path)


def main(arguments: list[str]) -> int:
    if not arguments:
        print("usage: verify_docs.py FILE [FILE ...]", file=sys.stderr)
        return 2
    failed = False
    for raw_path in arguments:
        path = Path(raw_path)
        try:
            validate(path)
        except (OSError, ValidationError) as error:
            print(f"invalid: {error}", file=sys.stderr)
            failed = True
        else:
            print(f"valid: {path}")
    return 1 if failed else 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))

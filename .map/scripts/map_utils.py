# MAP-MANAGED: {"generated_by":"mapify-cli","mapify_version":"3.27.0","template_hash":"2eafd050864b4a62d250c9b4c7e9294561f530a7a981bdb706a607b339bd94e5","installed_at":"2026-08-16T18:03:34Z"}
"""Shared utilities for MAP workflow scripts."""

import re
import subprocess


def sanitize_branch_name(branch: str) -> str:
    """Normalize a branch name for safe use as a filesystem path component.

    Replaces ``/`` and any non-``[a-zA-Z0-9_.-]`` character with ``-``,
    collapses runs of hyphens, and strips leading/trailing hyphens. Refuses
    path-traversal patterns (``..`` anywhere, or a leading ``.``) by
    returning ``"default"``. Empty or all-stripped input also yields
    ``"default"`` so callers always get a non-empty, traversal-safe segment.
    """
    if not isinstance(branch, str):
        return "default"
    sanitized = branch.replace("/", "-")
    sanitized = re.sub(r"[^a-zA-Z0-9_.-]", "-", sanitized)
    sanitized = re.sub(r"-+", "-", sanitized).strip("-")
    if ".." in sanitized or sanitized.startswith("."):
        return "default"
    return sanitized or "default"


def get_branch_name() -> str:
    """Get sanitized git branch name.

    Returns the current git branch with unsafe characters replaced by hyphens.
    Falls back to 'default' on any error (not in a git repo, git not installed, etc.).
    """
    try:
        result = subprocess.run(
            ["git", "rev-parse", "--abbrev-ref", "HEAD"],
            capture_output=True,
            text=True,
            timeout=1,
            check=False,
        )
        if result.returncode == 0:
            return sanitize_branch_name(result.stdout.strip())
        return "default"
    except Exception:  # noqa: BLE001 -- deliberate fallback/resilience boundary, must not propagate
        return "default"

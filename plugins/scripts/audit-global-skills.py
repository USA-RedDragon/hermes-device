#!/usr/bin/env python3
"""Report global skills that look project-specific, as candidates to move into a project skill.

    audit-global-skills.py [--skills DIR] [--memories DIR] [--json]

Read-only: it lists, it never moves or edits anything. A skill is a candidate when its text holds
  - an absolute path under a home or work directory (/home/<user>/..., /Users/..., ~/Projects/...),
  - a git remote or repository slug (host/owner/repo, git@host:owner/repo),
  - a private host name or address (*.local, *.lan, *.internal, *.ts.net, private network addresses),
  - the name of a project known to project-memory (its repository name or an alias's).
Defaults: DIR is $HERMES_HOME/skills and $HERMES_HOME/project-memories (HERMES_HOME defaults to ~/.hermes).
A hit is a hint, not a verdict: a skill about git hosting may name github.com on purpose.
"""
from __future__ import annotations

import argparse
import json
import os
import re
import sys
from pathlib import Path

PATTERNS = {
    "path": re.compile(r"(?:/home/[a-z_][\w.-]*/|/Users/[A-Za-z_][\w.-]*/|~/(?:Projects|projects|src|code|work|dev|repos?)/)[\w./~-]*"),
    "repo": re.compile(r"(?:git@|https?://)?(?:github\.com|gitlab\.com|bitbucket\.org|codeberg\.org)[:/][\w.-]+/[\w.-]+"),
    "host": re.compile(r"\b[a-z0-9][a-z0-9-]*(?:\.[a-z0-9-]+)*\.(?:local|lan|internal|home|ts\.net|corp)\b"),
    "address": re.compile(r"\b(?:10\.\d{1,3}|192\.168|172\.(?:1[6-9]|2\d|3[01]))\.\d{1,3}\.\d{1,3}\b"),
}
COMMON_WORDS = {"demo", "test", "tests", "docs", "app", "api", "web", "core", "main", "site", "home", "repo", "code",
                "tools", "scripts", "build", "config", "data", "hermes", "skills", "plugin", "plugins"}
MAX_SHOWN = 5


def project_names(memories: Path) -> dict[str, str]:
    """Lowercase project name or alias repository name -> project slug, for names specific enough to flag."""
    found: dict[str, str] = {}
    try:
        slugs = sorted(p for p in memories.iterdir() if p.is_dir())
    except OSError:
        return found
    for d in slugs:
        try:
            meta = json.loads((d / "meta.json").read_text(encoding="utf-8"))
        except (OSError, ValueError):
            continue
        names = [str(meta.get("name") or "")]
        names += [str(a).rsplit("/", 1)[-1] for a in meta.get("aliases") or [] if isinstance(a, str)]
        for n in names:
            if len(n) >= 4 and n.lower() not in COMMON_WORDS:
                found.setdefault(n.lower(), d.name)
    return found


def scan_text(text: str, names: dict[str, str]) -> list[dict]:
    hits = []
    for kind, rx in PATTERNS.items():
        seen = sorted({m.group(0) for m in rx.finditer(text)})
        if seen:
            hits.append({"kind": kind, "examples": seen[:MAX_SHOWN], "count": len(seen)})
    low = text.lower()
    named = sorted(n for n in names if re.search(rf"(?<![\w-]){re.escape(n)}(?![\w-])", low))
    if named:
        hits.append({"kind": "project", "examples": [f"{n} ({names[n]})" for n in named[:MAX_SHOWN]], "count": len(named)})
    return hits


def audit(skills: Path, memories: Path) -> list[dict]:
    names = project_names(memories)
    out = []
    for md in sorted(skills.rglob("SKILL.md")):
        rel = md.parent.relative_to(skills)
        if any(part.startswith(".") for part in rel.parts):
            continue
        text = "\n".join(p.read_text(encoding="utf-8", errors="replace") for p in
                         [md, *sorted(q for q in md.parent.rglob("*") if q.is_file() and q != md and q.suffix in (".md", ".txt", ".sh", ".py", ".yaml", ".yml", ".json"))][:20])
        hits = scan_text(text, names)
        if hits:
            out.append({"skill": rel.name, "path": rel.as_posix(), "score": sum(h["count"] for h in hits), "hits": hits})
    out.sort(key=lambda r: (-r["score"], r["path"]))
    return out


def main(argv: list[str] | None = None) -> int:
    home = Path(os.environ.get("HERMES_HOME") or Path.home() / ".hermes")
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--skills", type=Path, default=home / "skills")
    ap.add_argument("--memories", type=Path, default=home / "project-memories")
    ap.add_argument("--json", action="store_true")
    args = ap.parse_args(argv)
    if not args.skills.is_dir():
        print(f"no skills directory at {args.skills}", file=sys.stderr)
        return 2
    rows = audit(args.skills, args.memories)
    if args.json:
        print(json.dumps(rows, indent=2))
        return 0
    print(f"{len(rows)} global skill(s) look project-specific (nothing was changed):")
    for r in rows:
        print(f"- {r['path']} (score {r['score']})")
        for h in r["hits"]:
            print(f"    {h['kind']}: " + "; ".join(h["examples"]) + (f" (+{h['count'] - len(h['examples'])} more)" if h["count"] > len(h["examples"]) else ""))
    return 0


if __name__ == "__main__":
    sys.exit(main())

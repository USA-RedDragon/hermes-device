"""Every plugin directory is a complete Hermes plugin."""
import json
import os
import re
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parent.parent
PLUGINS = sorted(p.parent.name for p in ROOT.glob("*/plugin.yaml"))
# Private LAN addresses and personal domains.
PERSONAL = re.compile(r"(?i)\b192\.168\.|\.dev\b|\.pub\b")
# Words that must never appear come from the PRIVACY_DENYLIST environment variable (newline separated; a CI secret).
# Nothing derived from them, not even a hash, is kept in the repo.
DENYLIST = {w.strip().lower() for w in os.environ.get("PRIVACY_DENYLIST", "").splitlines() if w.strip()}


def words(line):
    return re.findall(r"[a-z0-9]+", line.lower())


def manifest(plugin):
    out = {}
    for line in (ROOT / plugin / "plugin.yaml").read_text().splitlines():
        key, _, value = line.partition(": ")
        if value:
            out[key] = value.strip()
    return out


def test_the_repo_has_the_plugins():
    assert PLUGINS == ["project-memory", "subagent-routes"]


@pytest.mark.parametrize("plugin", PLUGINS)
def test_plugin_files(plugin):
    meta = manifest(plugin)
    assert meta["name"] == plugin
    assert re.fullmatch(r"\d+\.\d+\.\d+", meta["version"])
    assert meta["author"] == "Talaria contributors"
    assert meta["requires_hermes"].startswith('">=')
    for name in ("__init__.py", "dashboard/plugin_api.py", "dashboard/manifest.json"):
        assert (ROOT / plugin / name).is_file(), name
    dash = json.loads((ROOT / plugin / "dashboard/manifest.json").read_text())
    assert dash["name"] == plugin and dash["api"] == "plugin_api.py"


@pytest.mark.parametrize("plugin", PLUGINS)
def test_register_exists(plugin):
    assert re.search(r"^def register\(ctx", (ROOT / plugin / "__init__.py").read_text(), re.M)


def scan(match):
    hits = []
    for path in ROOT.rglob("*"):
        if path.is_file() and ".git" not in path.parts and path.suffix in {".py", ".yaml", ".json", ".md", ".sh"}:
            if path.name == "test_package.py":
                continue
            for n, line in enumerate(path.read_text().splitlines(), 1):
                if match(line.replace("USA-RedDragon", "ORG")):  # the GitHub org is the one allowed name
                    hits.append(f"{path.relative_to(ROOT)}:{n}: {line.strip()[:80]}")
    return hits


def test_no_personal_addresses():
    hits = scan(lambda line: bool(PERSONAL.search(line)))
    assert not hits, "\n".join(hits)


@pytest.mark.skipif(not DENYLIST, reason="PRIVACY_DENYLIST is not set (newline separated words): the personal-name check is skipped")
def test_no_personal_names():
    hits = scan(lambda line: any(w in DENYLIST for w in words(line)))
    assert not hits, "\n".join(hits)

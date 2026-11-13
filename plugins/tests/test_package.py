"""Every plugin directory is a complete Hermes plugin."""
import hashlib
import json
import re
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parent.parent
PLUGINS = sorted(p.parent.name for p in ROOT.glob("*/plugin.yaml"))
# Private LAN addresses and personal domains.
PERSONAL = re.compile(r"(?i)\b192\.168\.|\.dev\b|\.pub\b")
# Names that must never appear, kept as SHA-256 of the lowercase word so that this file does not spell them out.
PERSONAL_WORDS = {
    "c35d6aaf3b3885dfc9f36cddf48a65e93a919e13165fbbcfc0f9de5636279559",
    "f5c53d03cec3f622898c7cf08946f88137143c69ab0b22e40c4f76ef0fb6a94d",
    "770edb4ac15711ddcc24c9330c559f76f4c23a201c5f13cf8eeceeb81fe92e67",
    "8d3514d5506b831af3ad83ce0b2442e49b3d4318ba81ecf8ade73f7f8284b083",
    "ab5db4901670272a1c0d9f013ba439d43317ca1fe4dfd6a6ca4b5c7aa7cc3723",
}


def personal(line):
    if PERSONAL.search(line):
        return True
    return any(hashlib.sha256(w.encode()).hexdigest() in PERSONAL_WORDS for w in re.findall(r"[a-z0-9]+", line.lower()))


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


def test_no_personal_identifiers():
    hits = []
    for path in ROOT.rglob("*"):
        if path.is_file() and ".git" not in path.parts and path.suffix in {".py", ".yaml", ".json", ".md", ".sh"}:
            if path.name == "test_package.py":
                continue
            for n, line in enumerate(path.read_text().splitlines(), 1):
                if personal(line.replace("USA-RedDragon", "ORG")):  # the GitHub org is the one allowed name
                    hits.append(f"{path.relative_to(ROOT)}:{n}: {line.strip()[:80]}")
    assert not hits, "\n".join(hits)

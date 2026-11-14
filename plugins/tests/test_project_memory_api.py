import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from conftest import load
from test_project_memory import project

P = "/api/plugins/project-memory"


@pytest.fixture
def client(pm, hermes_home, monkeypatch):
    monkeypatch.setattr(pm, "_config", lambda: {})
    api = load("project-memory/dashboard", "plugin_api.py", "project_memory_plugin_api")
    app = FastAPI()
    app.include_router(api.router, prefix=P)
    return TestClient(app), pm


def test_projects_and_notes_roundtrip(client):
    http, pm = client
    slug = project(pm)
    assert http.get(f"{P}/projects").json()["projects"][0]["id"] == slug
    r = http.post(f"{P}/projects/{slug}/memory/add", json={"content": "hello"})
    assert r.json()["targets"]["memory"]["entries"] == ["hello"]
    r = http.post(f"{P}/projects/{slug}/memory/edit", json={"entry": "hello", "content": "hi"})
    assert r.json()["targets"]["memory"]["entries"] == ["hi"]
    r = http.post(f"{P}/projects/{slug}/memory/remove", json={"entry": "hi"})
    assert r.json()["targets"]["memory"]["entries"] == []
    assert http.get(f"{P}/projects/{slug}").json()["project"]["identity"] == "github.com/alice/demo"


def test_errors(client):
    http, pm = client
    slug = project(pm)
    assert http.get(f"{P}/projects/nope").status_code == 404
    assert http.post(f"{P}/projects/{slug}/memory/zap", json={}).status_code == 404
    assert http.post(f"{P}/projects/{slug}/memory/add", json={"content": ""}).status_code == 400


def test_register(pm):
    mod = load("project-memory", "__init__.py", "hermes_project_memory_plugin")
    seen = {}

    class Ctx:
        def register_tool(self, name, toolset, schema, handler, **kw):
            seen["tool"] = (name, toolset)

        def register_system_prompt_section(self, sid, fn, **kw):
            seen["section"] = sid
            seen["max_chars"] = kw["max_chars"]

        def register_hook(self, name, fn):
            seen.setdefault("hooks", []).append(name)

    mod.register(Ctx())
    assert seen["tool"] == ("project_memory", "project-memory")
    assert 4000 <= seen["max_chars"] <= pm.MAX_CHAR_LIMIT + pm.SECTION_HEAD
    assert seen["section"] == "project-memory" and "subagent_start" in seen["hooks"]


def _skill(pm, slug, rel, name, description):
    d = pm.skills_dir(slug) / rel
    d.mkdir(parents=True)
    (d / "SKILL.md").write_text(f"---\nname: {name}\ndescription: {description}\n---\n\n# {name}\nStep 1.\n", encoding="utf-8")
    (d / "notes.txt").write_text("more", encoding="utf-8")
    return d


def test_project_skills_list_view_delete(client):
    http, pm = client
    slug = project(pm)
    other = project(pm, "github.com/alice/other")
    assert http.get(f"{P}/projects/{slug}/skills").json()["skills"] == []
    _skill(pm, slug, "demo-layout", "demo-layout", "Use when changing the layout of demo.")
    gone = _skill(pm, slug, "ci/demo-release", "demo-release", "Use when releasing demo.")
    _skill(pm, other, "other-ci", "other-ci", "Use for other CI.")
    pm._disabled_skills = lambda: {"demo-release"}

    got = http.get(f"{P}/projects/{slug}/skills").json()
    assert got["project"]["identity"] == "github.com/alice/demo"
    assert [(s["name"], s["path"], s["enabled"]) for s in got["skills"]] == [
        ("demo-layout", "demo-layout", True), ("demo-release", "ci/demo-release", False)]
    assert got["skills"][0]["description"] == "Use when changing the layout of demo."
    assert http.get(f"{P}/projects").json()["projects"][0]["skills"] == 2

    one = http.get(f"{P}/projects/{slug}/skills/demo-release").json()
    assert "Step 1." in one["content"] and one["files"] == ["notes.txt"] and one["name"] == "demo-release"

    after = http.delete(f"{P}/projects/{slug}/skills/demo-release").json()
    assert [s["name"] for s in after["skills"]] == ["demo-layout"]
    assert not gone.exists() and not gone.parent.exists()
    assert [s["name"] for s in http.get(f"{P}/projects/{other}/skills").json()["skills"]] == ["other-ci"]


def test_project_skills_refusals(client, tmp_path):
    http, pm = client
    slug = project(pm)
    other = project(pm, "github.com/alice/other")
    _skill(pm, other, "other-ci", "other-ci", "x")
    outside = tmp_path / "outside"
    outside.mkdir()
    (outside / "SKILL.md").write_text("---\nname: escape\n---\n", encoding="utf-8")
    (pm.skills_dir(slug)).mkdir(parents=True)
    (pm.skills_dir(slug) / "escape").symlink_to(outside, target_is_directory=True)
    for url in (f"{P}/projects/nope/skills", f"{P}/projects/{slug}/skills/other-ci", f"{P}/projects/{slug}/skills/..%2F..%2Fother",
                f"{P}/projects/{slug}/skills/escape"):
        assert http.get(url).status_code == 404, url
    assert http.delete(f"{P}/projects/{slug}/skills/other-ci").status_code == 404
    assert http.delete(f"{P}/projects/{slug}/skills/escape").status_code == 404
    assert (outside / "SKILL.md").exists()
    assert [s["name"] for s in http.get(f"{P}/projects/{other}/skills").json()["skills"]] == ["other-ci"]

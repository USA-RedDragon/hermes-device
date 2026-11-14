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

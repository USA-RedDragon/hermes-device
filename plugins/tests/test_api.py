"""The routes the hub calls, mounted on a bare FastAPI app."""
import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from conftest import load
from test_routes import CONFIG


@pytest.fixture
def client(routes, hermes_home, monkeypatch):
    monkeypatch.setattr(routes, "tip", lambda sid: sid)
    monkeypatch.setattr(routes, "lineage", lambda sid: [sid] if sid else [])
    routes._cache.update(key=None, data={})
    api = load("subagent-routes/dashboard", "plugin_api.py", "subagent_routes_plugin_api")
    monkeypatch.setattr("hermes_cli.config.load_config_readonly", lambda: {"delegation": CONFIG})
    app = FastAPI()
    app.include_router(api.router, prefix="/api/plugins/subagent-routes")
    return TestClient(app)


def test_get_choice_defaults_to_auto(client):
    body = client.get("/api/plugins/subagent-routes/sessions/s1").json()
    assert body["choice"] == "auto"
    assert [c["id"] for c in body["choices"]] == ["auto", "cheap", "mid", "strong", "local", "paid", "help-strong", "none"]
    assert "cheap routes for volume" in body["choices"][0]["line"]


def test_put_choice_is_kept(client):
    url = "/api/plugins/subagent-routes/sessions/s1"
    assert client.put(url, json={"choice": "help-strong"}).json()["choice"] == "help-strong"
    assert client.get(url).json()["choice"] == "help-strong"


@pytest.mark.parametrize("body", [{"choice": "nope"}, {"choice": 3}, {}, [1]])
def test_put_rejects_bad_choices(client, body):
    r = client.put("/api/plugins/subagent-routes/sessions/s1", json=body)
    assert r.status_code == 400 and r.json()["error"] == "bad_choice"

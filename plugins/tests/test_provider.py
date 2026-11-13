"""The delegation provider and the hooks, against Hermes's own base classes."""
import pytest

from agent.delegation_provider import DelegationRouteError
from conftest import load
from test_routes import CONFIG


@pytest.fixture
def plugin(routes, hermes_home, monkeypatch):
    mod = load("subagent-routes", "__init__.py", "hermes_subagent_routes_plugin")
    monkeypatch.setattr(routes, "tip", lambda sid: sid)
    monkeypatch.setattr(routes, "lineage", lambda sid: [sid] if sid else [])
    routes._cache.update(key=None, data={})
    monkeypatch.setattr(mod, "_session_id", lambda: "chat-1")
    return mod


@pytest.fixture
def provider(plugin):
    return plugin._provider()


def test_schema_lists_the_configured_routes(provider):
    params = provider.task_parameters(CONFIG)
    assert params["option"]["enum"] == ["cheap", "mid", "strong", "local", "paid"]
    text = params["option"]["description"]
    assert "cheap: Fast." in text
    assert "paid: Pay per token. Only when the user pins" in text
    assert provider.task_parameters({}) == {}


def test_description_uses_the_configured_sentence(provider):
    text = provider.description(CONFIG)
    assert "Prefer cheap routes for volume work. The user can pin" in text
    bare = {"backends": {"model_map": {"options": CONFIG["backends"]["model_map"]["options"]}}}
    assert "Prefer" not in provider.description(bare)
    assert provider.description({}) == ""


def test_auto_uses_option_or_default(provider):
    assert provider.route_task({"option": "cheap"}, CONFIG)["label"] == "cheap"
    assert provider.route_task({}, CONFIG)["model"] == "m-2"  # the configured default
    with pytest.raises(DelegationRouteError):
        provider.route_task({"option": "paid"}, CONFIG)  # pinned use only


def test_pinned_route_wins_over_option(provider, routes):
    routes.set_choice("chat-1", "strong")
    assert provider.route_task({"option": "cheap"}, CONFIG)["model"] == "m-3"


def test_none_refuses(provider, routes):
    routes.set_choice("chat-1", "none")
    with pytest.raises(DelegationRouteError):
        provider.route_task({}, CONFIG)


def test_help_route_allows_one_helper(provider, routes):
    routes.set_choice("chat-1", "help-strong")
    assert provider.route_task({}, CONFIG) == {}
    with pytest.raises(DelegationRouteError):
        provider.route_task({"option": "cheap"}, CONFIG)
    first = provider.route_task({"option": "strong"}, CONFIG)
    assert first["label"] == "strong (help)"
    with pytest.raises(DelegationRouteError):  # the first helper is still starting
        provider.route_task({"option": "strong"}, CONFIG)


def test_hooks_and_sleep_guard(plugin, routes, monkeypatch):
    registered = {}

    class Ctx:
        def register_delegation_provider(self, p):
            registered["provider"] = p

        def register_hook(self, name, fn):
            registered[name] = fn

        def get_config(self, key, default=None):
            return {"wait_guidance": True, "sleep_guard": True}.get(key, default)

    plugin.register(Ctx())
    assert set(registered) == {"provider", "pre_llm_call", "pre_tool_call", "subagent_stop", "on_session_delete"}
    block = registered["pre_tool_call"]("terminal", {"command": "sleep 120"})
    assert block["action"] == "block"
    assert registered["pre_tool_call"]("terminal", {"command": "ls"}) is None
    ctx = registered["pre_llm_call"](session_id="x", platform="subagent")
    assert "Never use sleep" in ctx["context"]
    routes.set_choice("chat-2", "strong")
    monkeypatch.setattr("tools.delegate_tool_config._load_config", lambda: CONFIG)
    ctx = registered["pre_llm_call"](session_id="chat-2", platform="cli", conversation_history=[])
    assert "route `strong`" in ctx["context"] and "Never use sleep" in ctx["context"]
    registered["on_session_delete"](session_id="chat-2")
    assert routes.stored_choice("chat-2")[0] is None

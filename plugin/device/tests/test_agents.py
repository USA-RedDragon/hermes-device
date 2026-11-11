"""device-agents toolset tests: the request bus, the inbox for running agents, delivery into
subagents and the hub's routes. Run inside the Hermes runtime with scripts/test-plugin.sh."""
from __future__ import annotations

import json
import sys
import threading
import time
import types
import unittest
from pathlib import Path
from unittest import mock

from fastapi import FastAPI
from starlette.testclient import TestClient

sys.path.insert(0, str(Path(__file__).resolve().parent))
from test_core import core  # noqa: E402
from test_routes import plugin_api  # noqa: E402

agents = core._load_sibling("hermes_device_agents", "agents.py")


class Clock:
    def __init__(self):
        self.t = 1000.0

    def __call__(self):
        return self.t


def hub_answers(bus, reply, polls=1):
    """A hub that takes requests and answers each with reply(request)."""
    def run():
        for _ in range(polls):
            for req in bus.take(2.0):
                bus.answer(req["id"], reply(req))
    th = threading.Thread(target=run, daemon=True)
    th.start()
    return th


class BusTest(unittest.TestCase):
    def test_not_connected(self):
        bus = agents.Bus()
        with self.assertRaises(agents.HubUnavailable):
            bus.call("peers", {"session_id": "s1"}, {})

    def test_round_trip(self):
        bus = agents.Bus()
        bus.take(0)
        seen = []

        def reply(req):
            seen.append(req)
            return {"result": {"ok": req["args"]["x"]}}
        hub_answers(bus, reply)
        self.assertEqual(bus.call("send", {"session_id": "s1"}, {"x": 7}), {"ok": 7})
        self.assertEqual(seen[0]["op"], "send")
        self.assertEqual(seen[0]["caller"], {"session_id": "s1"})
        self.assertTrue(seen[0]["id"].startswith("ar_"))

    def test_refusal(self):
        bus = agents.Bus()
        bus.take(0)
        hub_answers(bus, lambda req: {"error": {"code": "rate_limited", "message": "slow down"}})
        with self.assertRaises(agents.HubRefused) as cm:
            bus.call("send", {"session_id": "s1"}, {})
        self.assertEqual(cm.exception.code, "rate_limited")

    def test_timeout_withdraws_request(self):
        bus = agents.Bus()
        bus.take(0)
        with self.assertRaises(agents.HubUnavailable):
            bus.call("peers", {"session_id": "s1"}, {}, timeout=0.05)
        self.assertEqual(bus.take(0), [])
        self.assertFalse(bus.answer("ar_nope", {"result": 1}))

    def test_session_quota_and_notices(self):
        bus = agents.Bus()
        bus.take(0)
        threads = [threading.Thread(target=lambda: self.assertRaises(
            agents.HubUnavailable, bus.call, "peers", {"session_id": "s1"}, {}, 0.3)) for _ in range(agents.MAX_PER_SESSION)]
        for th in threads:
            th.start()
        deadline = time.monotonic() + 2
        while len(bus._queue) < agents.MAX_PER_SESSION and time.monotonic() < deadline:
            time.sleep(0.01)
        with self.assertRaises(agents.HubUnavailable):
            bus.call("peers", {"session_id": "s1"}, {}, 0.1)
        for _ in range(agents.MAX_QUEUE):
            bus.notify("undelivered", {"session_id": "s1"}, {"message_id": "am_1", "session_id": "s1"})
        for th in threads:
            th.join(2)
        self.assertEqual(sum(1 for r in bus.take(0) if r["op"] == "undelivered"), agents.MAX_TAKE)

    def test_stale_hub(self):
        clock = Clock()
        bus = agents.Bus(now=clock)
        bus.take(0)
        self.assertTrue(bus.connected())
        clock.t += agents.HUB_STALE_S + 1
        self.assertFalse(bus.connected())


class InboxTest(unittest.TestCase):
    def setUp(self):
        self.clock = Clock()
        self.bus = agents.Bus(now=self.clock)
        self.inbox = agents.Inbox(self.bus, now=self.clock)

    def test_tool_result_carries_messages_once(self):
        self.inbox.turn_started("s1")
        self.assertEqual(self.inbox.put("s1", "am_1", "[Agent message am_1 from x]\nhi"), "queued")
        with mock.patch.object(agents, "INBOX", self.inbox):
            out = agents.transform_tool_result("terminal", '{"output": "ok"}', session_id="s1")
            self.assertEqual(out, '{"output": "ok"}\n\n[Agent message am_1 from x]\nhi')
            self.assertIsNone(agents.transform_tool_result("terminal", "x", session_id="s1"))
            self.assertIsNone(agents.transform_tool_result("terminal", "x", session_id="s2"))

    def test_turn_end_returns_unread(self):
        self.inbox.turn_started("s1")
        self.inbox.put("s1", "am_1", "hi")
        self.inbox.turn_ended("s1")
        self.assertFalse(self.inbox.running("s1"))
        reqs = self.bus.take(0)
        self.assertEqual([(r["op"], r["args"]) for r in reqs],
                         [("undelivered", {"message_id": "am_1", "session_id": "s1"})])

    def test_stale_items_return(self):
        self.inbox.put("s1", "am_1", "hi")
        self.clock.t += agents.INBOX_TTL_S + 1
        self.assertEqual(self.inbox.take("s1"), [])
        self.assertEqual([r["args"]["message_id"] for r in self.bus.take(0)], ["am_1"])

    def test_full(self):
        for i in range(agents.MAX_INBOX):
            self.assertEqual(self.inbox.put("s1", f"am_{i}", "x"), "queued")
        self.assertEqual(self.inbox.put("s1", "am_x", "x"), "full")
        self.assertEqual(self.inbox.put("s2", "am_y", "x", running_only=True), "not_running")


def fake_registry(records):
    lock = threading.Lock()
    steered = []

    def steer(sid, text):
        if sid in records:
            steered.append((sid, text))
            return True
        return False
    mod = types.SimpleNamespace(_active_subagents_lock=lock, _active_subagents=records, steer_subagent=steer)
    return mod, steered


def record(sid, session, parent="", owner="s1", depth=0, goal="g"):
    return {"subagent_id": sid, "agent": types.SimpleNamespace(session_id=session), "parent_id": parent,
            "depth": depth, "goal": goal, "owner_agent_session_id": owner, "accepting_steer": True}


class TreeTest(unittest.TestCase):
    def setUp(self):
        self.reg, self.steered = fake_registry({
            "sa-0-a": record("sa-0-a", "c1"),
            "sa-1-b": record("sa-1-b", "c2"),
            "sa-0-c": record("sa-0-c", "c3", parent="sa-0-a", owner="c1", depth=1),
            "sa-0-z": record("sa-0-z", "z1", owner="other"),
        })
        tools = types.ModuleType("tools")
        tools.delegate_tool_registry = self.reg
        self.patch = mock.patch.dict(sys.modules, {"tools": tools, "tools.delegate_tool_registry": self.reg})
        self.patch.start()
        self.addCleanup(self.patch.stop)
        lifecycle = mock.patch.object(agents, "_LIFECYCLE", None)
        lifecycle.start()
        self.addCleanup(lifecycle.stop)

    def test_caller_of_subagent(self):
        c = agents.caller_of("c3")
        self.assertEqual(c["root_session_id"], "s1")
        self.assertEqual(c["subagent_id"], "sa-0-c")
        self.assertEqual(c["parent_session_id"], "c1")
        self.assertEqual(c["parent_subagent_id"], "sa-0-a")
        self.assertEqual(sorted(s["subagent_id"] for s in c["tree"]), ["sa-0-a", "sa-0-c", "sa-1-b"])

    def test_caller_of_chat(self):
        c = agents.caller_of("s1")
        self.assertEqual(c["root_session_id"], "s1")
        self.assertNotIn("subagent_id", c)
        self.assertEqual(len(c["tree"]), 3)

    def test_deliver(self):
        inbox = agents.Inbox(agents.Bus())
        with mock.patch.object(agents, "INBOX", inbox):
            self.assertEqual(agents.deliver({"mode": "steer", "message_id": "am_1", "session_id": "c1",
                                             "subagent_id": "sa-0-a", "text": "stop"}), {"status": "steered"})
            self.assertEqual(self.steered, [("sa-0-a", "stop")])
            self.assertEqual(agents.deliver({"mode": "steer", "message_id": "am_1", "session_id": "c9",
                                             "subagent_id": "sa-0-a", "text": "x"}), {"status": "gone"})
            self.assertEqual(agents.deliver({"mode": "inbox", "message_id": "am_2", "session_id": "c2",
                                             "subagent_id": "sa-1-b", "text": "hi"}), {"status": "queued"})
            self.assertEqual([i["message_id"] for i in inbox.take("c2")], ["am_2"])
            self.assertEqual(agents.deliver({"mode": "inbox", "message_id": "am_3", "session_id": "s1",
                                             "text": "hi"}), {"status": "not_running"})
            inbox.turn_started("s1")
            self.assertEqual(agents.deliver({"mode": "inbox", "message_id": "am_3", "session_id": "s1",
                                             "text": "hi"}), {"status": "queued"})
            with self.assertRaises(ValueError):
                agents.deliver({"mode": "steer", "message_id": "am_4", "session_id": "s1", "text": "x"})
            with self.assertRaises(ValueError):
                agents.deliver({"mode": "shout", "message_id": "am_4", "session_id": "s1", "text": "x"})


def snapshot(sid, session, parent="", owner="s1", depth=0, goal="g", steerable=True):
    """An ActiveSubagent of hermes-patches 0005."""
    return types.SimpleNamespace(subagent_id=sid, session_id=session, parent_subagent_id=parent or None,
                                 parent_session_id=owner or None, depth=depth, goal=goal, model="m",
                                 status="running", started_at=1.0, steerable=steerable)


class FakeLifecycle:
    def __init__(self, active):
        self.active = active
        self.steered = []

    def list_active(self):
        return tuple(self.active)

    def steer_active(self, subagent_id, text):
        if any(a.subagent_id == subagent_id and a.steerable for a in self.active):
            self.steered.append((subagent_id, text))
            return True
        return False


class LifecycleTreeTest(unittest.TestCase):
    """With subagent_lifecycle.list_active and steer_active, the plugin never reads the registry."""

    def setUp(self):
        self.lifecycle = FakeLifecycle([
            snapshot("sa-0-a", "c1"),
            snapshot("sa-1-b", "c2"),
            snapshot("sa-0-c", "c3", parent="sa-0-a", owner="c1", depth=1),
            snapshot("sa-0-z", "z1", owner="other"),
            snapshot("sa-0-q", "c4", steerable=False),
            snapshot("sa-0-n", None),
        ])
        patch = mock.patch.object(agents, "_LIFECYCLE", self.lifecycle)
        patch.start()
        self.addCleanup(patch.stop)
        broken = types.ModuleType("tools")
        modules = mock.patch.dict(sys.modules, {"tools": broken, "tools.delegate_tool_registry": None})
        modules.start()
        self.addCleanup(modules.stop)

    def test_caller_of_subagent(self):
        c = agents.caller_of("c3")
        self.assertEqual(c["root_session_id"], "s1")
        self.assertEqual((c["subagent_id"], c["parent_session_id"], c["parent_subagent_id"]),
                         ("sa-0-c", "c1", "sa-0-a"))
        self.assertEqual(sorted(s["subagent_id"] for s in c["tree"]), ["sa-0-a", "sa-0-c", "sa-1-b"])
        self.assertEqual(agents.caller_of("c1")["parent_subagent_id"], "")

    def test_deliver_steers_through_the_lifecycle(self):
        inbox = agents.Inbox(agents.Bus())
        with mock.patch.object(agents, "INBOX", inbox):
            self.assertEqual(agents.deliver({"mode": "steer", "message_id": "am_1", "session_id": "c1",
                                             "subagent_id": "sa-0-a", "text": "stop"}), {"status": "steered"})
            self.assertEqual(agents.deliver({"mode": "steer", "message_id": "am_2", "session_id": "c4",
                                             "subagent_id": "sa-0-q", "text": "x"}), {"status": "gone"})
            self.assertEqual(agents.deliver({"mode": "inbox", "message_id": "am_3", "session_id": "c2",
                                             "subagent_id": "sa-1-b", "text": "hi"}), {"status": "queued"})
        self.assertEqual(self.lifecycle.steered, [("sa-0-a", "stop")])


class LifecycleContractTest(unittest.TestCase):
    def test_snapshot_fields_match_hermes(self):
        try:
            from agent.subagent_lifecycle import ActiveSubagent, SubagentLifecycleService
        except ImportError:
            self.skipTest("this Hermes has no subagent_lifecycle.list_active")
        import dataclasses
        names = {f.name for f in dataclasses.fields(ActiveSubagent)}
        self.assertLessEqual({"subagent_id", "session_id", "parent_subagent_id", "parent_session_id", "depth",
                              "goal", "steerable"}, names)
        for name in ("list_active", "steer_active"):
            self.assertTrue(callable(getattr(SubagentLifecycleService, name)), name)


class RecordingCtx:
    def __init__(self, lifecycle=None):
        self.hooks: dict[str, list] = {}
        if lifecycle is not None:
            self.subagent_lifecycle = lifecycle

    def register_tool(self, *args, **kwargs):
        pass

    def register_hook(self, name, callback):
        self.hooks.setdefault(name, []).append(callback)


class TurnHookTest(unittest.TestCase):
    def setUp(self):
        self.inbox = agents.Inbox(agents.Bus())
        for patch in (mock.patch.object(agents, "INBOX", self.inbox), mock.patch.object(agents, "_LIFECYCLE", None)):
            patch.start()
            self.addCleanup(patch.stop)

    def register(self, hooks, lifecycle=None):
        ctx = RecordingCtx(lifecycle)
        agents.register(ctx, types.SimpleNamespace(valid_hooks=lambda: frozenset(hooks)))
        return ctx

    def test_turn_hooks_when_hermes_has_them(self):
        lifecycle = FakeLifecycle([])
        ctx = self.register({"pre_llm_call", "post_llm_call", "agent_turn_start", "agent_turn_end"}, lifecycle)
        self.assertEqual(sorted(ctx.hooks), ["agent_turn_end", "agent_turn_start", "transform_tool_result"])
        self.assertIs(agents._LIFECYCLE, lifecycle)

    def test_llm_call_hooks_on_an_older_hermes(self):
        ctx = self.register({"pre_llm_call", "post_llm_call"}, types.SimpleNamespace(launch=lambda r: None))
        self.assertEqual(sorted(ctx.hooks), ["post_llm_call", "pre_llm_call", "transform_tool_result"])
        self.assertIsNone(agents._LIFECYCLE)

    def test_failed_or_stopped_turn_ends_running_state(self):
        ctx = self.register({"agent_turn_start", "agent_turn_end"})
        for outcome in ("failed", "cancelled", "timed_out", "success"):
            payload = {"session_id": "s1", "turn_id": "t1", "task_id": "k", "platform": "tui", "model": "m"}
            ctx.hooks["agent_turn_start"][0](**payload)
            self.assertTrue(self.inbox.running("s1"))
            self.assertEqual(self.inbox.put("s1", "am_1", "hi", running_only=True), "queued")
            ctx.hooks["agent_turn_end"][0](**payload, outcome=outcome)
            self.assertFalse(self.inbox.running("s1"), outcome)
            self.assertEqual(self.inbox.put("s1", "am_2", "hi", running_only=True), "not_running")
            self.assertEqual(self.inbox.take("s1"), [])

    def test_hermes_turn_events_reach_the_hooks(self):
        """In a Hermes with hermes-patches 0005, a failed turn's real agent_turn_end clears the state."""
        try:
            from agent.turn_lifecycle_events import fire_turn_end, fire_turn_start
            from hermes_cli import plugins
        except ImportError:
            self.skipTest("this Hermes has no agent_turn_start/agent_turn_end")
        ctx = self.register(plugins.VALID_HOOKS)
        manager = plugins.get_plugin_manager()
        hooks = {name: ctx.hooks[name] for name in ("agent_turn_start", "agent_turn_end")}
        with mock.patch.object(manager, "_discovered", True), mock.patch.dict(manager._hooks, hooks):
            agent = types.SimpleNamespace(session_id="s9", platform="tui", model="m", _persist_disabled=False)
            started = fire_turn_start(agent, turn_id="t9", task_id="k9")
            self.assertTrue(self.inbox.running("s9"))
            fire_turn_end(started, outcome="failed")
        self.assertFalse(self.inbox.running("s9"))


class ToolTest(unittest.TestCase):
    def test_send_checks_arguments_before_the_hub(self):
        bus = agents.Bus()
        bus.take(0)
        with mock.patch.object(agents, "BUS", bus), mock.patch.object(agents, "caller_of", lambda sid: {"session_id": sid}):
            self.assertEqual(json.loads(agents._send({"to": "", "text": "x"}, session_id="s1"))["code"], "bad_arguments")
            self.assertEqual(json.loads(agents._send({"to": "a", "text": "x" * (agents.MAX_TEXT + 1)},
                                                     session_id="s1"))["code"], "too_long")
            self.assertEqual(bus.take(0), [])
            hub_answers(bus, lambda req: {"result": {"message_id": "am_1", "seen": req["args"],
                                                     "caller": req["caller"]}})
            out = json.loads(agents._send({"to": " b ", "text": "hi", "reply_to": "am_0"}, session_id="s1",
                                          task_id="t"))
        self.assertEqual(out["seen"], {"to": "b", "text": "hi", "reply_to": "am_0"})
        self.assertEqual(out["caller"], {"session_id": "s1"})

    def test_offered_needs_hub(self):
        bus = agents.Bus()
        with mock.patch.object(agents, "BUS", bus):
            self.assertFalse(agents.offered())
            bus.take(0)
            self.assertTrue(agents.offered())


class RoutesTest(unittest.TestCase):
    def setUp(self):
        app = FastAPI()
        app.include_router(plugin_api.router, prefix="/api/plugins/device")
        self.client = TestClient(app)
        self.bus = agents.Bus()
        p = mock.patch.object(agents, "BUS", self.bus)
        p.start()
        self.addCleanup(p.stop)

    def test_poll_and_answer(self):
        result = {}

        def tool():
            result["v"] = self.bus.call("peers", {"session_id": "s1"}, {})
        self.bus.take(0)
        th = threading.Thread(target=tool)
        th.start()
        r = self.client.get("/api/plugins/device/agents/requests?wait=2")
        self.assertEqual(r.status_code, 200)
        reqs = r.json()["requests"]
        self.assertEqual(len(reqs), 1)
        r = self.client.post(f"/api/plugins/device/agents/requests/{reqs[0]['id']}", json={"result": {"chats": []}})
        self.assertEqual(r.status_code, 204)
        th.join(5)
        self.assertEqual(result["v"], {"chats": []})
        r = self.client.post(f"/api/plugins/device/agents/requests/{reqs[0]['id']}", json={"result": 1})
        self.assertEqual(r.status_code, 404)
        r = self.client.post("/api/plugins/device/agents/requests/x", json={"result": 1, "error": {}})
        self.assertEqual(r.status_code, 400)

    def test_empty_poll_returns(self):
        t0 = time.monotonic()
        r = self.client.get("/api/plugins/device/agents/requests?wait=0.2")
        self.assertEqual(r.json(), {"requests": []})
        self.assertLess(time.monotonic() - t0, 2)

    def test_deliver_route(self):
        r = self.client.post("/api/plugins/device/agents/deliver", json={"mode": "x"})
        self.assertEqual(r.status_code, 400)
        r = self.client.post("/api/plugins/device/agents/deliver",
                             json={"mode": "inbox", "message_id": "am_1", "session_id": "s-idle", "text": "hi"})
        self.assertEqual(r.json(), {"status": "not_running"})


if __name__ == "__main__":
    unittest.main()

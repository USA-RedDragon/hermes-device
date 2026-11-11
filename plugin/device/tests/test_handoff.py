"""Restart handoff tests (docs/protocol.md section 4.7): the old hub hands device commands to
the next gateway's hub through handoff.json, and the exec route refuses legs while it does. Run
inside the Hermes runtime with scripts/test-plugin.sh."""
from __future__ import annotations

import json
import os
import sys
import tempfile
import threading
import time
import types
import unittest
from pathlib import Path
from unittest import mock

from fastapi import FastAPI
from starlette.testclient import TestClient
from starlette.websockets import WebSocketDisconnect

sys.path.insert(0, str(Path(__file__).resolve().parent))
from test_core import Store, core  # noqa: E402
from test_resume import Recorder, output  # noqa: E402
from test_routes import PATH, Signer, plugin_api  # noqa: E402

handoff = core._load_sibling("hermes_device_handoff", "handoff.py")


def reset_handoff():
    handoff.handing_off = False
    handoff._handed.clear()
    handoff._loaded.clear()


class Leg(Recorder):
    """A version 3 exec leg whose close ends like the route's handler: the hub detaches it,
    resumable unless the code is 1012."""

    def __init__(self, hub, device_id: str = "d_pc"):
        super().__init__(3, device_id)

        async def close(code, reason):
            self.closed.append((code, reason))
            hub.detach(self.conn, resumable=code != core.CLOSE_SERVICE_RESTART)

        self.conn.close = close


def session_of(proc):
    return types.SimpleNamespace(process=proc)


class HubHandoffTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(dir=os.environ.get("TMPDIR"))
        self.addCleanup(self.tmp.cleanup)
        self.state = Path(self.tmp.name) / "device" / "bindings.json"
        self.hub = core.Hub(self.state, parent_lookup=Store().lookup)
        self.cfg = {"backend_id": "alice", "grace_seconds": 30}
        for p in (mock.patch.object(core, "settings", lambda: self.cfg),
                  mock.patch.object(core, "hub", lambda: self.hub)):
            p.start()
            self.addCleanup(p.stop)
        reset_handoff()
        self.addCleanup(reset_handoff)

    def leg(self, device_id="d_pc") -> Leg:
        leg = Leg(self.hub, device_id)
        self.addCleanup(leg.stop)
        return leg

    def start(self, device_id="d_pc"):
        return self.closing(self.hub.start(device_id, cmd="make", cwd="/home/alice/src", login=False, timeout=60,
                                           stdin=None))

    def closing(self, proc):
        if proc is not None:
            self.addCleanup(proc.stdout.close)
        return proc

    def next_gateway(self) -> core.Hub:
        reset_handoff()
        self.hub = core.Hub(self.state, parent_lookup=Store().lookup)
        return self.hub

    def test_round_trip(self):
        first = self.leg()
        self.hub.attach(first.conn)
        proc = self.start()
        rid = first.execs()[0]["id"]
        self.hub.on_message(first.conn, {"type": "output", "id": rid, "data": "one\n", "offset": 0})
        self.assertEqual(proc.stdout.readline(), "one\n")
        handoff.prepare()
        self.assertEqual(first.closed, [(1001, "gateway handoff")])
        self.assertIsNone(self.hub.device("d_pc"))
        state = handoff.detach(session_of(proc))
        self.assertEqual(state, {"device_id": "d_pc", "rid": rid, "offset": 4, "cwd": "/home/alice/src"})
        self.assertEqual((self.hub._parked, self.hub._park_timers), ({}, {}))
        handoff.commit()
        path = self.state.with_name("handoff.json")
        doc = json.loads(path.read_text())
        self.assertEqual(doc["commands"], [state])
        self.assertIsNone(proc.wait(0.3), "the old gateway must not fail a handed-off command")

        new_hub = self.next_gateway()
        new_proc = self.closing(handoff.reattach(state))
        self.assertIsNotNone(new_proc)
        self.assertFalse(path.exists())
        self.assertEqual((new_proc.request_id, new_proc.received), (rid, 4))
        self.assertIsNone(handoff.reattach(state))
        second = self.leg()
        self.assertEqual(new_hub.attach(second.conn), [{"id": rid, "offset": 4}])
        new_hub.on_message(second.conn, {"type": "output", "id": rid, "data": "two\n", "offset": 4})
        new_hub.on_message(second.conn, {"type": "exit", "id": rid, "code": 0})
        self.assertEqual(new_proc.wait(5), 0)
        self.assertEqual(output(new_proc), "two\n")

    def test_old_grace_expiry_does_not_fail_handed_off(self):
        self.cfg["grace_seconds"] = 0.2
        first = self.leg()
        self.hub.attach(first.conn)
        proc = self.start()
        kept = self.start()
        handoff.prepare()
        self.assertIsNotNone(handoff.detach(session_of(proc)))
        self.assertEqual(kept.wait(5), core.EXIT_LOST, "a command not handed off keeps its grace")
        output(kept)
        self.assertIsNone(proc.wait(0.5))

    def test_command_parked_by_an_earlier_drop(self):
        first = self.leg()
        self.hub.attach(first.conn)
        proc = self.start()
        rid = first.execs()[0]["id"]
        self.hub.detach(first.conn, resumable=True)
        other = self.leg("d_lap")
        self.hub.attach(other.conn)
        handoff.prepare()
        self.assertEqual(other.closed, [(1001, "gateway handoff")])
        self.assertEqual(handoff.detach(session_of(proc))["rid"], rid)
        self.assertNotIn("d_pc", self.hub._park_timers)
        handoff.commit()
        new_hub = self.next_gateway()
        self.assertEqual(handoff.attach(new_hub, self.leg().conn), [{"id": rid, "offset": 0}])
        self.assertIsNotNone(self.closing(handoff.reattach({"rid": rid, "device_id": "d_pc", "offset": 0})))

    def test_detach_declines(self):
        first = self.leg()
        self.hub.attach(first.conn)
        proc = self.start()
        self.assertIsNone(handoff.detach(session_of(proc)), "no prepare, no handoff")
        handoff.prepare()
        self.assertIsNone(handoff.detach(types.SimpleNamespace(process=object())))
        self.assertIsNone(handoff.detach(types.SimpleNamespace()))
        done = self.closing(core.finished_process(0, ""))
        self.assertIsNone(handoff.detach(session_of(done)))
        handoff.commit()
        self.assertFalse(self.state.with_name("handoff.json").exists())

    def test_grace_zero_turns_handoff_off(self):
        self.cfg["grace_seconds"] = 0
        first = self.leg()
        self.hub.attach(first.conn)
        proc = self.start()
        handoff.prepare()
        self.assertFalse(handoff.handing_off)
        self.assertEqual(first.closed, [])
        self.assertIsNone(handoff.detach(session_of(proc)))

    def test_new_legs_refused_while_handing_off(self):
        handoff.prepare()
        self.assertTrue(handoff.handing_off)
        leg = self.leg()
        self.assertIsNone(handoff.attach(self.hub, leg.conn))
        self.assertIsNone(self.hub.device("d_pc"))

    def write(self, doc) -> Path:
        path = self.state.with_name("handoff.json")
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(doc if isinstance(doc, str) else json.dumps(doc))
        return path

    def test_load_drops_expired_and_bad_files(self):
        rid = "a" * 32
        entry = {"device_id": "d_pc", "rid": rid, "offset": 7, "cwd": "/home/alice/src"}
        for doc in ({"version": 1, "written_at": time.time() - 60, "commands": [entry]}, "{not json",
                    {"version": 2, "written_at": time.time(), "commands": [entry]},
                    {"version": 1, "written_at": time.time(),
                     "commands": [{**entry, "rid": "../x"}, {**entry, "offset": -1}, {**entry, "device_id": 3}]}):
            with self.subTest(doc=doc):
                self.cfg["grace_seconds"] = 30
                path = self.write(doc)
                handoff.load(self.hub)
                self.assertFalse(path.exists())
                self.assertEqual(self.hub._parked, {})
                self.assertIsNone(handoff.reattach(entry))

    def test_loaded_commands_expire_after_the_rest_of_the_grace(self):
        self.cfg["grace_seconds"] = 1
        rid = "b" * 32
        self.write({"version": 1, "written_at": time.time() - 0.7,
                    "commands": [{"device_id": "d_pc", "rid": rid, "offset": 0, "cwd": ""}]})
        began = time.monotonic()
        proc = handoff.reattach({"rid": rid})
        self.assertEqual(proc.wait(5), core.EXIT_LOST)
        self.assertLess(time.monotonic() - began, 0.9)
        self.assertIn("did not reconnect", output(proc))

    def test_kill_of_reattached_command_before_the_device_returns(self):
        rid = "c" * 32
        self.write({"version": 1, "written_at": time.time(),
                    "commands": [{"device_id": "d_pc", "rid": rid, "offset": 0, "cwd": ""}]})
        proc = handoff.reattach({"rid": rid})
        proc.kill()
        self.assertEqual(proc.wait(1), core.EXIT_KILLED)
        output(proc)
        self.assertEqual(self.hub.attach(self.leg().conn), [])


class RegisterTests(unittest.TestCase):
    def test_registers_only_when_the_registry_offers_it(self):
        for has_api in (False, True):
            with self.subTest(has_api=has_api):
                registry = types.SimpleNamespace()
                if has_api:
                    registry.register_reattach_provider = mock.Mock()
                module = types.ModuleType("tools.process_registry")
                module.process_registry = registry
                with mock.patch.dict(sys.modules, {"tools.process_registry": module}):
                    handoff.register(object(), core)
                if has_api:
                    registry.register_reattach_provider.assert_called_once_with("device", handoff)


class RouteHandoffTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(dir=os.environ.get("TMPDIR"))
        self.addCleanup(self.tmp.cleanup)
        base = Path(os.path.realpath(self.tmp.name))
        self.root = base / "src"
        self.root.mkdir()
        self.state = base / "device" / "bindings.json"
        self.hub = core.Hub(self.state, parent_lookup=Store().lookup)
        self.signer = Signer()
        self.cfg = {"backend_id": "alice", "placeholder": str(base / "ph"), "hub_public_key_env": "TEST_HUB_KEY",
                    "grace_seconds": 30}
        patches = [mock.patch.object(core, "hub", lambda: self.hub),
                   mock.patch.object(core, "settings", lambda: self.cfg),
                   mock.patch.object(core, "hermes_version", lambda: "v0.23.1"),
                   mock.patch.dict(os.environ, {"TEST_HUB_KEY": self.signer.public_b64})]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)
        reset_handoff()
        self.addCleanup(reset_handoff)
        app = FastAPI()
        app.include_router(plugin_api.router, prefix="/api/plugins/device")
        self.client = TestClient(app)
        self.addCleanup(self.client.close)

    def connect(self):
        msg = {"type": "hello", "version": 2, "max_version": 3, "roots": [str(self.root)], "tmp_dir": str(self.root),
               "home": str(self.root), "hostname": "desktop", "os": "linux", "arch": "amd64", "shell": "bash",
               "client_version": "v0.3.0"}
        cm = self.client.websocket_connect(PATH, headers={"X-Hermes-Device-Assertion": self.signer.sign()})
        ws = cm.__enter__()
        self.addCleanup(lambda: cm.__exit__(None, None, None))
        ws.send_json(msg)
        return ws

    def close_of(self, ws) -> WebSocketDisconnect:
        with self.assertRaises(WebSocketDisconnect) as ctx:
            while True:
                ws.receive_json()
        return ctx.exception

    def test_handoff_over_the_route(self):
        ws = self.connect()
        ws.receive_json()
        self.assertEqual(ws.receive_json(), {"type": "resume", "commands": []})
        proc = self.hub.start("d_8f2c", cmd="make", cwd=str(self.root), login=False, timeout=60, stdin=None)
        self.addCleanup(proc.stdout.close)
        rid = ws.receive_json()["id"]
        ws.send_json({"type": "output", "id": rid, "data": "one\n", "offset": 0})
        self.assertEqual(proc.stdout.readline(), "one\n")
        prepared = threading.Thread(target=handoff.prepare)
        prepared.start()
        self.assertEqual(self.close_of(ws).code, 1001)
        ws.close()
        prepared.join(10)
        self.assertIsNone(self.hub.device("d_8f2c"))
        self.assertIsNone(proc.poll(), "a leg closed for a handoff is parked")
        state = handoff.detach(session_of(proc))
        self.assertEqual(state["offset"], 4)
        handoff.commit()

        refused = self.connect()
        gone = self.close_of(refused)
        self.assertEqual((gone.code, gone.reason), (1013, "gateway handoff in progress"))
        self.assertIsNone(proc.wait(0.2))

        reset_handoff()
        self.hub = core.Hub(self.state, parent_lookup=Store().lookup)
        ws2 = self.connect()
        self.assertEqual(ws2.receive_json()["type"], "welcome")
        self.assertEqual(ws2.receive_json(), {"type": "resume", "commands": [{"id": rid, "offset": 4}]})
        new_proc = handoff.reattach(state)
        ws2.send_json({"type": "output", "id": rid, "data": "two\n", "offset": 4})
        ws2.send_json({"type": "exit", "id": rid, "code": 0})
        self.assertEqual(new_proc.wait(5), 0)
        self.assertEqual(output(new_proc), "two\n")


@unittest.skipUnless(hasattr(__import__("tools.process_registry", fromlist=["ProcessRegistry"]).ProcessRegistry,
                             "register_reattach_provider"), "Hermes without process handoff")
class RegistryHandoffTests(unittest.TestCase):
    """Against Hermes's own process registry, where it offers the handoff."""

    def setUp(self):
        from tools import process_registry as pr
        self.pr = pr
        self.tmp = tempfile.TemporaryDirectory(dir=os.environ.get("TMPDIR"))
        self.addCleanup(self.tmp.cleanup)
        self.state = Path(self.tmp.name) / "device" / "bindings.json"
        self.hub = core.Hub(self.state, parent_lookup=Store().lookup)
        self.cfg = {"backend_id": "alice", "grace_seconds": 30}
        for p in (mock.patch.object(core, "settings", lambda: self.cfg),
                  mock.patch.object(core, "hub", lambda: self.hub),
                  mock.patch.object(pr, "CHECKPOINT_PATH", Path(self.tmp.name) / "processes.json")):
            p.start()
            self.addCleanup(p.stop)
        reset_handoff()
        self.addCleanup(reset_handoff)

    def registry(self):
        registry = self.pr.ProcessRegistry()
        registry.register_reattach_provider("device", handoff)
        return registry

    def test_registry_round_trip(self):
        leg = Leg(self.hub)
        self.addCleanup(leg.stop)
        self.hub.attach(leg.conn)
        proc = self.hub.start("d_pc", cmd="make", cwd="/home/alice/src", login=False, timeout=60, stdin=None)
        rid = leg.execs()[0]["id"]
        old = self.registry()
        session = old.adopt_local(proc, command="make", cwd="/home/alice/src", task_id="t1")
        self.hub.on_message(leg.conn, {"type": "output", "id": rid, "data": "one\n", "offset": 0})
        self.assertEqual(old.hand_off_for_restart(["device"]), 1)
        self.assertIsNone(proc.wait(0.3))
        entry = next(e for e in json.loads(self.pr.CHECKPOINT_PATH.read_text()) if e["session_id"] == session.id)
        self.assertEqual({k: entry["reattach"][k] for k in ("kind", "device_id", "rid", "offset")},
                         {"kind": "device", "device_id": "d_pc", "rid": rid, "offset": 4})
        self.assertEqual(entry["reattach"]["output_tail"], "one\n")

        reset_handoff()
        self.hub = core.Hub(self.state, parent_lookup=Store().lookup)
        new = self.registry()
        self.assertTrue(new.reattach_from_checkpoint(entry))
        second = Leg(self.hub)
        self.addCleanup(second.stop)
        self.assertEqual(self.hub.attach(second.conn), [{"id": rid, "offset": 4}])
        self.hub.on_message(second.conn, {"type": "output", "id": rid, "data": "two\n", "offset": 4})
        self.hub.on_message(second.conn, {"type": "exit", "id": rid, "code": 3})
        deadline = time.monotonic() + 5
        while not new.get(session.id).exited and time.monotonic() < deadline:
            time.sleep(0.02)
        done = new.get(session.id)
        self.assertEqual((done.exit_code, done.output_buffer), (3, "one\ntwo\n"))


if __name__ == "__main__":
    unittest.main()

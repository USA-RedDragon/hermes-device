"""Exec leg resume tests (protocol version 3, docs/protocol.md section 4.7): the hub's parking of
pending commands, and the exec route's negotiation, welcome and resume. Run inside the Hermes
runtime with scripts/test-plugin.sh."""
from __future__ import annotations

import asyncio
import json
import os
import sys
import tempfile
import threading
import time
import unittest
from pathlib import Path
from unittest import mock

from fastapi import FastAPI
from starlette.testclient import TestClient
from starlette.websockets import WebSocketDisconnect

sys.path.insert(0, str(Path(__file__).resolve().parent))
from test_core import Store, core  # noqa: E402
from test_routes import PATH, Signer, plugin_api  # noqa: E402


class Recorder:
    """A device connection that records what the plugin sends and answers nothing by itself."""

    def __init__(self, version: int, device_id: str = "d_pc"):
        self.loop = asyncio.new_event_loop()
        self.thread = threading.Thread(target=self.loop.run_forever, daemon=True)
        self.thread.start()
        self.sent: list[dict] = []
        self.closed: list[tuple[int, str]] = []

        async def send_text(text):
            self.sent.append(json.loads(text))

        async def close(code, reason):
            self.closed.append((code, reason))

        self.conn = core.DeviceConn(device_id=device_id, name="pc", send_text=send_text, loop=self.loop,
                                    roots=["/home/alice/src"], tmp_dir="/home/alice/tmp", home="/home/alice",
                                    version=version, close=close)

    def execs(self) -> list[dict]:
        return [m for m in self.sent if m["type"] == "exec"]

    def stop(self):
        self.loop.call_soon_threadsafe(self.loop.stop)
        self.thread.join(5)
        self.loop.close()


def output(proc) -> str:
    with proc.stdout:
        return proc.stdout.read()


class HubResumeTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(dir=os.environ.get("TMPDIR"))
        self.addCleanup(self.tmp.cleanup)
        self.hub = core.Hub(Path(self.tmp.name) / "bindings.json", parent_lookup=Store().lookup)
        self.cfg = {"backend_id": "alice", "grace_seconds": 30}
        for p in (mock.patch.object(core, "settings", lambda: self.cfg),
                  mock.patch.object(core, "hub", lambda: self.hub)):
            p.start()
            self.addCleanup(p.stop)

    def device(self, version=3, device_id="d_pc") -> Recorder:
        rec = Recorder(version, device_id)
        self.addCleanup(rec.stop)
        return rec

    def start(self, cmd="sleep 30"):
        return self.hub.start("d_pc", cmd=cmd, cwd="/home/alice/src", login=False, timeout=60, stdin=None)

    def test_drop_parks_and_reconnect_resumes(self):
        first = self.device()
        self.hub.attach(first.conn)
        proc = self.start()
        rid = first.execs()[0]["id"]
        self.hub.on_message(first.conn, {"type": "output", "id": rid, "data": "héllo\n", "offset": 0})
        self.hub.detach(first.conn, resumable=True)
        self.assertIsNone(proc.wait(0.3), "a parked command must not finish")
        self.assertIsNone(self.hub.device("d_pc"))
        second = self.device()
        resume = self.hub.attach(second.conn)
        self.assertEqual(resume, [{"id": rid, "offset": len("héllo\n".encode())}])
        self.hub.on_message(second.conn, {"type": "output", "id": rid, "data": "llo\nworld\n", "offset": 3})
        self.hub.on_message(second.conn, {"type": "output", "id": rid, "data": "tail\n", "offset": 100})
        self.hub.on_message(second.conn, {"type": "exit", "id": rid, "code": 3})
        self.assertEqual(proc.wait(5), 3)
        self.assertEqual(output(proc), "héllo\nworld\n\n[hermes-device: 87 bytes of output were lost while the "
                                       "device was disconnected]\ntail\n")

    def test_grace_expiry_fails_with_255(self):
        self.cfg["grace_seconds"] = 0.2
        first = self.device()
        self.hub.attach(first.conn)
        proc = self.start()
        began = time.monotonic()
        self.hub.detach(first.conn, resumable=True)
        self.assertEqual(proc.wait(5), core.EXIT_LOST)
        self.assertGreaterEqual(time.monotonic() - began, 0.2)
        self.assertIn("did not reconnect within 0.2s", output(proc))
        self.assertEqual(self.hub.attach(self.device().conn), [])

    def test_not_resumable_fails_at_once(self):
        for version, resumable in ((3, False), (2, True)):
            with self.subTest(version=version, resumable=resumable):
                rec = self.device(version=version)
                self.hub.attach(rec.conn)
                proc = self.start()
                self.hub.detach(rec.conn, resumable=resumable)
                self.assertEqual(proc.wait(1), core.EXIT_LOST)
                self.assertIn("disconnected", output(proc))

    def test_grace_zero_turns_resume_off(self):
        self.cfg["grace_seconds"] = 0
        rec = self.device()
        self.hub.attach(rec.conn)
        proc = self.start()
        self.hub.detach(rec.conn, resumable=True)
        self.assertEqual(proc.wait(1), core.EXIT_LOST)
        output(proc)

    def test_version_2_reconnect_fails_parked(self):
        first = self.device()
        self.hub.attach(first.conn)
        proc = self.start()
        self.hub.detach(first.conn, resumable=True)
        self.assertEqual(self.hub.attach(self.device(version=2).conn), [])
        self.assertEqual(proc.wait(1), core.EXIT_LOST)
        self.assertIn("protocol version 2", output(proc))

    def test_kill_while_parked(self):
        first = self.device()
        self.hub.attach(first.conn)
        proc = self.start()
        self.hub.detach(first.conn, resumable=True)
        proc.kill()
        self.assertEqual(proc.wait(1), core.EXIT_KILLED)
        output(proc)
        self.assertEqual(self.hub.attach(self.device().conn), [])

    def test_kill_after_resume_goes_to_new_connection(self):
        first = self.device()
        self.hub.attach(first.conn)
        proc = self.start()
        rid = first.execs()[0]["id"]
        self.hub.detach(first.conn, resumable=True)
        second = self.device()
        self.hub.attach(second.conn)
        proc.kill()
        self.assertEqual(second.sent, [{"type": "kill", "id": rid}])
        self.assertEqual([m["type"] for m in first.sent], ["exec"])
        self.hub.on_message(second.conn, {"type": "exit", "id": rid, "code": 130, "killed": True})
        self.assertEqual(proc.wait(1), 130)
        output(proc)

    def test_replaced_connection_hands_over_pending(self):
        first = self.device()
        self.hub.attach(first.conn)
        proc = self.start()
        rid = first.execs()[0]["id"]
        second = self.device()
        self.assertEqual(self.hub.attach(second.conn), [{"id": rid, "offset": 0}])
        self.hub.detach(first.conn, resumable=True)
        deadline = time.monotonic() + 5
        while not first.closed and time.monotonic() < deadline:
            time.sleep(0.02)
        self.assertEqual(first.closed[0][0], core.CLOSE_REPLACED)
        self.assertIsNone(proc.poll())
        self.hub.on_message(second.conn, {"type": "exit", "id": rid, "code": 0})
        self.assertEqual(proc.wait(1), 0)
        output(proc)

    def test_kill_into_dead_socket_is_not_resumed(self):
        first = self.device()
        self.hub.attach(first.conn)
        proc = self.start()
        rid = first.execs()[0]["id"]
        proc.kill()
        self.assertEqual(first.sent[-1], {"type": "kill", "id": rid})
        second = self.device()
        self.assertEqual(self.hub.attach(second.conn), [])
        self.assertEqual(proc.wait(1), core.EXIT_KILLED)
        output(proc)

    def test_stale_grace_timer_does_not_expire_a_newer_park(self):
        self.cfg["grace_seconds"] = 30
        first = self.device()
        self.hub.attach(first.conn)
        proc = self.start()
        self.hub.detach(first.conn, resumable=True)
        stale = self.hub._park_timers["d_pc"][1]
        second = self.device()
        self.hub.attach(second.conn)
        self.hub.detach(second.conn, resumable=True)
        self.hub._expire("d_pc", stale)
        self.assertIsNone(proc.poll())
        self.assertEqual(len(self.hub.attach(self.device().conn)), 1)

class RouteResumeTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(dir=os.environ.get("TMPDIR"))
        self.addCleanup(self.tmp.cleanup)
        base = Path(os.path.realpath(self.tmp.name))
        self.root = base / "src"
        self.root.mkdir()
        self.hub = core.Hub(base / "bindings.json", parent_lookup=Store().lookup)
        self.signer = Signer()
        self.cfg = {"backend_id": "alice", "placeholder": str(base / "ph"), "hub_public_key_env": "TEST_HUB_KEY",
                    "grace_seconds": 30}
        patches = [mock.patch.object(core, "hub", lambda: self.hub),
                   mock.patch.object(core, "settings", lambda: self.cfg),
                   mock.patch.object(core, "hermes_version", lambda: "v0.22.7"),
                   mock.patch.dict(os.environ, {"TEST_HUB_KEY": self.signer.public_b64})]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)
        app = FastAPI()
        app.include_router(plugin_api.router, prefix="/api/plugins/device")
        self.client = TestClient(app)
        self.addCleanup(self.client.close)

    def connect(self, **hello):
        msg = {"type": "hello", "version": 2, "max_version": 3, "roots": [str(self.root)], "tmp_dir": str(self.root),
               "home": str(self.root), "hostname": "desktop", "os": "linux", "arch": "amd64", "shell": "bash",
               "client_version": "v0.3.0"}
        msg.update(hello)
        msg = {k: v for k, v in msg.items() if v is not None}
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

    def wait_detached(self):
        deadline = time.monotonic() + 5
        while self.hub.device("d_8f2c") is not None and time.monotonic() < deadline:
            time.sleep(0.02)
        self.assertIsNone(self.hub.device("d_8f2c"))

    def test_negotiates_version_3_and_sends_resume(self):
        ws = self.connect()
        welcome = ws.receive_json()
        self.assertEqual((welcome["version"], welcome["grace_seconds"], welcome["plugin_version"]),
                         (3, 30.0, core.PLUGIN_VERSION))
        self.assertEqual(ws.receive_json(), {"type": "resume", "commands": []})

    def test_version_2_device_gets_no_resume(self):
        ws = self.connect(max_version=None)
        welcome = ws.receive_json()
        self.assertEqual(welcome["version"], 2)
        self.assertNotIn("grace_seconds", welcome)
        ws.send_json({"type": "ping", "seq": 1})
        self.assertEqual(ws.receive_json(), {"type": "pong", "seq": 1})

    def test_mismatch_closes_4426_with_reason(self):
        for hello, want in (({"version": 4, "max_version": 5}, "speaks protocol 4 to 5"),
                            ({"version": 1, "max_version": None}, "speaks protocol 1;"),
                            ({"version": 3, "max_version": 2}, "speaks protocol 3;"),
                            ({"version": "2"}, "speaks protocol '2'")):
            with self.subTest(hello=hello):
                gone = self.close_of(self.connect(**hello))
                self.assertEqual(gone.code, core.CLOSE_UNSUPPORTED_VERSION)
                self.assertIn(want, gone.reason)
                self.assertIn(f"plugin {core.PLUGIN_VERSION} speaks 2 to 3", gone.reason)
                self.assertLessEqual(len(gone.reason.encode()), 120)

    def test_command_survives_reconnect(self):
        ws1 = self.connect()
        ws1.receive_json()
        ws1.receive_json()
        proc = self.hub.start("d_8f2c", cmd="make", cwd=str(self.root), login=False, timeout=60, stdin=None)
        exec_msg = ws1.receive_json()
        rid = exec_msg["id"]
        ws1.send_json({"type": "output", "id": rid, "data": "one\n", "offset": 0})
        ws1.send_json({"type": "ping", "seq": 1})
        self.assertEqual(ws1.receive_json(), {"type": "pong", "seq": 1})
        ws1.close(code=1001)
        self.wait_detached()
        self.assertIsNone(proc.poll())
        ws2 = self.connect()
        self.assertEqual(ws2.receive_json()["type"], "welcome")
        self.assertEqual(ws2.receive_json(), {"type": "resume", "commands": [{"id": rid, "offset": 4}]})
        ws2.send_json({"type": "output", "id": rid, "data": "two\n", "offset": 4})
        ws2.send_json({"type": "exit", "id": rid, "code": 0})
        self.assertEqual(proc.wait(5), 0)
        self.assertEqual(output(proc), "one\ntwo\n")

    def test_service_restart_fails_this_device_at_once(self):
        ws = self.connect()
        ws.receive_json()
        ws.receive_json()
        proc = self.hub.start("d_8f2c", cmd="make", cwd=str(self.root), login=False, timeout=60, stdin=None)
        ws.receive_json()
        ws.close(code=core.CLOSE_SERVICE_RESTART)
        self.assertEqual(proc.wait(5), core.EXIT_LOST)
        self.assertIn("disconnected", output(proc))
        ws2 = self.connect()
        ws2.receive_json()
        self.assertEqual(ws2.receive_json(), {"type": "resume", "commands": []})


if __name__ == "__main__":
    unittest.main()

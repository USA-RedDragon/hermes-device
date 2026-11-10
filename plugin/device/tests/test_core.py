"""Plugin tests with a fake in-process device. Run inside the Hermes runtime:

    hpy -m unittest discover -s plugin/device/tests -t plugin/device
"""
from __future__ import annotations

import asyncio
import importlib.util
import json
import os
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from pathlib import Path
from unittest import mock

_HERE = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location("hermes_device_core", _HERE / "core.py")
core = importlib.util.module_from_spec(spec)
sys.modules["hermes_device_core"] = core
spec.loader.exec_module(core)


class FakeDevice:
    """Runs exec requests with real bash, like the Go client, and enforces one root."""

    def __init__(self, hub, root: str, tmp: str):
        self.hub, self.root = hub, os.path.realpath(root)
        self.loop = asyncio.new_event_loop()
        threading.Thread(target=self.loop.run_forever, daemon=True).start()
        self.procs: dict[str, subprocess.Popen] = {}
        self.conn = core.DeviceConn(name="pc", send_text=self._recv, loop=self.loop, roots=[self.root], tmp_dir=tmp)
        hub.attach(self.conn)

    async def _recv(self, text: str) -> None:
        msg = json.loads(text)
        if msg["type"] == "exec":
            threading.Thread(target=self._run, args=(msg,), daemon=True).start()
        elif msg["type"] == "kill" and msg["id"] in self.procs:
            os.killpg(self.procs[msg["id"]].pid, 9)

    def _reply(self, msg: dict) -> None:
        self.hub.on_message(self.conn, msg)

    def _run(self, msg: dict) -> None:
        cwd = os.path.realpath(msg["cwd"])
        if not (cwd == self.root or cwd.startswith(self.root + "/")):
            self._reply({"type": "exit", "id": msg["id"], "code": 126, "error": f"device pc refused: {cwd} outside the allowed roots"})
            return
        args = ["bash"] + (["-l"] if msg["login"] else []) + ["-c", msg["cmd"]]
        p = subprocess.Popen(args, cwd=cwd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                             stdin=subprocess.PIPE if msg.get("stdin") is not None else subprocess.DEVNULL, text=True,
                             start_new_session=True)
        self.procs[msg["id"]] = p
        if msg.get("stdin") is not None:
            p.stdin.write(msg["stdin"])
            p.stdin.close()
        for line in p.stdout:
            self._reply({"type": "output", "id": msg["id"], "data": line})
        code = p.wait()
        self._reply({"type": "exit", "id": msg["id"], "code": 130 if code < 0 else code, "killed": code < 0})

    def close(self) -> None:
        self.hub.detach(self.conn)
        self.loop.call_soon_threadsafe(self.loop.stop)


class DeviceTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(dir=os.environ.get("TMPDIR"))
        base = Path(self.tmp.name)
        self.root, self.outside, self.placeholder, self.devtmp = (base / n for n in ("root", "outside", "ph", "devtmp"))
        for d in (self.root / "sub", self.outside, self.placeholder, self.devtmp):
            d.mkdir(parents=True)
        (self.root / "f.txt").write_text("one\ntwo\n")
        self.hub = core.Hub(base / "bindings.json")
        self.session = "sess-1"
        patches = [
            mock.patch.object(core, "hub", lambda: self.hub),
            mock.patch.object(core, "configured_devices", lambda: {"pc": "x" * 20}),
            mock.patch.object(core, "placeholder_roots", lambda: [str(self.placeholder)]),
            mock.patch.object(core, "current_session_id", lambda: self.session),
        ]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)
        self.device = FakeDevice(self.hub, str(self.root), str(self.devtmp))
        self.addCleanup(self.device.close)
        self.addCleanup(self.tmp.cleanup)

    def env(self):
        env = core.DeviceEnvironment(cwd=str(self.placeholder), timeout=30, task_id="t")
        env.init_session()
        return env

    def test_unbound_session_is_refused(self):
        r = self.env().execute("pwd")
        self.assertEqual(r["returncode"], 1)
        self.assertIn("/device", r["output"])

    def test_bind_runs_in_device_dir_and_ignores_placeholder(self):
        env = self.env()
        self.hub.bind(self.session, "pc", str(self.root))
        r = env.execute("pwd; exit 3", cwd=str(self.placeholder))
        self.assertEqual(r["returncode"], 3)
        self.assertEqual(r["output"].strip(), os.path.realpath(self.root))
        env.cwd = str(self.placeholder)  # a per-turn workspace override must not move the device cwd
        r = env.execute("cd sub && pwd")
        self.assertEqual(env.cwd, os.path.realpath(self.root / "sub"))
        self.assertEqual(self.hub.device_cwd(self.session), os.path.realpath(self.root / "sub"))
        r = env.execute("cat ../f.txt", stdin_data=None)
        self.assertEqual(r["output"], "one\ntwo\n")

    def test_stdin_payload(self):
        env = self.env()
        self.hub.bind(self.session, "pc", str(self.root))
        r = env.execute("cat", stdin_data="piped\n")
        self.assertEqual(r["output"], "piped\n")

    def test_bind_outside_root_refused(self):
        with self.assertRaises(core.DeviceError):
            self.hub.bind(self.session, "pc", str(self.outside))
        self.assertIsNone(self.hub.binding(self.session))

    def test_refused_cwd_resets_to_binding(self):
        env = self.env()
        self.hub.bind(self.session, "pc", str(self.root))
        r = env.execute("ls", cwd=str(self.outside))
        self.assertEqual(r["returncode"], 126)
        self.assertIn("refused", r["output"])
        self.assertEqual(env.cwd, os.path.realpath(self.root))

    def test_timeout_kills_device_command(self):
        env = self.env()
        self.hub.bind(self.session, "pc", str(self.root))
        start = time.monotonic()
        r = env.execute("sleep 30", timeout=1)
        self.assertLess(time.monotonic() - start, 2.5)
        self.assertEqual(r["returncode"], 124)
        time.sleep(0.5)
        self.assertTrue(all(p.poll() is not None for p in self.device.procs.values()))

    def test_disconnect_fails_pending(self):
        self.hub.bind(self.session, "pc", str(self.root))
        proc = self.hub.start("pc", cmd="sleep 30", cwd=str(self.root), login=False, timeout=60, stdin=None)
        time.sleep(0.3)
        self.hub.detach(self.device.conn)
        self.assertEqual(proc.wait(5), core.EXIT_LOST)
        self.assertIn("disconnected", proc.stdout.read())
        proc.stdout.close()
        for p in self.device.procs.values():
            if p.poll() is None:
                os.killpg(p.pid, 9)

    def test_new_connection_closes_old(self):
        closed = threading.Event()

        async def close():
            closed.set()

        self.device.conn.close = close
        newer = core.DeviceConn(name="pc", send_text=self.device._recv, loop=self.device.loop, roots=[self.device.root], tmp_dir="")
        self.hub.attach(newer)
        self.assertTrue(closed.wait(5))
        self.assertIs(self.hub.device("pc"), newer)

    def test_bindings_persist(self):
        self.hub.bind(self.session, "pc", str(self.root))
        again = core.Hub(Path(self.tmp.name) / "bindings.json")
        self.assertEqual(again.binding(self.session).dir, os.path.realpath(self.root))
        self.assertTrue(self.hub.unbind(self.session))
        self.assertIsNone(core.Hub(Path(self.tmp.name) / "bindings.json").binding(self.session))


class AuthTests(unittest.TestCase):
    def test_authenticate(self):
        with mock.patch.object(core, "configured_devices", lambda: {"pc": "s" * 20}):
            self.assertTrue(core.authenticate("pc", "s" * 20))
            self.assertFalse(core.authenticate("pc", "wrong"))
            self.assertFalse(core.authenticate("other", "s" * 20))


if __name__ == "__main__":
    unittest.main()

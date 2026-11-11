"""Plugin core tests with a fake in-process device and a fake session store. Run inside the
Hermes runtime with scripts/test-plugin.sh."""
from __future__ import annotations

import asyncio
import contextlib
import importlib.util
import json
import os
import posixpath
import stat
import subprocess
import sys
import tempfile
import threading
import time
import types
import unittest
from pathlib import Path
from unittest import mock

_HERE = Path(__file__).resolve().parent.parent


def load_core():
    module = sys.modules.get("hermes_device_core")
    if module is None:
        spec = importlib.util.spec_from_file_location("hermes_device_core", _HERE / "core.py")
        module = importlib.util.module_from_spec(spec)
        sys.modules["hermes_device_core"] = module
        spec.loader.exec_module(module)
    return module


core = load_core()


class FakeDevice:
    """Runs exec requests with real bash, like the Go client, and enforces its roots."""

    def __init__(self, hub, root: str, tmp: str, home: str, device_id: str = "d_pc", name: str = "pc"):
        self.hub, self.root = hub, os.path.realpath(root)
        self.loop = asyncio.new_event_loop()
        self.thread = threading.Thread(target=self.loop.run_forever, daemon=True)
        self.thread.start()
        self.procs: dict[str, subprocess.Popen] = {}
        self.execs: list[dict] = []
        self.detached: list[str] = []
        self.kills: list[str] = []
        self.closed: list[tuple[int, str]] = []
        self.conn = self.make_conn(device_id, name, tmp, home)
        hub.attach(self.conn)

    def make_conn(self, device_id, name, tmp, home):
        async def close(code, reason):
            self.closed.append((code, reason))
        return core.DeviceConn(device_id=device_id, name=name, send_text=self._recv, loop=self.loop,
                               roots=[self.root], tmp_dir=tmp, home=home, hostname="desktop", os="linux",
                               arch="amd64", close=close)

    async def _recv(self, text: str) -> None:
        msg = json.loads(text)
        if msg["type"] == "detach":
            self.detached.append(msg["id"])
        elif msg["type"] == "exec":
            self.execs.append(msg)
            threading.Thread(target=self._run, args=(msg,), daemon=True).start()
        elif msg["type"] == "kill" and msg["id"] in self.procs:
            self.kills.append(msg["id"])
            try:
                os.killpg(self.procs[msg["id"]].pid, 9)
            except ProcessLookupError:
                pass

    def _reply(self, msg: dict) -> None:
        self.hub.on_message(self.conn, msg)

    def _run(self, msg: dict) -> None:
        cwd = os.path.realpath(msg["cwd"])
        if not (cwd == self.root or cwd.startswith(self.root + "/")) or not os.path.isdir(cwd):
            self._reply({"type": "exit", "id": msg["id"], "code": 126,
                         "error": f"device pc refused: working directory {cwd!r}: outside the allowed roots"})
            return
        args = ["bash"] + (["-l"] if msg["login"] else []) + ["-c", msg["cmd"]]
        p = subprocess.Popen(args, cwd=cwd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                             stdin=subprocess.PIPE if msg.get("stdin") is not None else subprocess.DEVNULL,
                             start_new_session=True)
        self.procs[msg["id"]] = p
        if msg.get("stdin") is not None:
            p.stdin.write(msg["stdin"].encode())
            p.stdin.close()
        while True:
            chunk = p.stdout.read1(65536)
            if not chunk:
                break
            self._reply({"type": "output", "id": msg["id"], "data": chunk.decode("utf-8", "replace")})
        p.stdout.close()
        code = p.wait()
        self._reply({"type": "exit", "id": msg["id"], "code": 128 - code if code < 0 else code,
                     **({"killed": True} if code < 0 else {})})

    def close(self) -> None:
        self.hub.detach(self.conn)
        for p in list(self.procs.values()):
            if p.poll() is None:
                try:
                    os.killpg(p.pid, 9)
                except ProcessLookupError:
                    pass
        self.loop.call_soon_threadsafe(self.loop.stop)
        self.thread.join(5)
        self.loop.close()


class FakeLocal:
    uses_host_filesystem = True

    def __init__(self, cwd, timeout):
        self.cwd, self.timeout, self.commands = cwd, timeout, []

    def execute(self, command, cwd="", **kwargs):
        self.commands.append((command, cwd))
        return {"output": "ran on pod", "returncode": 0}

    def get_temp_dir(self):
        return tempfile.gettempdir()

    def cleanup(self):
        pass


class Store:
    """Fake session store: rows maps session id -> parent id (None for a root)."""

    def __init__(self):
        self.rows: dict[str, str | None] = {}
        self.calls: list[str] = []
        self.broken = False

    def lookup(self, sid):
        self.calls.append(sid)
        if self.broken:
            raise RuntimeError("database is locked")
        if sid not in self.rows:
            return False, None
        return True, self.rows[sid]


class Base(unittest.TestCase):
    settings: dict = {}

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(dir=os.environ.get("TMPDIR"))
        self.addCleanup(self.tmp.cleanup)
        base = Path(os.path.realpath(self.tmp.name))
        self.base = base
        self.home = base / "home"
        self.root = self.home / "src"
        self.outside, self.placeholder, self.devtmp = base / "outside", base / "ph", base / "devtmp"
        for d in (self.root / "sub", self.outside, self.placeholder, self.devtmp, self.home / ".ssh"):
            d.mkdir(parents=True, exist_ok=True)
        (self.root / "f.txt").write_text("one\ntwo\nthree\n")
        self.store = Store()
        self.store.rows["sess-1"] = None
        self.hub = core.Hub(base / "state" / "bindings.json", parent_lookup=self.store.lookup)
        self.session = "sess-1"
        self.cfg = {"backend_id": "alice", "placeholder": str(self.placeholder), **self.settings}
        patches = [
            mock.patch.object(core, "hub", lambda: self.hub),
            mock.patch.object(core, "settings", lambda: self.cfg),
            mock.patch.object(core, "terminal_cwd", lambda: ""),
            mock.patch.object(core, "_scope_terminal_cwd", lambda: ""),
            mock.patch.object(core, "current_session_id", lambda: self.session),
            mock.patch.object(core, "session_cwd", lambda: ""),
        ]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)
        self.device = FakeDevice(self.hub, str(self.root), str(self.devtmp), str(self.home))
        self.addCleanup(self.device.close)

    def bind(self, sid=None, directory=None):
        return self.hub.bind(sid or self.session, "d_pc", str(directory or self.root))

    def env(self, local=None):
        return core.DeviceRouterEnvironment(cwd=str(self.placeholder), timeout=30, task_id="t",
                                            local_factory=local or FakeLocal)


class EnvironmentTests(Base):
    def test_bound_runs_in_device_dir_and_ignores_placeholder(self):
        env = self.env()
        self.bind()
        r = env.execute("pwd; exit 3", cwd=str(self.placeholder))
        self.assertEqual(r["returncode"], 3)
        self.assertEqual(r["output"].strip(), str(self.root))
        self.assertTrue(all(m["cwd"] != str(self.placeholder) for m in self.device.execs))
        env.cwd = str(self.placeholder)
        env.execute("cd sub && pwd")
        self.assertEqual(env.cwd, str(self.root / "sub"))
        r = env.execute("cat ../f.txt")
        self.assertEqual(r["output"], "one\ntwo\nthree\n")
        self.assertFalse(env.uses_host_filesystem)
        self.assertFalse(env.is_local)
        self.assertEqual(env.get_temp_dir(), str(self.devtmp))

    def test_binding_starts_in_binding_dir_not_terminal_cwd(self):
        with mock.patch.object(core, "_scope_terminal_cwd", lambda: str(self.outside)):
            env = self.env()
            self.bind(directory=self.root / "sub")
            r = env.execute("pwd", cwd=str(self.outside))
        self.assertEqual(r["output"].strip(), str(self.root / "sub"))

    def test_stdin_payload(self):
        env = self.env()
        self.bind()
        self.assertEqual(env.execute("cat", stdin_data="piped\n")["output"], "piped\n")

    def test_refused_cwd_resets_to_binding(self):
        env = self.env()
        self.bind()
        r = env.execute("ls", cwd=str(self.outside))
        self.assertEqual(r["returncode"], 126)
        self.assertIn("refused", r["output"])
        self.assertEqual(env.cwd, str(self.root))

    def test_timeout_kills_device_command(self):
        env = self.env()
        self.bind()
        start = time.monotonic()
        r = env.execute("sleep 30", timeout=1)
        self.assertLess(time.monotonic() - start, 5)
        self.assertEqual(r["returncode"], 124)
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline and any(p.poll() is None for p in self.device.procs.values()):
            time.sleep(0.05)
        self.assertTrue(all(p.poll() is not None for p in self.device.procs.values()))

    def test_bound_offline_fails_with_255_and_never_falls_back(self):
        env = self.env()
        self.bind()
        self.hub.detach(self.device.conn)
        r = env.execute("pwd")
        self.assertEqual(r["returncode"], 255)
        self.assertIn("not connected", r["output"])
        self.assertIsNone(env._local)

    def test_never_bound_runs_on_pod(self):
        env = self.env()
        r = env.execute("pwd", cwd="/srv/work")
        self.assertEqual(r["output"], "ran on pod")
        self.assertEqual(env._local.commands, [("pwd", "/srv/work")])
        self.assertTrue(env.uses_host_filesystem)
        self.assertEqual(self.device.execs, [])

    def test_never_bound_refused_with_fallback_refuse(self):
        self.cfg["fallback"] = "refuse"
        env = self.env()
        r = env.execute("pwd")
        self.assertEqual(r["returncode"], 1)
        self.assertIn("refuses unbound sessions", r["output"])
        self.assertIsNone(env._local)
        self.assertFalse(env.uses_host_filesystem)

    def test_unbound_after_binding_fails_closed(self):
        env = self.env()
        self.bind()
        self.hub.unbind(self.session)
        r = env.execute("pwd")
        self.assertEqual(r["returncode"], 1)
        self.assertIn("sess-1", r["output"])
        self.assertIn("/device", r["output"])
        self.assertIsNone(env._local)

    def test_no_session_id_fails_closed(self):
        self.session = ""
        r = self.env().execute("pwd")
        self.assertEqual(r["returncode"], 1)
        self.assertIn("no session id", r["output"])

    def test_placeholder_cwd_without_binding_fails_closed(self):
        with mock.patch.object(core, "session_cwd", lambda: str(self.placeholder)):
            env = self.env()
            r = env.execute("pwd")
        self.assertEqual(r["returncode"], 1)
        self.assertIn("placeholder", r["output"])

    def test_disconnect_fails_pending(self):
        self.bind()
        proc = self.hub.start("d_pc", cmd="sleep 30", cwd=str(self.root), login=False, timeout=60, stdin=None)
        time.sleep(0.3)
        self.hub.detach(self.device.conn)
        self.assertEqual(proc.wait(5), core.EXIT_LOST)
        self.assertIn("disconnected", proc.stdout.read())
        proc.stdout.close()

    def test_new_connection_replaces_old_with_4409(self):
        proc = self.hub.start("d_pc", cmd="sleep 30", cwd=str(self.root), login=False, timeout=60, stdin=None)
        time.sleep(0.2)
        newer = self.device.make_conn("d_pc", "pc", str(self.devtmp), str(self.home))
        self.hub.attach(newer)
        self.assertEqual(proc.wait(5), core.EXIT_LOST)
        proc.stdout.close()
        deadline = time.monotonic() + 5
        while not self.device.closed and time.monotonic() < deadline:
            time.sleep(0.02)
        self.assertEqual(self.device.closed[0][0], core.CLOSE_REPLACED)
        self.assertIs(self.hub.device("d_pc"), newer)
        self.device.conn = newer

    def test_class_name_has_no_backend_hint(self):
        name = core.DeviceRouterEnvironment.__name__.lower()
        for hint in ("local", "ssh", "docker", "modal", "daytona"):
            self.assertNotIn(hint, name)


class BindingTests(Base):
    def test_bind_rules(self):
        self.store.rows.update({"sess-2": None, "sess-3": None, "sess-4": None})
        b = self.bind()
        self.assertEqual(b.dir, str(self.root))
        again = self.bind()
        self.assertEqual(again.generation, b.generation)
        moved = self.bind(directory=self.root / "sub")
        self.assertNotEqual(moved.generation, b.generation)
        with self.assertRaises(core.BindError) as ctx:
            self.hub.bind(self.session, "d_pc", "relative/dir")
        self.assertEqual((ctx.exception.status, ctx.exception.code), (400, "bad_dir"))
        with self.assertRaises(core.BindError) as ctx:
            self.hub.bind("sess-2", "d_pc", str(self.outside))
        self.assertEqual((ctx.exception.status, ctx.exception.code), (422, "dir_refused"))
        self.assertIn("outside", ctx.exception.message)
        with self.assertRaises(core.BindError) as ctx:
            self.hub.bind("sess-3", "d_nope", str(self.root))
        self.assertEqual((ctx.exception.status, ctx.exception.code), (404, "unknown_device"))
        other = FakeDevice(self.hub, str(self.root), str(self.devtmp), str(self.home), device_id="d_lap", name="lap")
        self.addCleanup(other.close)
        with self.assertRaises(core.BindError) as ctx:
            self.hub.bind(self.session, "d_lap", str(self.root))
        self.assertEqual((ctx.exception.status, ctx.exception.code), (409, "bound_to_other_device"))
        self.hub.detach(other.conn)
        with self.assertRaises(core.BindError) as ctx:
            self.hub.bind("sess-4", "d_lap", str(self.root))
        self.assertEqual((ctx.exception.status, ctx.exception.code), (409, "device_offline"))

    def assert_refused(self, sid, device_id, code, operator=False):
        with self.assertRaises(core.BindError) as ctx:
            self.hub.bind(sid, device_id, str(self.root), operator=operator)
        self.assertEqual((ctx.exception.status, ctx.exception.code), (409, code))
        return ctx.exception.message

    def test_device_cannot_take_another_devices_lineage(self):
        other = FakeDevice(self.hub, str(self.root), str(self.devtmp), str(self.home), device_id="d_lap", name="lap")
        self.addCleanup(other.close)
        self.store.rows.update({"comp": self.session, "branch": "comp", "web": None, "web-child": "web", "gone": None})
        self.bind()
        for sid in (self.session, "comp", "branch"):
            message = self.assert_refused(sid, "d_lap", "bound_to_other_device")
            self.assertIn("d_pc", message)
        self.assertIn("through session sess-1", self.assert_refused("branch", "d_lap", "bound_to_other_device"))
        self.assertIsNone(self.hub.binding("branch"))
        self.assertEqual(self.hub.bind("comp", "d_pc", str(self.root)).device_id, "d_pc")
        self.assertEqual(self.hub.bind("web-child", "d_lap", str(self.root)).device_id, "d_lap")
        self.assert_refused("web-child", "d_pc", "bound_to_other_device")
        self.hub.bind("gone", "d_pc", str(self.root))
        self.hub.unbind("gone")
        for device_id in ("d_lap", "d_pc"):
            self.assertIn("was bound to a device", self.assert_refused("gone", device_id, "not_bindable"))
        self.assertIn("no row", self.assert_refused("ghost", "d_lap", "not_bindable"))
        self.store.broken = True
        self.assert_refused("ghost-2", "d_lap", "not_bindable")

    def test_operator_moves_a_session(self):
        other = FakeDevice(self.hub, str(self.root), str(self.devtmp), str(self.home), device_id="d_lap", name="lap")
        self.addCleanup(other.close)
        self.store.rows.update({"comp": self.session, "gone": None})
        self.bind()
        moved = self.hub.bind(self.session, "d_lap", str(self.root), operator=True)
        self.assertEqual((moved.device_id, self.hub.resolve("comp").binding.device_id), ("d_lap", "d_lap"))
        self.assertIn("/device off", self.assert_refused("comp", "d_pc", "bound_to_other_device", operator=True))
        self.hub.bind("gone", "d_pc", str(self.root))
        self.hub.unbind("gone")
        self.assertEqual(self.hub.bind("gone", "d_lap", str(self.root), operator=True).device_id, "d_lap")

    def test_bind_resolves_symlinks(self):
        link = self.root / "link"
        link.symlink_to(self.root / "sub")
        self.assertEqual(self.hub.bind(self.session, "d_pc", str(link)).dir, str(self.root / "sub"))

    def test_not_device_session(self):
        hub = core.Hub(None, parent_lookup=self.store.lookup, session_check=lambda sid: "bypass worker")
        hub.attach(self.device.conn)
        with self.assertRaises(core.BindError) as ctx:
            hub.bind(self.session, "d_pc", str(self.root))
        self.assertEqual((ctx.exception.status, ctx.exception.code), (409, "not_device_session"))

    def test_file_format_and_ever_bound(self):
        b = self.bind()
        path = self.base / "state" / "bindings.json"
        doc = json.loads(path.read_text())
        self.assertEqual(doc, {"version": 2, "bindings": {self.session: {
            "device_id": "d_pc", "dir": str(self.root), "generation": b.generation, "bound_at": b.bound_at}},
            "ever_bound": [self.session]})
        self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o644)
        self.assertTrue(self.hub.unbind(self.session))
        doc = json.loads(path.read_text())
        self.assertEqual((doc["bindings"], doc["ever_bound"]), ({}, [self.session]))
        again = core.Hub(path, parent_lookup=self.store.lookup)
        self.assertIsNone(again.binding(self.session))
        self.assertEqual(again.resolve(self.session).state, core.STATE_FAIL_CLOSED)
        self.assertFalse(again.unbind(self.session))

    def test_v1_migration(self):
        path = self.base / "v1" / "bindings.json"
        path.parent.mkdir()
        path.write_text(json.dumps({"bindings": {
            "s-a": {"device": "pc", "dir": "/home/alice/src", "generation": "abc"},
            "s-b": {"device": "bad name!", "dir": "/home/alice/x", "generation": "def"}}}))
        hub = core.Hub(path, parent_lookup=lambda sid: (True, None))
        self.assertEqual(hub.binding("s-a").device_id, "pc")
        self.assertEqual(hub.binding("s-a").generation, "abc")
        self.assertIsNone(hub.binding("s-b"))
        self.assertEqual(hub.resolve("s-b").state, core.STATE_FAIL_CLOSED)
        doc = json.loads(path.read_text())
        self.assertEqual(doc["version"], 2)
        self.assertEqual(doc["ever_bound"], ["s-a", "s-b"])

    def test_corrupt_file_fails_closed(self):
        path = self.base / "bad" / "bindings.json"
        path.parent.mkdir()
        path.write_text("{not json")
        hub = core.Hub(path, parent_lookup=lambda sid: (True, None))
        res = hub.resolve("anything")
        self.assertEqual(res.state, core.STATE_FAIL_CLOSED)
        hub.attach(self.device.conn)
        with self.assertRaises(core.BindError) as ctx:
            hub.bind("s", "d_pc", str(self.root))
        self.assertEqual(ctx.exception.status, 503)
        self.assertEqual(path.read_text(), "{not json")


class LineageTests(Base):
    def test_child_inherits_and_nearest_binding_wins(self):
        self.store.rows.update({"c1": "sess-1", "c2": "c1"})
        self.bind()
        res = self.hub.resolve("c2")
        self.assertEqual((res.state, res.via), (core.STATE_BOUND, "sess-1"))
        self.hub.bind("c1", "d_pc", str(self.root / "sub"))
        res = self.hub.resolve("c2")
        self.assertEqual((res.via, res.binding.dir), ("c1", str(self.root / "sub")))

    def test_ever_bound_ancestor_fails_closed(self):
        self.store.rows.update({"c1": "sess-1"})
        self.bind()
        self.hub.unbind(self.session)
        self.assertEqual(self.hub.resolve("c1").state, core.STATE_FAIL_CLOSED)

    def test_never_bound(self):
        self.store.rows.update({"c1": "sess-1"})
        self.assertEqual(self.hub.resolve("c1").state, core.STATE_NEVER_BOUND)

    def test_missing_row_store_error_cycle_and_depth_fail_closed(self):
        self.assertIn("no row", self.hub.resolve("ghost").reason)
        self.store.rows.update({"x": "y", "y": "x"})
        self.assertIn("cycle", self.hub.resolve("x").reason)
        for i in range(150):
            self.store.rows[f"n{i}"] = f"n{i + 1}"
        self.store.rows["n150"] = None
        self.assertIn("longer than", self.hub.resolve("n0").reason)
        self.store.broken = True
        res = self.hub.resolve("sess-1")
        self.assertEqual(res.state, core.STATE_FAIL_CLOSED)
        self.assertIn("database is locked", res.reason)

    def test_in_process_parent_link_covers_a_missing_row(self):
        self.bind()
        self.assertEqual(self.hub.resolve("sub-child").state, core.STATE_FAIL_CLOSED)
        self.hub.note_parent("sub-child", self.session)
        self.assertEqual(self.hub.resolve("sub-child").state, core.STATE_BOUND)

    def test_orphan_side_agent_fails_closed(self):
        self.store.rows["bg_1"] = None
        self.hub.note_orphan("bg_1", "is a background task whose starting session could not be found")
        self.assertEqual(self.hub.resolve("bg_1").state, core.STATE_FAIL_CLOSED)

    def test_parent_links_cached(self):
        self.store.rows.update({"c1": "sess-1"})
        self.hub.resolve("c1")
        self.hub.resolve("c1")
        self.assertEqual(self.store.calls.count("c1"), 1)
        self.assertEqual(self.store.calls.count("sess-1"), 2)


class SessionListAudienceTests(Base):
    def test_keeps_only_sessions_bound_to_the_audience_device(self):
        self.store.rows.update({"c1": "sess-1", "other": None, "free": None, "was": None, "lap": None})
        self.bind()
        self.hub.bind("was", "d_pc", str(self.root))
        self.hub.unbind("was")
        laptop = FakeDevice(self.hub, str(self.root), str(self.devtmp), str(self.home), device_id="d_lap",
                            name="laptop")
        self.addCleanup(laptop.close)
        self.hub.bind("lap", "d_lap", str(self.root))
        ids = ["sess-1", "c1", "free", "was", "lap", "ghost", 7]
        self.assertEqual(core.filter_session_list(audience="hermes-device:d_pc", session_ids=ids),
                         ["sess-1", "c1"])
        self.assertEqual(core.filter_session_list(audience="hermes-device:d_lap", session_ids=ids), ["lap"])
        self.assertEqual(core.filter_session_list(audience="hermes-device:d_pc", session_ids=[]), [])
        self.assertEqual(core.filter_session_list(audience="hermes-device:", session_ids=ids), [])

    def test_other_audiences_are_not_judged(self):
        self.bind()
        for audience in ("alice", "", None, "hermes-devices:d_pc"):
            self.assertIsNone(core.filter_session_list(audience=audience, session_ids=["sess-1"]))


def _wait_for(predicate, timeout=10.0):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return True
        time.sleep(0.05)
    return predicate()


class BackgroundHandoffTests(Base):
    """A device command that Hermes moves to the background (section 8.4)."""

    def setUp(self):
        super().setUp()
        from tools.process_registry import ProcessRegistry, process_registry
        from tools.terminal_tool_background import yield_to_background_handler
        self.registry = process_registry
        self.original = getattr(yield_to_background_handler, core._WRAPPED_MARK, yield_to_background_handler)
        core._wrap(ProcessRegistry, "_signal_kill", core._make_signal_kill)
        self.device.conn.capabilities = frozenset({core.CAPABILITY_DETACH})
        self.bind()
        self.sessions: list[str] = []
        self.addCleanup(self._kill_sessions)

    def _kill_sessions(self):
        for sid in self.sessions:
            with contextlib.suppress(Exception):
                self.registry.kill_process(sid)

    def handler(self, command):
        wrapped = core._make_yield_to_background_handler(self.original)
        return wrapped(command=command, env_type=core.BACKEND_NAME, cwd=str(self.root), effective_task_id="t",
                       task_id="t", session_key="")

    def start(self, command):
        proc = self.hub.start("d_pc", cmd=command, cwd=str(self.root), login=False, timeout=30, stdin=None)
        self.assertTrue(_wait_for(lambda: self.device.execs and self.device.execs[-1]["cmd"] == command))
        return proc

    def handoff(self, command):
        proc = self.start(command)
        handed = self.handler(command)(proc, "")
        self.assertIsNotNone(handed)
        self.sessions.append(handed["yielded_session_id"])
        return proc, handed

    def test_device_command_runs_on_as_a_background_process(self):
        command = "echo started; sleep 1; echo done"
        proc, handed = self.handoff(command)
        self.assertEqual((handed["returncode"], handed["pid"]), (None, None))
        self.assertEqual(self.device.detached, [proc.request_id])
        session = self.registry.get(handed["yielded_session_id"])
        self.assertIsNotNone(session)
        self.assertIs(session.process, proc)
        done = self.registry.wait(handed["yielded_session_id"], timeout=15)
        self.assertEqual(done.get("exit_code"), 0, done)
        self.assertIn("done", done.get("output", ""))
        self.assertEqual(sum(1 for e in self.device.execs if e["cmd"] == command), 1)

    def test_kill_reaches_the_device(self):
        proc, handed = self.handoff("sleep 30")
        began = time.monotonic()
        out = self.registry.kill_process(handed["yielded_session_id"])
        self.assertEqual(out.get("status"), "killed", out)
        self.assertLess(time.monotonic() - began, core.DEVICE_KILL_WAIT_S)
        self.assertEqual(self.device.kills, [proc.request_id])
        self.assertIsNotNone(proc.poll())

    def test_device_without_detach_keeps_the_command_in_the_foreground(self):
        self.device.conn.capabilities = frozenset()
        proc = self.start("sleep 0.2")
        self.assertIsNone(self.handler("sleep 0.2")(proc, ""))
        self.assertEqual(self.device.detached, [])
        self.assertEqual(proc.wait(10), 0)

    def test_other_backends_and_pod_commands_keep_hermes_handoff(self):
        calls = []

        def original(*, env_type, **kwargs):
            calls.append(env_type)
            return (lambda proc, out: ("local", proc)) if env_type == "local" else None
        wrapped = core._make_yield_to_background_handler(original)
        common = dict(command="x", cwd="/", effective_task_id="t", task_id="t", session_key="")
        self.assertIsNone(wrapped(env_type="docker", **common))
        handler = wrapped(env_type=core.BACKEND_NAME, **common)
        self.assertEqual(handler("popen", ""), ("local", "popen"))
        self.assertEqual(calls, ["docker", core.BACKEND_NAME, "local"])

    def test_auto_background_through_the_device_environment(self):
        import inspect
        from tools.environments.base import BaseEnvironment
        if "yield_after" not in inspect.signature(BaseEnvironment.execute).parameters:
            self.skipTest("this Hermes has no terminal.auto_background_seconds (hermes-patches 0002)")
        command = "echo started; sleep 2; echo done"
        env = self.env()
        self.addCleanup(env.cleanup)
        began = time.monotonic()
        result = env.execute(command, timeout=30, bounded_capture=True, yield_handler=self.handler(command),
                             yield_after=0.5)
        self.assertLess(time.monotonic() - began, 2)
        self.assertEqual(result.get("yield_reason"), "auto_background", result)
        sid = result["yielded_session_id"]
        self.sessions.append(sid)
        self.assertIn("started", result["output"])
        done = self.registry.wait(sid, timeout=15)
        self.assertEqual(done.get("exit_code"), 0, done)
        self.assertIn("done", done.get("output", ""))
        self.assertEqual(sum(1 for e in self.device.execs if command in e["cmd"]), 1)
        self.device.conn.capabilities = frozenset()
        result = env.execute(command, timeout=30, bounded_capture=True, yield_handler=self.handler(command),
                             yield_after=0.5)
        self.assertEqual((result.get("returncode"), result.get("yielded_session_id")), (0, None), result)
        self.assertIn("done", result["output"])


class RecordingCtx:
    """A PluginContext stand-in that records the hooks registered."""

    def __init__(self):
        self.hooks: dict[str, list] = {}

    def register_hook(self, name, callback):
        self.hooks.setdefault(name, []).append(callback)

    def __getattr__(self, name):
        if name.startswith("register_"):
            return lambda *a, **k: None
        raise AttributeError(name)


class RegisterTests(unittest.TestCase):
    def test_session_list_hook_only_when_hermes_has_it(self):
        sys.path.insert(0, str(Path(core.__file__).parent.parent))
        try:
            import device as plugin
        finally:
            sys.path.pop(0)
        sibling = types.SimpleNamespace(register=lambda ctx, core: None)
        for hooks, want in ((frozenset({"pre_tool_call"}), False),
                            (frozenset({"pre_tool_call", "filter_session_list"}), True)):
            ctx = RecordingCtx()
            with mock.patch.object(core, "valid_hooks", lambda hooks=hooks: hooks), \
                    mock.patch.object(core, "install_hooks", lambda: []), \
                    mock.patch.object(core, "apply_safe_root_policy", lambda: None), \
                    mock.patch.object(core, "_load_sibling", lambda *a: sibling), \
                    mock.patch.object(core, "settings", lambda: {"placeholder": "/", "backend_id": "alice"}):
                plugin.register(ctx)
            self.assertEqual("filter_session_list" in ctx.hooks, want, hooks)
            if want:
                self.assertIs(ctx.hooks["filter_session_list"][0], core.filter_session_list)


class GuardTests(Base):
    def setUp(self):
        super().setUp()
        self.bind()
        self.recorded = {}
        p = mock.patch.object(core, "_record_cwd", lambda task, cwd: self.recorded.__setitem__(task, cwd))
        p.start()
        self.addCleanup(p.stop)
        q = mock.patch("tools.file_tools_paths._terminal_env_type_for_task", lambda task_id="default": "device")
        q.start()
        self.addCleanup(q.stop)

    def guard(self, tool, **args):
        return core.pre_tool_call(tool_name=tool, args=args, task_id="t", session_id=self.session)

    def test_relative_and_home_paths_rewritten(self):
        self.assertEqual(self.guard("read_file", path="f.txt"),
                         {"action": "modify", "args": {"path": str(self.root / "f.txt")}})
        self.assertEqual(self.guard("write_file", path="~/src/new.txt", content="x"),
                         {"action": "modify", "args": {"path": str(self.root / "new.txt")}})
        self.assertIsNone(self.guard("write_file", path=str(self.root / "new.txt"), content="x"))
        self.assertEqual(self.guard("search_files", pattern="x"),
                         {"action": "modify", "args": {"path": str(self.root)}})
        self.assertEqual(self.recorded, {"t": str(self.root)})

    def test_denylist_and_roots(self):
        self.device.conn.roots = [str(self.home)]
        for path in ("~/.ssh/id_rsa", "~/.ssh/config", "~/.aws/credentials", "~/.netrc"):
            r = self.guard("write_file", path=path, content="x")
            self.assertEqual(r["action"], "block", path)
        r = self.guard("write_file", path=str(self.outside / "x"), content="x")
        self.assertIn("outside the device roots", r["message"])

    def test_symlink_into_denied_path_blocked(self):
        self.device.conn.roots = [str(self.home)]
        (self.root / "keys").symlink_to(self.home / ".ssh")
        r = self.guard("patch", path="keys/authorized_keys", old_string="a", new_string="b")
        self.assertEqual(r["action"], "block")
        self.assertIn(".ssh", r["message"])

    def test_v4a_paths_rewritten_and_checked(self):
        body = ("*** Begin Patch\n*** Update File: f.txt\n@@\n-one\n+uno\n*** Add File: ~/src/n.txt\n+hi\n"
                "*** End Patch")
        r = self.guard("patch", mode="patch", patch=body)
        self.assertEqual(r["action"], "modify")
        self.assertIn(f"*** Update File: {self.root / 'f.txt'}\n", r["args"]["patch"])
        self.assertIn(f"*** Add File: {self.root / 'n.txt'}\n", r["args"]["patch"])
        bad = "*** Begin Patch\n*** Add File: ../outside/x\n+hi\n*** End Patch"
        self.assertEqual(self.guard("patch", mode="patch", patch=bad)["action"], "block")

    def test_wrong_backend_blocked(self):
        with mock.patch("tools.file_tools_paths._terminal_env_type_for_task", lambda task_id="default": "local"):
            r = self.guard("web_search", query="x")
        self.assertEqual(r["action"], "block")
        self.assertIn("terminal.backend: device", r["message"])

    def test_pod_only_tools_blocked_for_bound(self):
        self.assertEqual(self.guard("cronjob_manage", action="create")["action"], "block")
        self.assertIsNone(self.guard("web_search", query="x"))

    def test_bound_session_schedules_jobs_that_run_in_the_sandbox(self):
        """With fallback: device a job's runs go to the sandbox, so a bound session may schedule,
        list, pause and remove jobs, but never with the fields that run on the pod."""
        self.cfg.update(fallback="device", fallback_device="d_sb")
        for action in ("create", "list", "pause", "remove"):
            self.assertIsNone(self.guard("cronjob_manage", action=action, prompt="p", schedule="in 2m"))
        for pod_arg in ({"script": "s.py"}, {"monitor": "https://example.com"}, {"no_agent": True},
                        {"workdir": "/opt/data"}, {"monitor_url": "https://example.com"},
                        {"monitor_script": "m.py"}):
            r = self.guard("cronjob_manage", action="update", job_id="j", **pod_arg)
            self.assertEqual(r["action"], "block")
            self.assertIn("would run on the server", r["message"])
        self.assertEqual(self.guard("kanban_create")["action"], "block")
        jobs = {"pod": {"id": "pod", "script": "s.py"}, "agent": {"id": "agent", "prompt": "p"}}
        with mock.patch("cron.jobs.resolve_job_ref", lambda ref: jobs.get(ref)):
            for action in ("run", "resume", "update"):
                r = self.guard("cronjob_manage", action=action, job_id="pod")
                self.assertEqual(r["action"], "block")
                self.assertIn("scheduled job pod has script", r["message"])
                self.assertIsNone(self.guard("cronjob_manage", action=action, job_id="agent"))
            for action in ("pause", "remove", "cancel"):
                self.assertIsNone(self.guard("cronjob_manage", action=action, job_id="pod"))
        marks = []
        fake = mock.Mock()
        fake.choices.return_value.mark_device_job = lambda job, sid: marks.append((job, sid))
        with mock.patch("cron.jobs.resolve_job_ref", lambda ref: jobs.get(ref)), \
                mock.patch.dict(sys.modules, {"hermes_device_browser": fake}):
            self.assertIsNone(self.guard("cronjob_manage", action="update", job_id="agent", prompt="q"))
            self.assertIsNone(self.guard("cronjob_manage", action="run", job_id="agent"))
        self.assertEqual(marks, [("agent", self.session)], "an update from a device session marks the job")

    def test_fail_closed_blocks_file_tools(self):
        self.hub.unbind(self.session)
        self.assertEqual(self.guard("read_file", path="f.txt")["action"], "block")
        self.assertIsNone(self.guard("web_search", query="x"))

    def test_device_errors_block(self):
        with mock.patch.object(core, "device_realpaths", side_effect=RuntimeError("device read failed")):
            r = self.guard("write_file", path="f.txt", content="x")
        self.assertEqual(r["action"], "block")
        self.assertIn("device read failed", r["message"])
        self.hub.detach(self.device.conn)
        self.assertEqual(self.guard("write_file", path="f.txt", content="x")["action"], "block")

    def test_never_bound_passes(self):
        self.session = "other"
        self.store.rows["other"] = None
        self.assertIsNone(self.guard("write_file", path="/etc/passwd", content="x"))


class ReaderTests(Base):
    def setUp(self):
        super().setUp()
        core.install_hooks()

    def test_edit_approval_reads_device(self):
        from acp_adapter import edit_approval
        self.bind()
        (self.root / "sub" / "e.txt").write_text("\ufeffhello\n")
        self.assertEqual(edit_approval._required_path({"path": "sub/e.txt"}), str(self.root / "sub" / "e.txt"))
        self.assertEqual(edit_approval._required_path({"path": "~/x"}), str(self.home / "x"))
        self.assertEqual(edit_approval._read_text_if_exists(str(self.root / "sub" / "e.txt")), "hello\n")
        self.assertIsNone(edit_approval._read_text_if_exists(str(self.root / "missing")))
        with self.assertRaises(OSError):
            edit_approval._read_text_if_exists(str(self.root / "sub"))
        p = edit_approval.build_edit_proposal("write_file", {"path": "sub/e.txt", "content": "new"})
        self.assertEqual((p.path, p.old_text), (str(self.root / "sub" / "e.txt"), "hello\n"))
        self.hub.detach(self.device.conn)
        with self.assertRaises(OSError):
            edit_approval._read_text_if_exists(str(self.root / "f.txt"))

    def test_edit_approval_never_bound_reads_pod(self):
        from acp_adapter import edit_approval
        pod_file = self.outside / "pod.txt"
        pod_file.write_text("pod")
        self.assertEqual(edit_approval._read_text_if_exists(str(pod_file)), "pod")

    def test_checkpoint_skipped_unless_on_pod(self):
        calls = []
        wrapped = core._make_ensure_checkpoint(lambda mgr, wd, reason: calls.append(wd) or True)
        self.bind()
        self.assertFalse(wrapped(object(), str(self.root), "before write_file"))
        self.hub.unbind(self.session)
        self.assertFalse(wrapped(object(), str(self.root), "before write_file"))
        self.session = "other"
        self.store.rows["other"] = None
        self.assertTrue(wrapped(object(), "/srv/pod", "before write_file"))
        self.assertEqual(calls, ["/srv/pod"])

    def expand(self, text, cwd=None):
        from agent.context_references import preprocess_context_references
        return preprocess_context_references(text, cwd=str(cwd or self.placeholder), context_length=200_000)

    def test_file_and_folder_refs_read_device(self):
        self.bind()
        r = self.expand("see @file:f.txt:2-3 and @folder:sub")
        self.assertIn("two\nthree", r.message)
        self.assertNotIn("one", r.message.split("Attached Context")[1])
        self.assertIn("sub/", r.message)
        r = self.expand("@file:../outside/x")
        self.assertIn("outside the session's device directory", r.message)

    def test_git_refs_run_on_device(self):
        self.bind()
        subprocess.run(["git", "init", "-q", str(self.root)], check=True)
        subprocess.run(["git", "-C", str(self.root), "add", "f.txt"], check=True)
        (self.root / "f.txt").write_text("changed\n")
        r = self.expand("look @diff")
        self.assertIn("+changed", r.message)

    def test_fail_closed_refs_refused_and_never_bound_on_pod(self):
        self.bind()
        self.hub.unbind(self.session)
        r = self.expand("@file:f.txt")
        self.assertIn("does not run on the server", r.message)
        self.session = "other"
        self.store.rows["other"] = None
        (self.outside / "p.txt").write_text("pod file")
        r = self.expand("@file:p.txt", cwd=self.outside)
        self.assertIn("pod file", r.message)

    def test_placeholder_cwd_refs_fail_closed(self):
        self.session = "other"
        self.store.rows["other"] = None
        (self.placeholder / "p.txt").write_text("pod placeholder file")
        r = self.expand("@file:p.txt")
        self.assertNotIn("pod placeholder file", r.message)
        self.assertIn("placeholder", r.message)

    def test_inbound_references_sets_session(self):
        from gateway import session_context_refs
        seen = []
        original = session_context_refs.inbound_references.__hermes_device_original__

        async def fake_original(runner, source, key, text):
            seen.append(core.SESSION_OVERRIDE.get())
            return text, None

        wrapped = core._make_inbound_references(fake_original)
        from gateway.config import Platform
        source = mock.Mock(platform=Platform.LOCAL, chat_id="local-abc")
        asyncio.run(wrapped(mock.Mock(), source, "key", "t"))
        runner = mock.Mock()
        runner.session_store.peek_session_id.return_value = "sid-9"
        asyncio.run(wrapped(runner, mock.Mock(platform="telegram"), "key", "t"))
        self.assertEqual(seen, ["local-abc", "sid-9"])
        self.assertIsNone(core.SESSION_OVERRIDE.get())
        self.assertTrue(callable(original))

    def test_managed_bypass_refused_for_bound(self):
        self.bind()

        async def fake_original(authority, row, policy):
            return "expanded", None

        wrapped = core._make_managed_turn_text(fake_original)
        text, refusal = asyncio.run(wrapped(None, {"target_session_id": self.session},
                                            mock.Mock(ignore_user_config=True, cwd=str(self.placeholder))))
        self.assertIsNone(text)
        self.assertIn("Safe-mode", refusal)
        self.assertEqual(asyncio.run(wrapped(None, {"target_session_id": self.session},
                                             mock.Mock(ignore_user_config=False))), ("expanded", None))

    def test_device_reference_provider(self):
        sys.path.insert(0, str(_HERE.parent))
        try:
            plugin = importlib.import_module("device")
        finally:
            sys.path.pop(0)
        provider = plugin.make_reference_provider(core)
        self.bind()
        block = asyncio.run(provider.expand("f.txt"))
        self.assertIn("one\ntwo", block)
        items = asyncio.run(provider.autocomplete("s"))
        self.assertEqual([i.text for i in items], ["sub/"])
        self.hub.unbind(self.session)
        with self.assertRaises(ValueError):
            asyncio.run(provider.expand("f.txt"))


class SideAgentTests(Base):
    def test_side_spawn_links_to_starting_session(self):
        self.bind()
        captured = []
        wrapped = core._make_side_spawn(lambda authority, ref, task_id, *a, **k: captured.append(task_id))
        authority = mock.Mock()
        authority.physical_target.return_value = self.session
        wrapped(authority, mock.Mock(session_id=self.session), "bg_1", "background.complete", lambda: "")
        self.assertEqual(captured, ["bg_1"])
        self.assertEqual(self.hub.resolve("bg_1").state, core.STATE_BOUND)

    def test_background_task_without_parent_fails_closed(self):
        async def original(self_, prompt, source, task_id, *a, **k):
            return task_id

        wrapped = core._make_run_background_task(original)
        runner = mock.Mock()
        runner._session_key_for_source.return_value = "k"
        runner.session_store.peek_session_id.return_value = None
        source = mock.Mock(platform="telegram")
        self.assertEqual(asyncio.run(wrapped(runner, "p", source, "bg_2")), "bg_2")
        self.store.rows["bg_2"] = None
        self.assertEqual(self.hub.resolve("bg_2").state, core.STATE_FAIL_CLOSED)


class InitSessionStateTests(Base):
    def test_records_delegate_parent(self):
        def original(agent, session_id, session_db, parent_session_id, *rest):
            agent.session_id = session_id
        wrapped = core._make_init_session_state(original)
        agent = mock.Mock()
        wrapped(agent, "child-1", None, self.session, None, None, False, 1, 1, 1)
        self.bind()
        self.assertEqual(self.hub.resolve("child-1").state, core.STATE_BOUND)


class SafeRootTests(Base):
    def test_overlapping_device_roots_removed(self):
        self.cfg["device_write_roots"] = ["/home/alice/src", "/tmp/dev", "/opt", str(self.placeholder / "x")]
        env = {"HERMES_WRITE_SAFE_ROOT": os.pathsep.join(
            ["/home/alice/src", "/tmp/dev", "/opt", str(self.placeholder / "x"), "/srv/other"])}
        removed = core.apply_safe_root_policy(env)
        self.assertEqual(sorted(removed), sorted(["/tmp/dev", "/opt", str(self.placeholder / "x")]))
        self.assertEqual(env["HERMES_WRITE_SAFE_ROOT"], os.pathsep.join(["/home/alice/src", "/srv/other"]))


class CommandTests(Base):
    def test_device_command(self):
        self.assertIn("not bound", core.device_command("", session_id=self.session))
        self.assertIn("pc [d_pc]", core.device_command("list", session_id=self.session))
        out = core.device_command(f"pc {self.root}", session_id=self.session)
        self.assertIn("now runs commands on pc [d_pc]", out)
        self.assertIn(str(self.root), core.device_command("", session_id=self.session))
        self.store.rows["c1"] = self.session
        self.assertIn("inherited from session sess-1", core.device_command("", session_id="c1"))
        self.assertIn("fails closed", core.device_command("off", session_id="c1"))
        self.assertIsNone(self.hub.binding(self.session))
        self.assertIn("Rebind", core.device_command("", session_id=self.session))
        self.assertIn("Not bound", core.device_command(f"nope {self.root}", session_id=self.session))
        self.assertIn("now runs commands on pc", core.device_command(f"pc {self.root}", session_id=self.session))
        other = FakeDevice(self.hub, str(self.root), str(self.devtmp), str(self.home), device_id="d_lap", name="lap")
        self.addCleanup(other.close)
        self.assertIn("now runs commands on lap", core.device_command(f"lap {self.root}", session_id=self.session))
        self.assertEqual(self.hub.binding(self.session).device_id, "d_lap")
        out = core.device_command(f"pc {self.root}", session_id="c1")
        self.assertIn("Not bound", out)
        self.assertIn("/device off", out)

    def test_device_command_refuses_other_users(self):
        """/device binds only to a computer of the session's hub user (docs/browser.md section 13)."""
        self.device.conn.user = "alice"
        bob = FakeDevice(self.hub, str(self.root), str(self.devtmp), str(self.home), device_id="d_bob", name="bob")
        self.addCleanup(bob.close)
        bob.conn.user = "bob"
        out = core.device_command(f"bob {self.root}", session_id=self.session)
        self.assertIn("more than one hub user", out)
        self.assertIsNone(self.hub.binding(self.session))
        self.cfg["user"] = "alice"
        self.assertIn("another hub user", core.device_command(f"bob {self.root}", session_id=self.session))
        self.assertIn("now runs commands on pc", core.device_command(f"pc {self.root}", session_id=self.session))
        del self.cfg["user"]
        self.assertIn("another hub user", core.device_command(f"bob {self.root}", session_id=self.session))
        self.assertEqual(self.hub.binding(self.session).device_id, "d_pc")


class SandboxTests(Base):
    """``fallback: device``: never-bound sessions run on the sandbox device, never on the pod."""

    def setUp(self):
        super().setUp()
        self.work = self.base / "work"
        (self.work / ".ssh").mkdir(parents=True)
        self.cfg.update(fallback="device", fallback_device="d_sb", fallback_root=str(self.work))
        self.sandbox = FakeDevice(self.hub, str(self.work), str(self.devtmp), str(self.work), device_id="d_sb",
                                  name="sandbox")
        self.addCleanup(self.sandbox.close)
        self.sdir = self.work / self.session

    def mkdirs(self, device=None):
        return [m for m in (device or self.sandbox).execs if m["cmd"].startswith("mkdir -p -- ")]

    def test_never_bound_runs_in_its_sandbox_dir(self):
        env = self.env()
        r = env.execute("pwd", cwd=str(self.placeholder))
        self.assertEqual((r["returncode"], r["output"].strip()), (0, str(self.sdir)))
        self.assertTrue(self.sdir.is_dir())
        self.assertIsNone(env._local)
        self.assertFalse(env.uses_host_filesystem)
        self.assertEqual(self.device.execs, [])
        self.assertEqual(env.get_temp_dir(), str(self.devtmp))
        env.execute("mkdir -p a && cd a")
        self.assertEqual(env.cwd, str(self.sdir / "a"))
        self.assertEqual(len(self.mkdirs()), 1)
        self.assertEqual(self.mkdirs()[0]["cwd"], str(self.work))

    def test_not_recorded_as_a_binding(self):
        self.env().execute("true")
        self.assertIsNone(self.hub.binding(self.session))
        self.assertFalse(self.hub.ever_bound(self.session))
        self.assertFalse((self.base / "state" / "bindings.json").exists())
        self.assertEqual(self.hub.resolve(self.session).state, core.STATE_NEVER_BOUND)
        res = core.resolve_current()
        self.assertEqual((res.state, res.sandbox, res.via, res.binding.dir),
                         (core.STATE_BOUND, True, self.session, str(self.sdir)))

    def test_lineage_shares_the_root_dir(self):
        self.store.rows.update({"c1": "sess-1", "c2": "c1", "other": None})
        self.session = "c2"
        self.assertEqual(self.env().execute("pwd")["output"].strip(), str(self.sdir))
        self.session = "other"
        self.assertEqual(self.env().execute("pwd")["output"].strip(), str(self.work / "other"))

    def test_offline_fails_and_never_runs_on_pod(self):
        env = self.env()
        env.execute("true")
        self.hub.detach(self.sandbox.conn)
        r = env.execute("pwd")
        self.assertEqual(r["returncode"], 1)
        self.assertIn("sandbox device d_sb, which is not connected", r["output"])
        self.assertIn("never on the server", r["output"])
        self.assertIsNone(env._local)
        self.assertFalse(env.uses_host_filesystem)

    def test_dropped_while_running_is_lost(self):
        env = self.env()
        env.execute("true")
        res = core.resolve_current()
        self.hub.detach(self.sandbox.conn)
        with mock.patch.object(env, "_resolve", lambda: res):
            r = env.execute("pwd")
        self.assertEqual(r["returncode"], 255)
        self.assertIn("sandbox device d_sb is not connected", r["output"])

    def test_by_name_and_ambiguous_name(self):
        self.cfg["fallback_device"] = "sandbox"
        self.assertEqual(self.env().execute("pwd")["output"].strip(), str(self.sdir))
        twin = FakeDevice(self.hub, str(self.work), str(self.devtmp), str(self.work), device_id="d_sb2",
                          name="sandbox")
        self.addCleanup(twin.close)
        r = self.env().execute("pwd")
        self.assertEqual(r["returncode"], 1)
        self.assertIn("ambiguous", r["output"])

    def test_missing_settings_refuse(self):
        self.cfg["fallback_device"] = ""
        self.assertEqual(core.fallback(), "refuse")
        self.assertIn("refuses unbound sessions", self.env().execute("pwd")["output"])
        self.cfg.update(fallback_device="d_sb", fallback_root="work")
        r = self.env().execute("pwd")
        self.assertEqual(r["returncode"], 1)
        self.assertIn("no sandbox", r["output"])
        self.cfg["fallback_root"] = str(self.outside)
        self.assertIn("do not hold fallback_root", self.env().execute("pwd")["output"])
        self.assertEqual(self.sandbox.execs, [])

    def test_explicit_bind_wins_and_moves_to_the_pc(self):
        env = self.env()
        self.assertEqual(env.execute("pwd")["output"].strip(), str(self.sdir))
        out = core.device_command(f"pc {self.root}", session_id=self.session)
        self.assertIn("now runs commands on pc [d_pc]", out)
        self.assertEqual(env.execute("pwd")["output"].strip(), str(self.root))
        self.assertTrue(self.hub.ever_bound(self.session))
        self.hub.unbind(self.session)
        r = env.execute("pwd")
        self.assertEqual(r["returncode"], 1)
        self.assertIn("was bound to a device", r["output"])

    def test_reconnect_creates_the_dir_again(self):
        env = self.env()
        env.execute("true")
        self.sdir.rmdir()
        newer = self.sandbox.make_conn("d_sb", "sandbox", str(self.devtmp), str(self.work))
        self.hub.attach(newer)
        self.sandbox.conn = newer
        self.assertEqual(env.execute("pwd")["output"].strip(), str(self.sdir))
        self.assertEqual(len(self.mkdirs()), 2)

    def test_mkdir_failure_is_reported(self):
        (self.work / "blocker").write_text("x")
        self.session = "blocker"
        self.store.rows["blocker"] = None
        r = self.env().execute("pwd")
        self.assertNotEqual(r["returncode"], 0)
        self.assertIn("could not create the sandbox directory", r["output"])

    def test_placeholder_cwd_still_fails_closed(self):
        with mock.patch.object(core, "session_cwd", lambda: str(self.placeholder)):
            r = self.env().execute("pwd")
        self.assertIn("placeholder", r["output"])
        self.assertEqual(self.sandbox.execs, [])

    def test_dir_names(self):
        self.assertEqual(core.sandbox_dir_name("20261110_120000_ab12"), "20261110_120000_ab12")
        for odd in ("..", ".", "a/b", "../x", "", "x" * 200, "é"):
            name = core.sandbox_dir_name(odd)
            self.assertRegex(name, r"^[A-Za-z0-9_-][A-Za-z0-9._~-]*$", odd)
            self.assertNotIn("/", name)
        self.assertNotEqual(core.sandbox_dir_name("a/b"), core.sandbox_dir_name("a_b"))
        hashed = core.sandbox_dir_name("a:b")
        self.assertIn("~", hashed)
        self.assertNotEqual(core.sandbox_dir_name(hashed), hashed)

    def test_deleted_dir_is_created_again(self):
        env = self.env()
        env.execute("true")
        r = env.execute('rm -rf "$PWD"')
        self.assertEqual(r["returncode"], 0)
        r = env.execute("pwd")
        self.assertEqual(r["returncode"], 126)
        r = env.execute("pwd")
        self.assertEqual((r["returncode"], r["output"].strip()), (0, str(self.sdir)))
        self.assertEqual(len(self.mkdirs()), 2)

    def test_nested_fallback_root_is_created(self):
        self.cfg["fallback_root"] = str(self.work / "sessions")
        r = self.env().execute("pwd")
        self.assertEqual((r["returncode"], r["output"].strip()), (0, str(self.work / "sessions" / self.session)))
        self.assertEqual(self.mkdirs()[0]["cwd"], str(self.work))

    def test_a_seen_id_never_matches_a_name(self):
        self.hub.detach(self.sandbox.conn)
        pc = FakeDevice(self.hub, str(self.work), str(self.devtmp), str(self.work), device_id="d_pc2", name="d_sb")
        self.addCleanup(pc.close)
        r = self.env().execute("pwd")
        self.assertEqual(r["returncode"], 1)
        self.assertIn("not connected", r["output"])
        self.assertEqual(pc.execs, [])

    def test_safe_mode_turn_refused_with_the_real_reason(self):
        async def fake_original(authority, row, policy):
            return "expanded", None
        wrapped = core._make_managed_turn_text(fake_original)
        text, refusal = asyncio.run(wrapped(None, {"target_session_id": self.session},
                                            mock.Mock(ignore_user_config=True, cwd=str(self.outside))))
        self.assertIsNone(text)
        self.assertIn("runs in the sandbox device d_sb", refusal)
        self.assertNotIn("Try again", refusal)

    def test_guard_rewrites_to_the_sandbox_and_checks_its_home(self):
        recorded = {}
        with mock.patch.object(core, "_record_cwd", lambda task, cwd: recorded.__setitem__(task, cwd)), \
                mock.patch("tools.file_tools_paths._terminal_env_type_for_task", lambda task_id="default": "device"):
            r = core.pre_tool_call(tool_name="write_file", args={"path": "x.txt", "content": "x"}, task_id="t")
            self.assertEqual(r, {"action": "modify", "args": {"path": str(self.sdir / "x.txt")}})
            r = core.pre_tool_call(tool_name="write_file", args={"path": "~/.ssh/authorized_keys", "content": "x"},
                                   task_id="t")
            self.assertEqual(r["action"], "block")
            self.assertIsNone(core.pre_tool_call(tool_name="cronjob_manage",
                                                 args={"action": "create", "prompt": "p", "schedule": "in 2m",
                                                       "script": "", "no_agent": False}, task_id="t"))
            for pod_arg in ({"script": "s.py"}, {"monitor": "https://example.com"}, {"no_agent": True},
                            {"workdir": "/opt/data"}, {"monitor_url": "https://example.com"}):
                r = core.pre_tool_call(tool_name="cronjob_manage", args={"action": "create", **pod_arg}, task_id="t")
                self.assertEqual(r["action"], "block")
                self.assertIn("would run on the server", r["message"])
            r = core.pre_tool_call(tool_name="kanban_create", args={}, task_id="t")
            self.assertIn("runs in the sandbox device", r["message"])
            self.hub.detach(self.sandbox.conn)
            r = core.pre_tool_call(tool_name="read_file", args={"path": "x.txt"}, task_id="t")
            self.assertEqual(r["action"], "block")
            self.assertIn("sandbox", r["message"])
        with mock.patch("tools.file_tools_paths._terminal_env_type_for_task", lambda task_id="default": "local"):
            self.hub.attach(self.sandbox.conn)
            r = core.pre_tool_call(tool_name="read_file", args={"path": "x.txt"}, task_id="t")
            self.assertIn("terminal.backend: device", r["message"])
        self.assertEqual(recorded, {"t": str(self.sdir)})

    def test_readers_use_the_sandbox(self):
        core.install_hooks()
        from acp_adapter import edit_approval
        from agent.context_references import preprocess_context_references
        self.env().execute("printf 'in sandbox\\n' > s.txt")
        self.assertEqual(edit_approval._required_path({"path": "s.txt"}), str(self.sdir / "s.txt"))
        self.assertEqual(edit_approval._read_text_if_exists(str(self.sdir / "s.txt")), "in sandbox\n")
        r = preprocess_context_references("see @file:s.txt", cwd=str(self.outside), context_length=200_000)
        self.assertIn("in sandbox", r.message)
        calls = []
        wrapped = core._make_ensure_checkpoint(lambda mgr, wd, reason: calls.append(wd) or True)
        self.assertFalse(wrapped(object(), str(self.sdir), "before write_file"))
        self.hub.detach(self.sandbox.conn)
        with self.assertRaises(OSError):
            edit_approval._read_text_if_exists(str(self.sdir / "s.txt"))
        self.assertEqual(calls, [])

    def test_device_command(self):
        out = core.device_command("", session_id=self.session)
        self.assertIn(f"runs in the sandbox device sandbox [d_sb] in {self.sdir}", out)
        self.assertIn("runs in the sandbox device until you bind it", core.device_command("off", session_id=self.session))
        with mock.patch.object(core, "session_cwd", lambda: str(self.placeholder)):
            self.assertIn("placeholder", core.device_command("", session_id=self.session))
        self.hub.detach(self.sandbox.conn)
        self.assertIn("not connected", core.device_command("", session_id=self.session))

    def test_safe_root_policy_checks_fallback_root(self):
        self.cfg["fallback_root"] = str(self.placeholder)
        env = {"HERMES_WRITE_SAFE_ROOT": os.pathsep.join([str(self.placeholder), "/srv/other"])}
        self.assertEqual(core.apply_safe_root_policy(env), [str(self.placeholder)])
        self.assertEqual(env["HERMES_WRITE_SAFE_ROOT"], "/srv/other")


class MappedDevice(FakeDevice):
    """A FakeDevice whose files live in a pod directory (``backing``) but whose paths are device
    paths under ``prefix``, which does not exist on the pod. Commands, cwds and output are
    translated, so a reader that opens the device path on the pod finds nothing."""

    def __init__(self, hub, prefix: str, backing: str, root: str, tmp: str, home: str, **kwargs):
        self.prefix, self.backing = prefix, os.path.realpath(backing)
        super().__init__(hub, self.to_local(root), tmp, home, **kwargs)

    def to_local(self, text: str) -> str:
        return text.replace(self.prefix, self.backing)

    def to_device(self, text: str) -> str:
        return text.replace(self.backing, self.prefix)

    def make_conn(self, device_id, name, tmp, home):
        conn = super().make_conn(device_id, name, tmp, home)
        conn.roots = [self.to_device(self.root)]
        return conn

    def _run(self, msg: dict) -> None:
        super()._run(dict(msg, cwd=self.to_local(msg["cwd"]), cmd=self.to_local(msg["cmd"])))

    def _reply(self, msg: dict) -> None:
        super()._reply({k: self.to_device(v) if k in ("data", "error") else v for k, v in msg.items()})


class EditApprovalDiffTests(Base):
    """The diff that ACP edit approval sends to the editor, through Hermes's real dispatch
    (model_tools.handle_function_call: pre_tool_call guard, then edit approval, then the ACP
    request_permission tool call), is against the device file, for a bound session and for a
    sandbox session. The device paths do not exist on the pod, so a pod read would show a
    whole-file create."""

    OLD = "def f():\n    return 1\n"

    def setUp(self):
        super().setUp()
        core.install_hooks()
        self.prefix = f"/hermes-device-test-{os.urandom(6).hex()}"
        self.assertFalse(os.path.exists(self.prefix))
        for p in (mock.patch.object(core, "_record_cwd", lambda task, cwd: None),
                  mock.patch("tools.file_tools_paths._terminal_env_type_for_task",
                             lambda task_id="default": "device")):
            p.start()
            self.addCleanup(p.stop)

    def mapped(self, device_id: str, name: str, root: str, home: str) -> MappedDevice:
        backing = self.base / "devfs"
        dev = MappedDevice(self.hub, self.prefix, str(backing), root, self.prefix + "/tmp", home,
                           device_id=device_id, name=name)
        for d in (root, self.prefix + "/tmp"):
            Path(dev.to_local(d)).mkdir(parents=True, exist_ok=True)
        self.addCleanup(dev.close)
        return dev

    def approve(self, tool: str, args: dict):
        """Run ``tool`` through Hermes's dispatch with an ACP edit requester bound and the plugin's
        pre_tool_call guard registered. The editor denies, so nothing is written. Returns the
        tool result and the content of every permission request."""
        import model_tools
        from acp.schema import AllowedOutcome, RequestPermissionResponse
        from acp_adapter import edit_approval
        from hermes_cli import lifecycle

        requests = []

        async def request_permission(session_id, tool_call, options):
            requests.append(tool_call)
            return RequestPermissionResponse(outcome=AllowedOutcome(option_id="deny", outcome="selected"))

        original_hooks = lifecycle._plugin_hooks

        def plugin_hooks(hook_name, **kwargs):
            if hook_name == "pre_tool_call":
                return [r for r in (core.pre_tool_call(**kwargs),) if r is not None]
            return original_hooks(hook_name, **kwargs)

        loop = asyncio.new_event_loop()
        thread = threading.Thread(target=loop.run_forever, daemon=True)
        thread.start()
        token = edit_approval.set_edit_approval_requester(
            edit_approval.make_acp_edit_approval_requester(request_permission, loop, "acp-1", timeout=20))
        try:
            with mock.patch.object(lifecycle, "_plugin_hooks", plugin_hooks):
                result = model_tools.handle_function_call(tool, dict(args), task_id="t", session_id=self.session)
        finally:
            edit_approval.reset_edit_approval_requester(token)
            loop.call_soon_threadsafe(loop.stop)
            thread.join(5)
            loop.close()
        return json.loads(result), [c for r in requests for c in r.content]

    def check_diffs(self, dev: MappedDevice, directory: str):
        path = posixpath.join(directory, "app.py")
        local = Path(dev.to_local(path))
        local.write_text(self.OLD)
        self.assertFalse(os.path.exists(path))
        from acp_adapter import edit_approval
        pod_read = getattr(edit_approval._read_text_if_exists, core._WRAPPED_MARK)
        self.assertIsNone(pod_read(path))

        v4a = "*** Begin Patch\n*** Update File: app.py\n@@\n def f():\n-    return 1\n+    return 2\n*** End Patch"
        patched = "def f():\n    return 2\n"
        cases = [
            ("write_file", {"path": "app.py", "content": "print(1)\n"}, path, self.OLD, "print(1)\n"),
            ("write_file", {"path": path, "content": "print(1)\n"}, path, self.OLD, "print(1)\n"),
            ("patch", {"path": "app.py", "old_string": "return 1", "new_string": "return 2"},
             path, self.OLD, patched),
            ("patch", {"mode": "replace", "path": "~/" + posixpath.relpath(path, dev.conn.home),
                       "old_string": "return 1", "new_string": "return 2"}, path, self.OLD, patched),
            ("patch", {"mode": "patch", "patch": v4a}, path, self.OLD,
             v4a.replace("File: app.py", f"File: {path}")),
            ("write_file", {"path": "new.py", "content": "x\n"}, posixpath.join(directory, "new.py"), None, "x\n"),
        ]
        for tool, args, want_path, old, new in cases:
            with self.subTest(tool=tool, args=args):
                result, content = self.approve(tool, args)
                self.assertIn("Edit approval denied by ACP client", result.get("error", ""), result)
                self.assertEqual([(c.type, c.path, c.oldText, c.newText) for c in content],
                                 [("diff", want_path, old, new)])
                self.assertEqual(local.read_text(), self.OLD)
        self.assertFalse(os.path.exists(self.prefix))
        self.assertFalse(Path(dev.to_local(posixpath.join(directory, "new.py"))).exists())

    def test_bound_session(self):
        home = self.prefix + "/home/alice"
        dev = self.mapped("d_remote", "desktop", home + "/src", home)
        self.hub.bind(self.session, "d_remote", home + "/src")
        self.check_diffs(dev, home + "/src")

    def test_sandbox_session(self):
        work = self.prefix + "/work"
        self.cfg.update(fallback="device", fallback_device="d_sb", fallback_root=work)
        dev = self.mapped("d_sb", "sandbox", work, work)
        directory = posixpath.join(work, self.session)
        Path(dev.to_local(directory)).mkdir()
        res = core.resolve_current()
        self.assertEqual((res.state, res.sandbox, res.binding.dir), (core.STATE_BOUND, True, directory))
        self.assertIsNone(self.hub.binding(self.session))
        self.check_diffs(dev, directory)
        self.assertEqual(self.device.execs, [])


class HermesImageTests(unittest.TestCase):
    DIGEST = "sha256:" + "0" * 64

    def image(self, env):
        clean = {k: v for k, v in os.environ.items() if k not in (core.IMAGE_REPOSITORY_ENV, core.IMAGE_TAG_ENV)}
        with mock.patch.dict(os.environ, {**clean, **env}, clear=True):
            return core.hermes_image()

    def test_forms(self):
        repo = "registry.example.com/docker.io/nousresearch/hermes-agent"
        for tag, want in ((f"v0.23.1@{self.DIGEST}", f"{repo}:v0.23.1@{self.DIGEST}"),
                          (self.DIGEST, f"{repo}@{self.DIGEST}"),
                          (f"@{self.DIGEST}", f"{repo}@{self.DIGEST}"),
                          ("v0.23.1", f"{repo}:v0.23.1")):
            with self.subTest(tag=tag):
                self.assertEqual(self.image({core.IMAGE_REPOSITORY_ENV: repo, core.IMAGE_TAG_ENV: f" {tag}\n"}), want)

    def test_base_image(self):
        ref = f"registry.example.com/hermes-agent:v0.23.1@{self.DIGEST}"
        for value, want in ((ref, ref), (f" {ref}\n", ref), ("", ""), ("registry.example.com/hermes-agent:v0.23.1", None)):
            with self.subTest(value=value):
                clean = {k: v for k, v in os.environ.items() if k != core.BASE_IMAGE_ENV}
                with mock.patch.dict(os.environ, {**clean, core.BASE_IMAGE_ENV: value}, clear=True):
                    if want is None:
                        with self.assertLogs(core.logger, "WARNING"):
                            self.assertEqual(core.hermes_base_image(), "")
                    else:
                        self.assertEqual(core.hermes_base_image(), want)

    def test_missing_or_bad(self):
        for env in ({}, {core.IMAGE_REPOSITORY_ENV: "a/b"}, {core.IMAGE_TAG_ENV: "v1"},
                    {core.IMAGE_REPOSITORY_ENV: "a b", core.IMAGE_TAG_ENV: "v1"},
                    {core.IMAGE_REPOSITORY_ENV: "a" * core.MAX_HERMES_IMAGE, core.IMAGE_TAG_ENV: "v1"}):
            with self.subTest(env=env):
                with self.assertLogs(core.logger, "WARNING") if len(env) == 2 else contextlib.nullcontext():
                    self.assertEqual(self.image(env), "")


class SessionIdTests(unittest.TestCase):
    def test_reads_only_the_context_variable(self):
        from gateway import session_context
        with mock.patch.dict(os.environ, {"HERMES_SESSION_ID": "from-env"}):
            ctx = __import__("contextvars").Context()
            self.assertEqual(ctx.run(core.current_session_id), "")

            def bound():
                session_context._SESSION_ID.set("from-var")
                return core.current_session_id()
            self.assertEqual(__import__("contextvars").Context().run(bound), "from-var")

            def override():
                core.SESSION_OVERRIDE.set("ov")
                return core.current_session_id()
            self.assertEqual(__import__("contextvars").Context().run(override), "ov")


if __name__ == "__main__":
    unittest.main()

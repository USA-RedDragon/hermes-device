"""Exec leg and HTTP route tests against the real FastAPI router, with a scripted device client
on Starlette's TestClient. Run inside the Hermes runtime with scripts/test-plugin.sh."""
from __future__ import annotations

import base64
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

from fastapi import FastAPI
from starlette.testclient import TestClient
from starlette.websockets import WebSocketDisconnect

_HERE = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(Path(__file__).resolve().parent))
from test_core import Store, core  # noqa: E402

_spec = importlib.util.spec_from_file_location("hermes_device_plugin_api", _HERE / "dashboard" / "plugin_api.py")
plugin_api = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(plugin_api)

PATH = "/api/plugins/device/ws"


def _b64(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode()


class Signer:
    def __init__(self):
        from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
        self.key = Ed25519PrivateKey.from_private_bytes(bytes(range(1, 33)))
        from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat
        self.public_b64 = base64.b64encode(self.key.public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)).decode()

    def sign(self, **overrides) -> str:
        now = int(time.time())
        claims = {"typ": "hermes-device-assertion", "aud": "alice", "leg": "exec", "htm": "GET", "htp": PATH,
                  "sub": "user-1", "device_id": "d_8f2c", "device_name": "desktop", "iat": now, "exp": now + 30,
                  "nonce": _b64(os.urandom(16))}
        claims.update(overrides)
        signed = "v1." + _b64(json.dumps(claims).encode())
        return signed + "." + _b64(self.key.sign(signed.encode()))


class Device(threading.Thread):
    """A scripted device on an open TestClient WebSocket: runs exec with bash in its roots."""

    def __init__(self, ws, root: str):
        super().__init__(daemon=True)
        self.ws, self.root = ws, root
        self.pings: list[int] = []
        self.pongs: list = []
        self.close_code = None

    def run(self):
        while True:
            try:
                msg = self.ws.receive_json()
            except WebSocketDisconnect as exc:
                self.close_code = exc.code
                return
            except Exception:
                return
            if msg["type"] == "exec":
                cwd = os.path.realpath(msg["cwd"])
                if not (cwd == self.root or cwd.startswith(self.root + "/")) or not os.path.isdir(cwd):
                    self.ws.send_json({"type": "exit", "id": msg["id"], "code": 126,
                                       "error": f"device desktop refused: working directory {cwd!r}"})
                    continue
                p = subprocess.run(["bash", "-c", msg["cmd"]], cwd=cwd, capture_output=True, text=True,
                                   input=msg.get("stdin"))
                if p.stdout or p.stderr:
                    self.ws.send_json({"type": "output", "id": msg["id"], "data": p.stdout + p.stderr})
                self.ws.send_json({"type": "exit", "id": msg["id"], "code": p.returncode})
            elif msg["type"] == "ping":
                self.pings.append(msg["seq"])
                self.ws.send_json({"type": "pong", "seq": msg["seq"]})
            elif msg["type"] == "pong":
                self.pongs.append(msg["seq"])


class RouteTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(dir=os.environ.get("TMPDIR"))
        self.addCleanup(self.tmp.cleanup)
        base = Path(os.path.realpath(self.tmp.name))
        self.root = base / "home" / "src"
        (self.root / "sub").mkdir(parents=True)
        (self.root / ".hidden").mkdir()
        (self.root / "f.txt").write_text("x")
        self.outside = base / "outside"
        self.outside.mkdir()
        self.store = Store()
        self.store.rows.update({"s1": None, "c1": "s1"})
        self.hub = core.Hub(base / "bindings.json", parent_lookup=self.store.lookup)
        self.signer = Signer()
        self.cfg = {"backend_id": "alice", "placeholder": str(base / "ph"), "hub_public_key_env": "TEST_HUB_KEY"}
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

    def hello(self, **overrides):
        msg = {"type": "hello", "version": 2, "roots": [str(self.root)], "tmp_dir": str(self.root),
               "home": str(self.root.parent), "hostname": "desktop", "os": "linux", "arch": "amd64",
               "shell": "bash", "client_version": "v0.2.0"}
        msg.update(overrides)
        return msg

    def connect(self, header=None, **hello):
        headers = {"X-Hermes-Device-Assertion": header if header is not None else self.signer.sign()}
        ws = self.client.websocket_connect(PATH, headers=headers).__enter__()
        self.addCleanup(lambda: ws.__exit__(None, None, None))
        ws.send_json(self.hello(**hello))
        return ws

    def close_code(self, ws) -> int:
        with self.assertRaises(WebSocketDisconnect) as ctx:
            while True:
                ws.receive_json()
        return ctx.exception.code

    def online(self, **kw):
        ws = self.connect(**kw)
        welcome = ws.receive_json()
        device = Device(ws, str(self.root))
        device.start()
        return welcome, device

    # exec leg
    def test_assertion_refusals_close_4403(self):
        for header in ("", "v1.x.y", self.signer.sign(htp="/acp"), self.signer.sign(aud="bob"),
                       self.signer.sign(leg="acp"), self.signer.sign(exp=int(time.time()) - 60)):
            with self.subTest(header=header[:20]):
                with self.client.websocket_connect(PATH, headers={"X-Hermes-Device-Assertion": header}) as ws:
                    self.assertEqual(self.close_code(ws), core.CLOSE_ASSERTION_REFUSED)
        with self.client.websocket_connect(PATH) as ws:
            self.assertEqual(self.close_code(ws), core.CLOSE_ASSERTION_REFUSED)

    def test_replayed_assertion_refused(self):
        header = self.signer.sign()
        with self.client.websocket_connect(PATH, headers={"X-Hermes-Device-Assertion": header}) as ws:
            ws.send_json(self.hello())
            self.assertEqual(ws.receive_json()["type"], "welcome")
        with self.client.websocket_connect(PATH, headers={"X-Hermes-Device-Assertion": header}) as ws:
            self.assertEqual(self.close_code(ws), core.CLOSE_ASSERTION_REFUSED)

    def test_hello_rules(self):
        self.assertEqual(self.close_code(self.connect(version=1)), core.CLOSE_UNSUPPORTED_VERSION)
        self.assertEqual(self.close_code(self.connect(os="windows")), core.CLOSE_UNSUPPORTED_PLATFORM)
        self.assertEqual(self.close_code(self.connect(type="exec")), core.CLOSE_PROTOCOL_ERROR)
        with mock.patch.object(core, "HELLO_TIMEOUT_S", 0.2):
            with self.client.websocket_connect(PATH, headers={"X-Hermes-Device-Assertion": self.signer.sign()}) as ws:
                self.assertEqual(self.close_code(ws), core.CLOSE_PROTOCOL_ERROR)

    def test_welcome_exec_and_devices(self):
        welcome, device = self.online()
        self.assertEqual(welcome, {"type": "welcome", "version": 2, "device_id": "d_8f2c", "name": "desktop",
                                   "ping_interval": 25, "plugin_version": "0.4.1", "hermes_version": "v0.22.7"})
        code, out, _ = self.hub.run("d_8f2c", "echo hi; exit 4", str(self.root))
        self.assertEqual((code, out), (4, "hi\n"))
        devices = self.client.get("/api/plugins/device/devices").json()["devices"]
        self.assertEqual([(d["device_id"], d["name"], d["roots"]) for d in devices],
                         [("d_8f2c", "desktop", [str(self.root)])])
        self.assertEqual(set(devices[0]), {"device_id", "name", "hostname", "os", "arch", "home", "roots",
                                           "client_version", "connected_at", "capabilities"})

    def test_pings_both_ways(self):
        with mock.patch.object(core, "PING_INTERVAL_S", 0.1):
            ws = self.connect()
            ws.receive_json()
            ws.send_json({"type": "ping", "seq": 7})
            got = [ws.receive_json() for _ in range(3)]
        self.assertIn({"type": "pong", "seq": 7}, got)
        self.assertIn({"type": "ping", "seq": 1}, got)

    def test_silence_closes_4408(self):
        with mock.patch.object(core, "SILENCE_TIMEOUT_S", 0.3):
            ws = self.connect()
            ws.receive_json()
            self.assertEqual(self.close_code(ws), core.CLOSE_PING_TIMEOUT)

    def test_newest_wins_4409_and_pending_lost(self):
        ws1 = self.connect()
        ws1.receive_json()
        proc = self.hub.start("d_8f2c", cmd="sleep 5", cwd=str(self.root), login=False, timeout=10, stdin=None)
        self.assertEqual(ws1.receive_json()["type"], "exec")
        ws2 = self.connect()
        self.assertEqual(ws2.receive_json()["type"], "welcome")
        self.assertEqual(proc.wait(5), core.EXIT_LOST)
        proc.stdout.close()
        self.assertEqual(self.close_code(ws1), core.CLOSE_REPLACED)

    def test_disconnect_detaches(self):
        welcome, device = self.online()
        device.ws.close()
        deadline = time.monotonic() + 5
        while self.hub.device("d_8f2c") is not None and time.monotonic() < deadline:
            time.sleep(0.02)
        self.assertIsNone(self.hub.device("d_8f2c"))

    def test_dev_static_tokens(self):
        self.assertEqual(self.close_code(self.connect_dev(name="pc", token="t" * 20)), core.CLOSE_ASSERTION_REFUSED)
        self.cfg.update(dev_static_tokens=True, devices={"pc": {"token": "t" * 20}})
        ws = self.connect_dev(version=1, name="pc", token="t" * 20)
        self.assertEqual(ws.receive_json()["device_id"], "pc")
        self.assertEqual(self.close_code(self.connect_dev(name="pc", token="wrong")), core.CLOSE_UNAUTHORIZED)
        self.assertEqual(self.close_code(self.connect(version=1)), core.CLOSE_UNSUPPORTED_VERSION)

    def connect_dev(self, **hello):
        ws = self.client.websocket_connect(PATH).__enter__()
        self.addCleanup(lambda: ws.__exit__(None, None, None))
        ws.send_json(self.hello(**hello))
        return ws

    # HTTP routes
    def test_info(self):
        r = self.client.get("/api/plugins/device/info").json()
        self.assertEqual(r, {"plugin_version": "0.4.1", "hermes_version": "v0.22.7", "protocol_version": 3,
                             "backend_id": "alice", "placeholder": self.cfg["placeholder"]})

    def test_binding_routes(self):
        url = "/api/plugins/device/sessions/s1/binding"
        r = self.client.put(url, json={"device_id": "d_8f2c", "dir": str(self.root)})
        self.assertEqual((r.status_code, r.json()["error"]), (404, "unknown_device"))
        self.online()
        for body, status, code in (([], 400, "bad_request"), ({"device_id": "d_8f2c"}, 400, "bad_request"),
                                   ({"device_id": "d_8f2c", "dir": "rel"}, 400, "bad_dir"),
                                   ({"device_id": "d_8f2c", "dir": str(self.outside)}, 422, "dir_refused")):
            with self.subTest(body=body):
                r = self.client.put(url, json=body)
                self.assertEqual((r.status_code, r.json()["error"]), (status, code))
                self.assertIn("message", r.json())
        r = self.client.put(url, json={"device_id": "d_8f2c", "dir": str(self.root / "sub" / "..")})
        self.assertEqual(r.status_code, 200)
        binding = r.json()
        self.assertEqual(set(binding), {"session_id", "device_id", "dir", "generation", "bound_at"})
        self.assertEqual(binding["dir"], str(self.root))
        r = self.client.put(url, json={"device_id": "d_8f2c", "dir": str(self.root)})
        self.assertEqual(r.json()["generation"], binding["generation"])
        r = self.client.get("/api/plugins/device/sessions/c1/binding")
        self.assertEqual(r.status_code, 200)
        self.assertEqual(r.json()["resolved"], {"state": "bound", "via": "s1"})
        r = self.client.put(url, json={"device_id": "d_other", "dir": str(self.root)})
        self.assertEqual(r.status_code, 404)
        self.hub._seen.add("d_other")
        r = self.client.put(url, json={"device_id": "d_other", "dir": str(self.root)})
        self.assertEqual((r.status_code, r.json()["error"]), (409, "device_offline"))
        self.assertEqual(len(self.client.get("/api/plugins/device/bindings").json()["bindings"]), 1)
        self.assertEqual(self.client.delete(url).status_code, 204)
        r = self.client.delete(url)
        self.assertEqual((r.status_code, r.json()["error"]), (404, "not_bound"))
        r = self.client.get("/api/plugins/device/sessions/c1/binding")
        self.assertEqual((r.status_code, r.json()["error"]), (404, "not_bound"))
        self.assertEqual(r.json()["resolved"], {"state": "fail_closed", "via": None})
        r = self.client.get("/api/plugins/device/sessions/ghost/binding")
        self.assertEqual(r.json()["resolved"]["state"], "fail_closed")

    def test_binding_route_names_the_sandbox(self):
        url = "/api/plugins/device/sessions/c1/binding"
        self.assertEqual(self.client.get(url).json()["resolved"], {"state": "never_bound", "via": None})
        self.cfg.update(fallback="device", fallback_device="d_8f2c", fallback_root=str(self.root))
        r = self.client.get(url)
        self.assertEqual((r.status_code, r.json()["resolved"]),
                         (404, {"state": "never_bound", "via": None, "sandbox": None}))
        self.online()
        r = self.client.get(url)
        self.assertEqual(r.json()["resolved"]["sandbox"], {"device_id": "d_8f2c", "dir": str(self.root / "s1")})
        self.assertEqual(self.client.get("/api/plugins/device/bindings").json(), {"bindings": []})

    def test_bound_to_other_device(self):
        self.online()
        self.hub.bind("s1", "d_8f2c", str(self.root))
        other = self.signer.sign(device_id="d_lap", device_name="laptop")
        ws = self.connect(header=other)
        ws.receive_json()
        Device(ws, str(self.root)).start()
        for sid in ("s1", "c1"):
            r = self.client.put(f"/api/plugins/device/sessions/{sid}/binding", json={"device_id": "d_lap",
                                                                                   "dir": str(self.root)})
            self.assertEqual((r.status_code, r.json()["error"]), (409, "bound_to_other_device"))
        self.assertIsNone(self.hub.binding("c1"))

    def test_dirs_route(self):
        r = self.client.get("/api/plugins/device/devices/d_8f2c/dirs")
        self.assertEqual((r.status_code, r.json()["error"]), (409, "device_offline"))
        self.online()
        r = self.client.get("/api/plugins/device/devices/d_8f2c/dirs")
        self.assertEqual(r.json(), {"path": str(self.root), "entries": [{"name": ".hidden"}, {"name": "sub"}]})
        r = self.client.get("/api/plugins/device/devices/d_8f2c/dirs", params={"path": "rel"})
        self.assertEqual((r.status_code, r.json()["error"]), (400, "bad_dir"))
        r = self.client.get("/api/plugins/device/devices/d_8f2c/dirs", params={"path": str(self.outside)})
        self.assertEqual((r.status_code, r.json()["error"]), (422, "dir_refused"))


    # file uploads
    def upload(self, sid="s1", name="notes.txt", data=b"hello\n", **extra):
        body = {"name": name, "content_base64": base64.b64encode(data).decode(), **extra}
        return self.client.post(f"/api/plugins/device/sessions/{sid}/files", json=body)

    def test_upload_writes_into_the_session_folder(self):
        self.online()
        self.hub.bind("s1", "d_8f2c", str(self.root))
        folder = self.root / ".hermes" / "attachments"
        r = self.upload(sid="c1", data=b"\x00\xffbinary")
        self.assertEqual(r.status_code, 200, r.text)
        self.assertEqual(r.json(), {"path": str(folder / "notes.txt"), "name": "notes.txt", "device_id": "d_8f2c",
                                    "bytes": 8})
        self.assertEqual((folder / "notes.txt").read_bytes(), b"\x00\xffbinary")
        self.assertEqual((folder / "notes.txt").stat().st_mode & 0o777, 0o600)
        r = self.upload()
        self.assertEqual(r.json()["name"], "notes-2.txt")
        self.assertEqual((folder / "notes.txt").read_bytes(), b"\x00\xffbinary")
        for name, want in (("../../evil.sh", "evil.sh"), ("..", "attachment"), (".bashrc", "bashrc"),
                           ("a\nb\x00c.txt", "a_b_c.txt"), ("dir\\x.txt", "x.txt")):
            with self.subTest(name=name):
                r = self.upload(name=name)
                self.assertEqual(r.status_code, 200, r.text)
                self.assertEqual(r.json()["name"], want)
                self.assertEqual(Path(r.json()["path"]).parent, folder)
        self.assertEqual(sorted(p.name for p in self.outside.iterdir()), [])

    def test_upload_refuses_symlinks_out_of_the_session_folder(self):
        self.online()
        self.hub.bind("s1", "d_8f2c", str(self.root / "sub"))
        (self.root / "sub" / ".hermes").symlink_to(self.outside)
        r = self.upload()
        self.assertEqual((r.status_code, r.json()["error"]), (422, "write_refused"))
        self.assertEqual(list(self.outside.iterdir()), [])
        (self.root / "sub" / ".hermes").unlink()
        (self.root / "sub" / ".hermes").mkdir()
        (self.root / "sub" / ".hermes" / "attachments").symlink_to(self.outside)
        r = self.upload()
        self.assertEqual((r.status_code, r.json()["error"]), (422, "write_refused"))
        self.assertEqual(list(self.outside.iterdir()), [])

    def test_upload_checks_the_write_rules(self):
        self.online(home=str(self.root))
        self.hub.bind("s1", "d_8f2c", str(self.root))
        with mock.patch.object(core, "UPLOAD_DIR", (".ssh",)):
            r = self.upload(name="authorized_keys")
        self.assertEqual((r.status_code, r.json()["error"]), (422, "write_refused"))
        self.assertFalse((self.root / ".ssh" / "authorized_keys").exists())
        with mock.patch.dict(os.environ, {"HERMES_WRITE_SAFE_ROOT": str(self.outside)}):
            r = self.upload()
        self.assertEqual((r.status_code, r.json()["error"]), (422, "write_refused"))
        with mock.patch.dict(os.environ, {"HERMES_WRITE_SAFE_ROOT": str(self.root)}):
            self.assertEqual(self.upload().status_code, 200)

    def test_upload_refusals(self):
        r = self.upload()
        self.assertEqual((r.status_code, r.json()["error"]), (409, "runs_on_pod"))
        self.cfg.update(fallback="refuse")
        self.assertEqual(self.upload().json()["error"], "fail_closed")
        self.cfg.update(fallback="local")
        self.online()
        self.hub.bind("s1", "d_8f2c", str(self.root))
        self.hub.unbind("s1")
        self.assertEqual(self.upload().json()["error"], "fail_closed")
        for body in ([], {"name": "x"}, {"name": 1, "content_base64": ""}):
            with self.subTest(body=body):
                r = self.client.post("/api/plugins/device/sessions/s1/files", json=body)
                self.assertEqual((r.status_code, r.json()["error"]), (400, "bad_request"))
        r = self.client.post("/api/plugins/device/sessions/s1/files", json={"name": "x", "content_base64": "@@"})
        self.assertEqual((r.status_code, r.json()["error"]), (400, "bad_request"))
        with mock.patch.object(core, "MAX_UPLOAD_BYTES", 4):
            r = self.upload(sid="s2", data=b"12345")
            self.assertEqual((r.status_code, r.json()["error"]), (413, "too_large"))
            r = self.upload(sid="s2", data=b"1" * 64)
            self.assertEqual((r.status_code, r.json()["error"]), (413, "too_large"))

    def test_upload_reaches_the_sandbox_and_offline_devices_fail(self):
        self.cfg.update(fallback="device", fallback_device="d_8f2c", fallback_root=str(self.root / "work"))
        r = self.upload(sid="c1")
        self.assertEqual((r.status_code, r.json()["error"]), (409, "fail_closed"))
        _, device = self.online()
        r = self.upload(sid="c1")
        self.assertEqual(r.status_code, 200, r.text)
        self.assertEqual(r.json()["path"], str(self.root / "work" / "s1" / ".hermes" / "attachments" / "notes.txt"))
        self.hub.bind("s1", "d_8f2c", str(self.root))
        device.ws.close()
        deadline = time.monotonic() + 5
        while self.hub.device("d_8f2c") is not None and time.monotonic() < deadline:
            time.sleep(0.02)
        r = self.upload()
        self.assertEqual((r.status_code, r.json()["error"]), (409, "device_offline"))

    def test_upload_name(self):
        self.assertEqual(core.upload_name("x" * 200 + ".pdf"), "x" * 116 + ".pdf")
        self.assertEqual(core.upload_name("\u00e9" * 100), "\u00e9" * 60)
        self.assertEqual(core.upload_name(" . "), "attachment")

if __name__ == "__main__":
    unittest.main()

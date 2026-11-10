"""The device-browser tools against a real device client and a real, throwaway, headless Firefox.

internal/browser/firefoxtest starts Firefox, the broker and the device client, and runs this
module with HERMES_BROWSER_E2E_SOCKET set: a Unix socket that carries the exec leg as JSON lines,
the device's hello first. Skipped otherwise."""
from __future__ import annotations

import asyncio
import json
import os
import socket
import sys
import tempfile
import threading
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
from test_core import Store, core  # noqa: E402

browser = core._load_sibling("hermes_device_browser", "browser.py")
SOCK = os.environ.get("HERMES_BROWSER_E2E_SOCKET", "")
FORM = os.environ.get("HERMES_BROWSER_E2E_FORM_URL", "")
ROOT = os.environ.get("HERMES_BROWSER_E2E_ROOT", "")


@unittest.skipUnless(SOCK, "run by internal/browser/firefoxtest with HERMES_BROWSER_E2E=1")
class PluginEndToEnd(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(dir=os.environ.get("TMPDIR"))
        self.addCleanup(self.tmp.cleanup)
        store = Store()
        store.rows["e2e"] = None
        self.hub = core.Hub(Path(self.tmp.name) / "bindings.json", parent_lookup=store.lookup)
        self.cfg = {"backend_id": "alice", "placeholder": self.tmp.name}
        for p in (mock.patch.object(core, "hub", lambda: self.hub), mock.patch.object(core, "settings", lambda: self.cfg),
                  mock.patch.object(core, "current_session_id", lambda: "e2e"),
                  mock.patch.object(core, "session_cwd", lambda: "")):
            p.start()
            self.addCleanup(p.stop)
        self.sock = socket.socket(socket.AF_UNIX)
        self.sock.connect(SOCK)
        self.addCleanup(self.sock.close)
        self.reader = self.sock.makefile("rb")
        hello = json.loads(self.reader.readline())
        self.loop = asyncio.new_event_loop()
        threading.Thread(target=self.loop.run_forever, daemon=True).start()
        self.addCleanup(lambda: self.loop.call_soon_threadsafe(self.loop.stop))
        self.wlock = threading.Lock()

        async def send_text(text: str) -> None:
            with self.wlock:
                self.sock.sendall(text.encode() + b"\n")

        self.conn = core.DeviceConn(device_id="d_e2e", name="desktop", send_text=send_text, loop=self.loop,
                                    roots=hello["roots"], tmp_dir=hello["tmp_dir"], home=hello["home"], version=3,
                                    capabilities=frozenset({core.CAPABILITY_BROWSER}))
        self.hub.attach(self.conn)
        threading.Thread(target=self.read, daemon=True).start()
        self.registry = self.register()

    def read(self):
        for line in self.reader:
            msg = json.loads(line)
            if msg.get("type") == core.TYPE_PING:
                with self.wlock:
                    self.sock.sendall(json.dumps({"type": core.TYPE_PONG, "seq": msg["seq"]}).encode() + b"\n")
            else:
                self.hub.on_message(self.conn, msg)

    def register(self):
        from tools.registry import ToolRegistry
        reg = ToolRegistry()

        class Ctx:
            def register_tool(self, name, toolset, schema, handler, check_fn=None, description="", emoji=""):
                reg.register(name=name, toolset=toolset, schema=schema, handler=handler, check_fn=check_fn,
                             description=description, emoji=emoji)

            def register_hook(self, *_):
                pass

        browser.register(Ctx(), core)
        return reg

    def tool(self, name: str, expect: object = "", **args):
        """pre_tool_call then the handler; an approve counts as the user's yes. expect is the
        rule_key the call must ask for, None for no ask, "" to not check."""
        decision = browser.pre_tool_call(tool_name=name, args=args, session_id="e2e")
        if expect != "":
            self.assertEqual(decision["rule_key"] if decision else None, expect, f"{name} {args}")
        out = self.registry.dispatch(name, args, task_id="t", session_id="e2e")
        out = out if isinstance(out, dict) else json.loads(out)
        print(f"{name} {json.dumps(args)} -> {json.dumps(out)[:300]}")
        return out

    def ref(self, snapshot: str, role: str) -> str:
        line = [ln for ln in snapshot.splitlines() if ln.strip().startswith(f"- {role}")][-1]
        return line.rsplit("[ref=", 1)[1].rstrip("]")

    def test_tools_drive_firefox(self):
        deadline = threading.Event()
        for _ in range(100):
            if self.conn.browser.get("connected"):
                break
            deadline.wait(0.1)
        self.assertTrue(self.conn.browser.get("connected"), "no browser_status from the device")
        self.hub.bind("e2e", "d_e2e", ROOT)

        status = self.tool("browser_status")
        self.assertTrue(status["available"])
        self.assertTrue(status["browser"].startswith("Firefox"))

        page = self.tool("browser_navigate", "browser:visit:example.com", url="https://example.com/")
        self.assertEqual(page["title"], "Example Domain")
        tab = page["tab_id"]
        snap = self.tool("browser_snapshot", tab_id=tab, full_text=True)
        self.assertIn('- link "Learn more"', snap["snapshot"])
        self.assertIn("documentation examples", snap["text"])

        with mock.patch.object(browser, "_native_vision", lambda: True):
            shot = self.tool("browser_vision", tab_id=tab, question="What is the heading?")
        self.assertTrue(shot["_multimodal"])
        self.assertTrue(shot["content"][1]["image_url"]["url"].startswith("data:image/jpeg;base64,"))

        link = self.ref(snap["snapshot"], "link")
        moved = self.tool("browser_click", "browser:visit:iana.org", tab_id=tab, ref=link)
        self.assertNotEqual(moved["url"], "https://example.com/")
        self.assertEqual(self.tool("browser_back", tab_id=tab)["url"], "https://example.com/")

        self.tool("browser_navigate", "browser:visit:127.0.0.1", tab_id=tab, url=FORM + "/")
        snap = self.tool("browser_snapshot", tab_id=tab, interactive_only=True)
        self.tool("browser_type", None, tab_id=tab, ref=self.ref(snap["snapshot"], "textbox"), text="from the plugin")
        button = self.ref(snap["snapshot"], "button")
        unapproved = json.loads(self.registry.dispatch("browser_click", {"tab_id": tab, "ref": button},
                                                       task_id="t", session_id="e2e"))
        self.assertEqual(unapproved["code"], "refused", "the extension must refuse a submit the user did not approve")
        self.tool("browser_click", "browser:submit:127.0.0.1", tab_id=tab, ref=button)
        self.assertIn("got: from the plugin", self.tool("browser_snapshot", tab_id=tab, full_text=True)["text"])

        self.tool("browser_navigate", None, tab_id=tab, url=FORM + "/login")
        snap = self.tool("browser_snapshot", tab_id=tab, interactive_only=True)
        field = self.ref(snap["snapshot"], "textbox")
        with mock.patch.object(browser, "_confirm", return_value=False) as asked:
            self.assertEqual(self.tool("browser_type", None, tab_id=tab, ref=field, text="alice")["code"], "declined")
        self.assertEqual(asked.call_count, 1)
        with mock.patch.object(browser, "_confirm", return_value=True) as asked:
            self.assertNotIn("error", self.tool("browser_type", None, tab_id=tab, ref=field, text="alice"))
            self.assertNotIn("error", self.tool("browser_type", None, tab_id=tab, ref=field, text="bob"))
        self.assertEqual(asked.call_count, 2)
        self.assertIn("one-time-code, password", asked.call_args.args[0])

        self.assertIn("closed", self.tool("browser_tabs", action="close", tab_id=tab))


if __name__ == "__main__":
    unittest.main()

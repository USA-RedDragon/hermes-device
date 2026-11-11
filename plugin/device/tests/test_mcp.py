"""device-mcp toolset tests: the exec leg MCP messages, capability negotiation, per-session
gating, approvals and result rendering, with a fake device. Run inside the Hermes runtime with
scripts/test-plugin.sh."""
from __future__ import annotations

import json
import sys
import threading
import time
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import test_browser  # noqa: E402
from test_browser import Recorder  # noqa: E402
from test_contract import require_source  # noqa: E402
from test_core import Base, FakeDevice, core  # noqa: E402

mcptools = core._load_sibling("hermes_device_mcp", "mcptools.py")

TOOLS = [
    {"name": "list_strings", "description": "List strings in the open binary", "annotations": {"readOnlyHint": True},
     "inputSchema": {"type": "object", "properties": {"limit": {"type": "integer"}}}},
    {"name": "set_comment", "description": "Set a comment at an address",
     "inputSchema": {"type": "object", "properties": {"address": {"type": "string"}}}},
    {"name": "open_binary", "description": "Open a binary for analysis " + "x" * 400,
     "inputSchema": {"type": "object", "properties": {"path": {"type": "string"}}}},
]


class FakeMCP:
    """The MCP servers behind a device: answers mcp requests."""

    def __init__(self, servers=("rea",)):
        self.servers = servers
        self.requests: list[dict] = []
        self.results: dict[str, dict] = {}
        self.hold: set[str] = set()

    def answer(self, msg: dict) -> dict | None:
        self.requests.append(msg)
        op = msg["op"]
        if op == core.MCP_OP_LIST:
            return {"ok": True, "result": {"servers": [{"name": s, "running": False} for s in self.servers]}}
        if msg.get("server") not in self.servers:
            return {"ok": False, "code": core.MCP_UNKNOWN_SERVER, "error": "no such server"}
        if op == core.MCP_OP_TOOLS:
            return {"ok": True, "result": {"tools": TOOLS}}
        if msg["tool"] in self.hold:
            return None
        if msg["tool"] in self.results:
            return {"ok": True, "result": self.results[msg["tool"]]}
        return {"ok": True, "result": {"content": [{"type": "text", "text": json.dumps(msg["arguments"])}]}}


def attach_mcp(device, fake: FakeMCP) -> None:
    conn = device.conn
    conn.capabilities = frozenset({core.CAPABILITY_MCP})
    conn.mcp_servers = tuple(fake.servers)
    inner = conn.send_text

    async def send_text(text: str) -> None:
        msg = json.loads(text)
        if msg["type"] != core.TYPE_MCP:
            await inner(text)
            return
        reply = fake.answer(msg)
        if reply is not None:
            reply.update(type=core.TYPE_MCP_RESULT, id=msg["id"])
            threading.Thread(target=device.hub.on_message, args=(conn, reply), daemon=True).start()

    conn.send_text = send_text


class ToolTests(Base):
    def setUp(self):
        super().setUp()
        mcptools._TOOLS.clear()
        mcptools._CHECKED.clear()
        self.fake = FakeMCP()
        attach_mcp(self.device, self.fake)
        self.bind()
        self.registry = self.registered()

    def registered(self):
        from tools.registry import ToolRegistry
        reg = ToolRegistry()
        hooks = []

        class Ctx:
            def register_tool(self, name, toolset, schema, handler, check_fn=None, description="", emoji=""):
                reg.register(name=name, toolset=toolset, schema=schema, handler=handler, check_fn=check_fn,
                             description=description, emoji=emoji)

            def register_hook(self, name, cb):
                hooks.append((name, cb))

        mcptools.register(Ctx(), core)
        self.assertEqual(hooks, [("pre_tool_call", mcptools.pre_tool_call)])
        return reg

    def run_tool(self, name: str, approve: bool = True, **args):
        decision = mcptools.pre_tool_call(tool_name=name, args=args, task_id="t", session_id=self.session)
        self.decision = decision
        if decision and decision.get("action") == "block":
            return {"error": decision["message"], "blocked": True}
        if decision and not approve:
            return {"error": "denied"}
        out = self.registry.dispatch(name, args, task_id="t", session_id=self.session)
        return out if isinstance(out, dict) else json.loads(out)

    def calls(self):
        return [r for r in self.fake.requests if r["op"] == core.MCP_OP_CALL]

    def test_registers_toolset(self):
        names = {"mcp_device_servers", "mcp_device_tools", "mcp_device_call"}
        self.assertEqual(set(mcptools.SCHEMAS), names)
        self.assertEqual(self.registry.get_toolset_for_tool("mcp_device_call"), "device-mcp")
        defs = {d["function"]["name"] if "function" in d else d["name"]
                for d in self.registry.get_definitions(names, quiet=True)}
        self.assertEqual(defs, names)

    def test_check_fn(self):
        self.assertTrue(mcptools.offered())
        self.cfg["mcp"] = False
        self.assertFalse(mcptools.offered())
        out = self.run_tool("mcp_device_servers")
        self.assertIn("turned off", out["error"])
        self.cfg["mcp"] = True
        self.device.conn.capabilities = frozenset()
        self.assertFalse(mcptools.offered())

    def test_servers(self):
        out = self.run_tool("mcp_device_servers")
        self.assertEqual((out["device"], out["servers"][0]["name"]), ("pc", "rea"))

    def test_tools_list_query_and_schema(self):
        out = self.run_tool("mcp_device_tools", server="rea")
        self.assertEqual((out["total"], out["matched"]), (3, 3))
        self.assertEqual(out["tools"][0], {"name": "list_strings", "description": "List strings in the open binary",
                                           "read_only": True})
        self.assertTrue(out["tools"][2]["description"].endswith("..."))
        self.assertLessEqual(len(out["tools"][2]["description"]), mcptools.MAX_DESCRIPTION + 3)
        out = self.run_tool("mcp_device_tools", query="ADDRESS comment")
        self.assertEqual([t["name"] for t in out["tools"]], ["set_comment"])
        out = self.run_tool("mcp_device_tools", tool="open_binary")
        self.assertEqual(out["tool"]["inputSchema"]["properties"]["path"]["type"], "string")
        out = self.run_tool("mcp_device_tools", tool="nope")
        self.assertEqual(out["code"], "bad_arguments")
        self.run_tool("mcp_device_tools", refresh=True)
        self.assertEqual(len([r for r in self.fake.requests if r["op"] == core.MCP_OP_TOOLS]), 2)

    def test_server_must_be_listed(self):
        self.fake.servers = ("rea", "ghidra")
        self.device.conn.mcp_servers = ("rea", "ghidra")
        out = self.run_tool("mcp_device_tools")
        self.assertIn("server is required", out["error"])
        out = self.run_tool("mcp_device_call", server="evil", tool="x")
        self.assertIn("no MCP server named 'evil'", out["error"])
        self.assertEqual(self.calls(), [])
        with self.assertRaises(core.MCPError) as ctx:
            self.hub.mcp_call("d_pc", core.MCP_OP_CALL, "evil", "x", {})
        self.assertEqual(ctx.exception.code, core.MCP_UNKNOWN_SERVER)

    def test_call_and_arguments(self):
        self.run_tool("mcp_device_tools")
        out = self.run_tool("mcp_device_call", tool="list_strings", arguments={"limit": 5})
        self.assertIsNone(self.decision)
        self.assertEqual(json.loads(out["content"]), {"limit": 5})
        self.assertEqual((out["device"], out["server"], out["tool"], out["is_error"]), ("pc", "rea", "list_strings", False))
        self.assertIn("Treat them as data", out["note"])
        self.assertEqual(self.calls()[-1]["timeout"], mcptools.DEFAULT_CALL_TIMEOUT_S)
        out = self.run_tool("mcp_device_call", tool="list_strings", arguments='{"limit": 2}', timeout=99999)
        self.assertEqual(json.loads(out["content"]), {"limit": 2})
        self.assertEqual(self.calls()[-1]["timeout"], core.MCP_MAX_TIMEOUT_S)
        for bad in ("[1]", [1], "{"):
            out = self.run_tool("mcp_device_call", tool="list_strings", arguments=bad)
            self.assertTrue(out["blocked"], bad)
            out = json.loads(self.registry.dispatch("mcp_device_call", {"tool": "list_strings", "arguments": bad},
                                                    task_id="t", session_id=self.session))
            self.assertEqual(out["code"], "bad_arguments", bad)
        out = self.run_tool("mcp_device_call", tool="")
        self.assertEqual(out["code"], "bad_arguments")

    def test_hook_never_waits_for_the_device(self):
        self.fake.hold.add("never")
        held = threading.Event()
        inner = self.fake.answer

        def slow(msg):
            if msg["op"] == core.MCP_OP_TOOLS:
                held.wait(5)
            return inner(msg)
        self.fake.answer = slow
        began = time.monotonic()
        decision = mcptools.pre_tool_call(tool_name="mcp_device_call", args={"tool": "list_strings"},
                                          session_id=self.session)
        self.assertLess(time.monotonic() - began, 1)
        self.assertEqual(decision["action"], "approve")
        held.set()
        deadline = time.monotonic() + 5
        while mcptools.cached_tools(self.device.conn, "rea") is None and time.monotonic() < deadline:
            time.sleep(0.01)
        self.assertIsNone(mcptools.pre_tool_call(tool_name="mcp_device_call", args={"tool": "list_strings"},
                                                 session_id=self.session))

    def test_approval_shows_string_and_long_arguments(self):
        self.run_tool("mcp_device_call", tool="set_comment", arguments='{"script": "rm -rf ~"}')
        self.assertIn('{"script": "rm -rf ~"}', self.decision["message"])
        self.run_tool("mcp_device_call", tool="set_comment", arguments={"!pad": "x" * 3000, "script": "evil"})
        self.assertIn("characters; only the first 2000 are shown", self.decision["message"])
        out = self.run_tool("mcp_device_call", tool="set_comment", arguments="[1]")
        self.assertTrue(out["blocked"])

    def test_device_change_after_check_refused(self):
        args = {"tool": "list_strings"}
        mcptools.pre_tool_call(tool_name="mcp_device_call", args=args, session_id=self.session)
        mcptools._put_checked(mcptools._checked_key(self.session, args), "d_other")
        out = json.loads(self.registry.dispatch("mcp_device_call", args, task_id="t", session_id=self.session))
        self.assertIn("device changed", out["error"])
        self.assertEqual(self.calls(), [])

    def test_approval_for_tools_not_read_only(self):
        self.run_tool("mcp_device_tools")
        self.run_tool("mcp_device_call", tool="set_comment", arguments={"address": "0x10"})
        self.assertEqual(self.decision["action"], "approve")
        self.assertEqual(self.decision["rule_key"], "device-mcp:d_pc:rea:set_comment")
        self.assertIn('{"address": "0x10"}', self.decision["message"])
        self.assertIn("device pc", self.decision["message"])
        out = self.run_tool("mcp_device_call", approve=False, tool="unknown_tool")
        self.assertEqual(self.decision["rule_key"], "device-mcp:d_pc:rea:unknown_tool")
        self.assertEqual(out, {"error": "denied"})
        self.cfg["mcp_approval"] = "always"
        self.run_tool("mcp_device_call", tool="list_strings")
        self.assertEqual(self.decision["action"], "approve")
        self.cfg["mcp_approval"] = "write"
        self.run_tool("mcp_device_call", tool="list_strings")
        self.assertIsNone(self.decision)
        self.cfg["mcp_approval"] = "always"
        self.run_tool("mcp_device_call", tool="list_strings")
        self.assertEqual(self.decision["action"], "approve")
        self.cfg["mcp_approval"] = "never"
        self.run_tool("mcp_device_call", tool="set_comment")
        self.assertIsNone(self.decision)
        self.cfg["mcp_approval"] = "bogus"
        self.run_tool("mcp_device_call", tool="set_comment")
        self.assertEqual(self.decision["action"], "approve")

    def test_approval_when_tool_list_fails(self):
        self.fake.servers = ()
        mcptools._TOOLS.clear()
        self.run_tool("mcp_device_call", tool="list_strings")
        self.assertEqual(self.decision["action"], "approve")

    def test_unbound_session_refused(self):
        self.hub.unbind(self.session)
        self.store.rows["fresh"] = None
        self.session = "fresh"
        for name, args in (("mcp_device_servers", {}), ("mcp_device_tools", {}),
                           ("mcp_device_call", {"tool": "list_strings"})):
            out = self.run_tool(name, **args)
            self.assertIn("not bound to a device", out["error"])
            if name == "mcp_device_call":
                self.assertEqual(self.decision["action"], "block")
            out = json.loads(self.registry.dispatch(name, args, task_id="t", session_id=self.session))
            self.assertIn("not bound to a device", out["error"])
        self.assertEqual(self.fake.requests, [])

    def test_sandbox_refused(self):
        res = core.Resolution(core.STATE_BOUND, self.session, binding=self.hub.binding(self.session), sandbox=True)
        with mock.patch.object(core, "resolve_session", lambda *a, **k: res):
            out = self.run_tool("mcp_device_call", tool="list_strings")
        self.assertIn("sandbox device", out["error"])
        self.assertEqual(self.fake.requests, [])

    def test_other_device_unreachable(self):
        """A session bound to device d_pc never reaches d_other's servers, whatever it names."""
        other = FakeDevice(self.hub, str(self.root), str(self.devtmp), str(self.home), device_id="d_other", name="other")
        self.addCleanup(other.close)
        other_fake = FakeMCP(servers=("secret",))
        attach_mcp(other, other_fake)
        out = self.run_tool("mcp_device_call", server="secret", tool="x", device="d_other")
        self.assertIn("no MCP server named 'secret'", out["error"])
        self.assertEqual(other_fake.requests, [])
        self.run_tool("mcp_device_call", tool="list_strings")
        self.assertEqual(other_fake.requests, [])
        self.assertEqual(len(self.calls()), 1)

    def test_device_without_capability(self):
        self.device.conn.capabilities = frozenset()
        out = self.run_tool("mcp_device_servers")
        self.assertIn("mcp.servers", out["error"])
        self.assertEqual(self.fake.requests, [])

    def test_device_error_reported(self):
        self.fake.servers = ()
        out = self.run_tool("mcp_device_call", approve=True, tool="x")
        self.assertEqual(out["code"], core.MCP_UNKNOWN_SERVER)

    def test_tool_error_and_rich_content(self):
        self.fake.results["set_comment"] = {"isError": True, "content": [
            {"type": "text", "text": "bad address"},
            {"type": "resource_link", "uri": "file:///x", "name": "x"},
            {"type": "resource", "resource": {"uri": "file:///y", "text": "body"}},
            {"type": "resource", "resource": {"uri": "file:///z", "blob": "AAAA"}},
            {"type": "audio", "data": "AAAA"},
            {"type": "image", "data": "AAAA", "mimeType": "image/tiff"}],
            "structuredContent": {"ok": False}}
        out = self.run_tool("mcp_device_call", tool="set_comment")
        self.assertTrue(out["is_error"])
        for part in ("bad address", "[resource x: file:///x]", "[resource file:///y]\nbody",
                     "[binary resource file:///z not shown]", "[audio content not shown]", "image/tiff"):
            self.assertIn(part, out["content"])
        self.assertEqual(out["structured_content"], {"ok": False})

    def test_images(self):
        self.fake.results["shot"] = {"content": [{"type": "text", "text": "a screen"},
                                                 {"type": "image", "data": "iVBORw0K", "mimeType": "image/png"}]}
        with mock.patch.object(mcptools, "_native_vision", lambda: True):
            out = self.registry.dispatch("mcp_device_call", {"tool": "shot"}, task_id="t", session_id=self.session)
        self.assertTrue(out["_multimodal"])
        self.assertEqual(out["content"][1]["image_url"]["url"], "data:image/png;base64,iVBORw0K")
        self.assertIn("a screen", out["text_summary"])
        with mock.patch.object(mcptools, "_native_vision", lambda: False):
            out = json.loads(self.registry.dispatch("mcp_device_call", {"tool": "shot"}, task_id="t",
                                                    session_id=self.session))
        self.assertIn("cannot see images", out["images"])
        self.fake.results["bad"] = {"content": [{"type": "image", "data": "not base64!", "mimeType": "image/png"},
                                                {"type": "image", "data": "A" * (8 << 20), "mimeType": "image/png"}]}
        with mock.patch.object(mcptools, "_native_vision", lambda: True):
            out = json.loads(self.registry.dispatch("mcp_device_call", {"tool": "bad"}, task_id="t",
                                                    session_id=self.session))
        self.assertEqual(out["content"].count("not shown"), 2)

    def test_long_text_cut(self):
        self.fake.results["big"] = {"content": [{"type": "text", "text": "a" * (mcptools.MAX_TEXT + 10)}]}
        out = self.run_tool("mcp_device_call", tool="big")
        self.assertIn("characters cut", out["content"])
        self.assertLess(len(out["content"]), mcptools.MAX_TEXT + 100)

    def test_timeout_kills_request(self):
        self.fake.hold.add("slow")
        with mock.patch.object(core, "BROWSER_SLACK_S", 0.1):
            began = time.monotonic()
            out = self.run_tool("mcp_device_call", tool="slow", timeout=1)
        self.assertEqual(out["code"], core.MCP_TIMEOUT)
        self.assertLess(time.monotonic() - began, 5)
        self.assertEqual(self.device.conn.mcp_calls, {})

    def test_interrupt_cancels_request(self):
        self.fake.hold.add("slow")
        sent = []
        inner = self.device.conn.send_text

        async def record(text):
            sent.append(json.loads(text))
            await inner(text)
        self.device.conn.send_text = record
        with mock.patch.object(mcptools, "_interrupted", lambda: True):
            out = self.run_tool("mcp_device_call", tool="slow")
        self.assertEqual(out["code"], core.MCP_CANCELLED)
        self.assertEqual(sent[-1]["type"], core.TYPE_KILL)
        self.assertEqual(sent[-1]["id"], sent[-2]["id"])


class HubMCPTests(unittest.TestCase):
    def setUp(self):
        import os
        import tempfile
        from test_core import Store
        self.tmp = tempfile.TemporaryDirectory(dir=os.environ.get("TMPDIR"))
        self.addCleanup(self.tmp.cleanup)
        self.hub = core.Hub(Path(self.tmp.name) / "bindings.json", parent_lookup=Store().lookup)
        for p in (mock.patch.object(core, "settings", lambda: {"backend_id": "alice"}),
                  mock.patch.object(core, "hub", lambda: self.hub)):
            p.start()
            self.addCleanup(p.stop)
        self.dev = Recorder()
        self.addCleanup(self.dev.stop)
        self.dev.conn.capabilities = frozenset({core.CAPABILITY_MCP})
        self.dev.conn.mcp_servers = ("rea",)
        self.hub.attach(self.dev.conn)

    def call_async(self):
        box = {}

        def run():
            try:
                box["result"] = self.hub.mcp_call("d_pc", core.MCP_OP_CALL, "rea", "t", {"a": 1}, timeout=30)
            except core.MCPError as exc:
                box["error"] = exc
        before = len(self.dev.sent)
        t = threading.Thread(target=run, daemon=True)
        t.start()
        deadline = time.monotonic() + 5
        while len(self.dev.sent) == before and time.monotonic() < deadline:
            time.sleep(0.01)
        return t, box

    def test_result_error_and_forgery(self):
        t, box = self.call_async()
        msg = self.dev.sent[-1]
        self.assertEqual({k: msg[k] for k in ("type", "op", "server", "tool", "arguments", "timeout")},
                         {"type": "mcp", "op": "call", "server": "rea", "tool": "t", "arguments": {"a": 1}, "timeout": 30})
        self.hub.on_message(self.dev.conn, {"type": "mcp_result", "id": "forged", "ok": True, "result": 1})
        self.hub.on_message(self.dev.conn, {"type": "mcp_result", "id": msg["id"], "ok": True, "result": {"x": 1}})
        t.join(5)
        self.assertEqual(box["result"], {"x": 1})
        t, box = self.call_async()
        self.hub.on_message(self.dev.conn, {"type": "mcp_result", "id": self.dev.sent[-1]["id"], "ok": False,
                                            "code": "busy", "error": "too many"})
        t.join(5)
        self.assertEqual((box["error"].code, str(box["error"])), ("busy", "too many"))

    def test_without_capability_ignored_and_refused(self):
        t, box = self.call_async()
        self.dev.conn.capabilities = frozenset()
        self.hub.on_message(self.dev.conn, {"type": "mcp_result", "id": self.dev.sent[-1]["id"], "ok": True})
        self.assertNotIn("result", box)
        self.hub.detach(self.dev.conn, resumable=True)
        t.join(5)
        self.assertEqual(box["error"].code, core.MCP_NOT_CONNECTED)
        with self.assertRaises(core.MCPError):
            self.hub.mcp_call("d_pc", core.MCP_OP_LIST)

    def test_request_size_limit(self):
        with self.assertRaises(core.MCPError) as ctx:
            self.hub.mcp_call("d_pc", core.MCP_OP_CALL, "rea", "t", {"a": "x" * core.MAX_MCP_REQUEST})
        self.assertEqual(ctx.exception.code, core.MCP_REFUSED)
        self.assertEqual(self.dev.sent, [])

    def test_info_lists_servers(self):
        self.assertEqual(self.dev.conn.info()["mcp_servers"], ["rea"])


class MCPNegotiationTests(test_browser.NegotiationTests):
    def test_mcp_negotiated_with_servers(self):
        ws, welcome = self.connect(capabilities=["mcp"], mcp_servers=["rea", "Bad Name", "rea", 3, "ghidra"])
        self.assertEqual(welcome["capabilities"], ["mcp"])
        self.wait(lambda: self.hub.device("d_8f2c") is not None)
        conn = self.hub.device("d_8f2c")
        self.assertEqual(conn.mcp_servers, ("rea", "ghidra"))
        box = {}
        t = threading.Thread(target=lambda: box.update(r=self.hub.mcp_call("d_8f2c", core.MCP_OP_LIST, timeout=10)))
        t.start()
        msg = ws.receive_json()
        while msg["type"] != "mcp":
            msg = ws.receive_json()
        ws.send_json({"type": "mcp_result", "id": msg["id"], "ok": True, "result": {"servers": []}})
        t.join(5)
        self.assertEqual(box["r"], {"servers": []})

    def test_mcp_without_servers_not_accepted(self):
        _, welcome = self.connect(capabilities=["mcp"], mcp_servers=["Bad Name"])
        self.assertNotIn("capabilities", welcome)
        self.wait(lambda: self.hub.device("d_8f2c") is not None)
        self.assertEqual(self.hub.device("d_8f2c").capabilities, frozenset())


class HermesMCPContract(unittest.TestCase):
    """The Hermes internals the toolset depends on."""

    @classmethod
    def setUpClass(cls):
        require_source()

    def test_untrusted_wrapper_covers_the_toolset(self):
        from agent.tool_dispatch_helpers import _is_untrusted_tool
        for name in mcptools.SCHEMAS:
            self.assertTrue(_is_untrusted_tool(name), name)

    def test_interrupt_helper(self):
        from tools.interrupt import is_interrupted
        self.assertFalse(is_interrupted())

    def test_no_core_tool_collision(self):
        import toolsets
        core_tools = {t for ts in toolsets.TOOLSETS.values() for t in ts.get("tools", [])}
        self.assertFalse(core_tools & set(mcptools.SCHEMAS))


if __name__ == "__main__":
    unittest.main()

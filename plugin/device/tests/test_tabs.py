"""Automatic cleanup of the agent's tabs: one test per rule of docs/browser.md section 3.4. Run
inside the Hermes runtime with scripts/test-plugin.sh."""
from __future__ import annotations

import json
import sys
import threading
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
from test_browser import BrowserBase, FakeBrowser, browser, core  # noqa: E402

tabs = core._load_sibling("hermes_device_tabs", "tabs.py")


class Clock:
    def __init__(self):
        self.now = 1000.0

    def __call__(self):
        return self.now

    def advance(self, seconds: float):
        self.now += seconds


class TabBrowser(FakeBrowser):
    """Opens a new tab id on every tab_open, and really closes tabs. Tab 7 is the user's own tab
    (in the group, as the fake lists it, but never opened by an agent tool)."""

    def __init__(self):
        super().__init__()
        self.next_id = 10
        self.closed: list[int] = []

    def answer(self, msg):
        op, args = msg["op"], msg.get("args") or {}
        if op in self.fail or op in self.hold:
            return super().answer(msg)
        if op == "tab_open":
            self.requests.append(msg)
            tab = {"tab_id": self.next_id, "url": args["url"], "title": "Opened", "active": False}
            self.tabs[self.next_id] = tab
            self.next_id += 1
            return {"ok": True, "result": tab}
        if op == "tab_close":
            self.requests.append(msg)
            if args["tab_id"] not in self.tabs:
                return {"ok": False, "code": core.BROWSER_REFUSED, "error": f"tab {args['tab_id']} is not in the Hermes tab group"}
            del self.tabs[args["tab_id"]]
            self.closed.append(args["tab_id"])
            return {"ok": True, "result": {"closed": args["tab_id"]}}
        if op == "navigate":
            self.requests.append(msg)
            if args["tab_id"] not in self.tabs:
                return {"ok": False, "code": core.BROWSER_REFUSED, "error": f"tab {args['tab_id']} is not in the Hermes tab group"}
            self.tabs[args["tab_id"]]["url"] = args["url"]
            return {"ok": True, "result": dict(self.tabs[args["tab_id"]])}
        return super().answer(msg)


class TabBase(BrowserBase):
    def setUp(self):
        super().setUp()
        self.fake = TabBrowser()
        from test_browser import attach_browser
        attach_browser(self.device, self.fake)
        self.clock = Clock()
        self.led = tabs.Ledger(self.base / "state" / "tabs.json", clock=self.clock,
                               limits=lambda: (120.0, 1800.0, 86400.0))
        p = mock.patch.object(browser, "_LEDGER", self.led)
        p.start()
        self.addCleanup(p.stop)

    def sweep(self) -> int:
        return browser.sweep_tabs(self.led)

    def turn(self, session=None, turn="t1"):
        browser.on_turn_start(session_id=session or self.session, turn_id=turn)

    def end(self, session=None):
        browser.on_turn_end(session_id=session or self.session)

    def open(self, url="https://example.com/", **args):
        return self.run_tool("browser_navigate", url=url, **args)["tab_id"]


class RecordTests(TabBase):
    def test_a_tab_records_the_chat_and_the_turn(self):
        self.turn(turn="turn-9")
        tab = self.run_tool("browser_tabs", action="open", url="https://example.com/")["tab_id"]
        rec = self.led.get("d_pc", tab)
        self.assertEqual((rec["session"], rec["turn"], rec["keep"]), (self.session, "turn-9", False))

    def test_user_tab_is_never_recorded_and_never_closed(self):
        self.turn()
        self.run_tool("browser_snapshot", tab_id=7)
        self.run_tool("browser_click", tab_id=7, ref="e1")
        self.assertIsNone(self.led.get("d_pc", 7))
        self.end()
        self.clock.advance(100000)
        self.sweep()
        self.assertNotIn(7, self.fake.closed)
        self.assertIn(7, self.fake.tabs)


class TurnEndTests(TabBase):
    def test_b_closes_after_the_grace_of_the_turn_end(self):
        self.turn()
        a = self.open()
        self.assertEqual(self.sweep(), 0)
        self.end()
        self.clock.advance(119)
        self.assertEqual(self.sweep(), 0)
        self.clock.advance(2)
        self.assertEqual(self.sweep(), 1)
        self.assertEqual(self.fake.closed, [a])
        self.assertNotIn(a, self.fake.tabs)
        self.assertIsNone(self.led.get("d_pc", a))

    def test_tab_stays_while_the_turn_runs(self):
        self.turn()
        self.open()
        self.clock.advance(600)
        self.assertEqual(self.sweep(), 0)

    def test_any_use_resets_the_grace(self):
        self.turn()
        a = self.open()
        self.end()
        self.clock.advance(100)
        self.run_tool("browser_snapshot", tab_id=a)
        self.clock.advance(100)
        self.assertEqual(self.sweep(), 0)
        self.clock.advance(21)
        self.assertEqual(self.sweep(), 1)

    def test_next_turn_holds_the_tab_again(self):
        self.turn()
        a = self.open()
        self.end()
        self.clock.advance(60)
        self.turn(turn="t2")
        self.clock.advance(600)
        self.assertEqual(self.sweep(), 0)
        self.assertIn(a, self.fake.tabs)

    def test_idle_chat_loses_every_unkept_tab_even_when_the_turn_never_ended(self):
        self.turn()
        a = self.open()
        self.clock.advance(1801)
        self.assertEqual(self.sweep(), 1)
        self.assertNotIn(a, self.fake.tabs)

    def test_explicit_close_forgets_the_record(self):
        a = self.open()
        self.run_tool("browser_tabs", action="close", tab_id=a)
        self.assertIsNone(self.led.get("d_pc", a))

    def test_other_chats_tabs_are_not_touched(self):
        self.turn()
        a = self.open()
        self.store.rows["other"] = None
        self.session = "other"
        self.bind("other")
        self.turn("other")
        b = self.open()
        self.end("other")
        self.clock.advance(200)
        self.assertEqual(self.sweep(), 1)
        self.assertEqual(self.fake.closed, [b])
        self.assertIn(a, self.fake.tabs)


    def test_a_chat_the_hooks_never_named_keeps_its_tabs_until_idle(self):
        a = self.open()
        self.clock.advance(1000)
        self.assertEqual(self.sweep(), 0)
        self.assertIn(a, self.fake.tabs)
        self.clock.advance(801)
        self.assertEqual(self.sweep(), 1)


class HookTests(TabBase):
    def test_turn_hooks_are_registered_where_the_hermes_has_them(self):
        hooks = []

        class Ctx:
            def register_tool(self, *a, **k):
                pass

            def register_hook(self, name, cb):
                hooks.append(name)

        with mock.patch.object(core, "valid_hooks", lambda: frozenset(
                {"agent_turn_start", "agent_turn_end", "pre_llm_call", "subagent_stop"})):
            browser.register(Ctx(), core)
        self.assertEqual([h for h in hooks if h not in ("pre_tool_call", "on_session_delete")],
                         ["agent_turn_start", "agent_turn_end", "pre_llm_call", "subagent_stop"])


class KeepTests(TabBase):
    def test_c_keep_flag_on_navigate_and_open(self):
        self.turn()
        a = self.open(keep=True)
        b = self.run_tool("browser_tabs", action="open", url="https://example.net/", keep=True)["tab_id"]
        self.end()
        self.clock.advance(5000)
        self.assertEqual(self.sweep(), 0)
        self.assertEqual({a, b} - set(self.fake.tabs), set())
        self.assertEqual(self.led.get("d_pc", a)["reason"], "requested")

    def test_keep_and_release_actions(self):
        self.turn()
        a = self.open()
        out = self.run_tool("browser_tabs", action="keep", tab_id=a)
        self.assertEqual(out, {"tab_id": a, "keep": True})
        self.end()
        self.clock.advance(5000)
        self.assertEqual(self.sweep(), 0)
        self.assertTrue([t for t in self.run_tool("browser_tabs")["tabs"] if t["tab_id"] == a][0]["keep"])
        self.run_tool("browser_tabs", action="release", tab_id=a)
        self.clock.advance(121)
        self.assertEqual(self.sweep(), 1)

    def test_keep_refuses_a_tab_this_chat_did_not_open(self):
        out = self.run_tool("browser_tabs", action="keep", tab_id=7)
        self.assertIn("not opened by this chat", out["error"])

    def test_screenshot_keeps_the_tab(self):
        self.turn()
        a = self.open()
        self.run_tool("browser_screenshot", tab_id=a)
        self.assertEqual(self.led.get("d_pc", a)["reason"], "shown to the user")
        self.end()
        self.clock.advance(5000)
        self.assertEqual(self.sweep(), 0)

    def test_login_that_did_not_sign_in_waits_on_the_user(self):
        self.fake.login_result = {"filled": True, "submitted": False, "reason": "2fa"}
        self.fake.autologin.add("app.example.com")
        a = self.run_tool("browser_login", url="https://app.example.com/login")["tab_id"]
        self.assertTrue(self.led.get("d_pc", a)["keep"])

    def test_login_that_signed_in_is_not_kept(self):
        self.fake.autologin.add("app.example.com")
        a = self.run_tool("browser_login", url="https://app.example.com/login")["tab_id"]
        self.assertFalse(self.led.get("d_pc", a)["keep"])

    def test_kept_tab_goes_when_the_chat_is_deleted(self):
        a = self.open(keep=True)
        browser.on_session_delete(session_id=self.session, reason="delete")
        self.assertEqual(self.sweep(), 1)
        self.assertNotIn(a, self.fake.tabs)

    def test_kept_tab_goes_when_the_chat_is_archived(self):
        a = self.open(keep=True)
        browser.on_session_delete(session_id=self.session, reason="archive")
        self.assertEqual(self.sweep(), 1)
        self.assertNotIn(a, self.fake.tabs)

    def test_kept_tab_has_a_long_idle_limit(self):
        a = self.open(keep=True)
        self.clock.advance(86399)
        self.assertEqual(self.sweep(), 0)
        self.clock.advance(2)
        self.assertEqual(self.sweep(), 1)
        self.assertNotIn(a, self.fake.tabs)


class SubagentTests(TabBase):
    def test_d_subagent_tabs_close_when_it_ends_and_kept_ones_go_to_the_parent(self):
        self.store.rows["child"] = None
        self.bind("child")
        self.session = "child"
        self.turn("child")
        plain = self.open()
        kept = self.open(keep=True, new_tab=True)
        browser.on_subagent_stop(parent_session_id="sess-1", child_session_id="child")
        self.assertEqual(self.sweep(), 1)
        self.assertEqual(self.fake.closed, [plain])
        self.assertEqual(self.led.get("d_pc", kept)["session"], "sess-1")
        self.assertTrue(self.led.get("d_pc", kept)["keep"])

    def test_parent_tabs_are_not_closed_by_the_child_ending(self):
        self.turn()
        a = self.open()
        browser.on_subagent_stop(parent_session_id="sess-1", child_session_id="child")
        self.assertEqual(self.sweep(), 0)
        self.assertIn(a, self.fake.tabs)


class SafetyTests(TabBase):
    def test_e_no_close_while_a_call_is_in_flight(self):
        self.turn()
        a = self.open()
        self.end()
        self.clock.advance(500)
        self.led.begin_call(self.session, "d_pc", a)
        self.assertEqual(self.sweep(), 0)
        self.led.end_call(self.session, "d_pc", a)
        self.clock.advance(121)
        self.assertEqual(self.sweep(), 1)

    def test_no_close_while_another_chat_uses_the_tab(self):
        a = self.open()
        self.clock.advance(5000)
        self.led.begin_call("someone-else", "d_pc", a)
        self.assertEqual(self.sweep(), 0)

    def test_a_slow_call_holds_the_chats_tabs_end_to_end(self):
        self.turn()
        a = self.open()
        self.end()
        self.clock.advance(500)
        seen = {}
        gate, go = threading.Event(), threading.Event()
        real = self.fake.answer

        def slow(msg):
            if msg["op"] == "snapshot":
                gate.set()
                go.wait(5)
            return real(msg)
        self.fake.answer = slow
        t = threading.Thread(target=lambda: seen.update(r=self.run_tool("browser_snapshot", tab_id=a)))
        t.start()
        self.assertTrue(gate.wait(5))
        self.assertEqual(self.sweep(), 0)
        go.set()
        t.join(5)
        self.assertIn("snapshot", seen["r"])
        self.assertIn(a, self.fake.tabs)

    def test_a_tab_firefox_no_longer_lists_is_forgotten_not_closed(self):
        a = self.open()
        del self.fake.tabs[a]
        self.clock.advance(5000)
        self.assertEqual(self.sweep(), 0)
        self.assertIsNone(self.led.get("d_pc", a))
        self.assertEqual(self.fake.closed, [])

    def test_a_reused_tab_id_belongs_to_the_newest_opener(self):
        a = self.open()
        self.led.opened("other-chat", "d_pc", a)
        self.assertFalse(self.led.mine(self.session, "d_pc", a))
        self.led.keep(self.session, "d_pc", a)
        self.assertFalse(self.led.get("d_pc", a)["keep"])

    def test_cleanup_can_be_turned_off(self):
        self.cfg["tab_cleanup"] = False
        a = self.open()
        self.assertIsNone(self.led.get("d_pc", a))
        self.assertEqual(self.sweep(), 0)

    def test_limits_come_from_the_settings(self):
        self.cfg.update(tab_grace_seconds=5, tab_idle_minutes=2, tab_keep_hours=0)
        self.assertEqual(browser.tab_limits(), (5.0, 120.0, 0.0))
        self.cfg.update(tab_grace_seconds="x", tab_idle_minutes=-1)
        self.assertEqual(browser.tab_limits()[:2], (tabs.GRACE_S, tabs.IDLE_S))

    def test_reaper_thread_sweeps(self):
        a = self.open()
        self.clock.advance(5000)
        reaper = tabs.Reaper(self.led, browser.sweep_tabs, interval=0.05)
        reaper.start()
        try:
            for _ in range(100):
                if a in self.fake.closed:
                    break
                threading.Event().wait(0.05)
        finally:
            reaper.stop()
        self.assertEqual(self.fake.closed, [a])


class ReuseTests(TabBase):
    def test_f_navigate_without_tab_id_reuses_the_idle_tab(self):
        a = self.open("https://example.com/")
        out = self.run_tool("browser_navigate", url="https://example.net/")
        self.assertEqual(out["tab_id"], a)
        self.assertTrue(out["reused_tab"])
        self.assertEqual(len([r for r in self.fake.requests if r["op"] == "tab_open"]), 1)
        self.assertEqual(self.fake.tabs[a]["url"], "https://example.net/")

    def test_new_tab_forces_a_new_tab(self):
        a = self.open()
        b = self.open("https://example.net/", new_tab=True)
        self.assertNotEqual(a, b)

    def test_a_kept_tab_is_not_reused(self):
        a = self.open(keep=True)
        b = self.open("https://example.net/")
        self.assertNotEqual(a, b)

    def test_a_tab_the_user_closed_is_replaced(self):
        a = self.open()
        del self.fake.tabs[a]
        out = self.run_tool("browser_navigate", url="https://example.net/")
        self.assertNotEqual(out["tab_id"], a)
        self.assertNotIn("reused_tab", out)
        self.assertIsNone(self.led.get("d_pc", a))
        self.assertTrue(self.led.mine(self.session, "d_pc", out["tab_id"]))

    def test_other_chats_tab_is_not_reused(self):
        a = self.open()
        self.store.rows["other"] = None
        self.session = "other"
        self.bind("other")
        b = self.open("https://example.net/")
        self.assertNotEqual(a, b)

    def test_reuse_uses_the_approval_of_the_visit(self):
        self.open("https://example.com/")
        self.run_tool("browser_navigate", url="https://example.net/")
        self.assertEqual(self.decision["rule_key"], "browser:visit:example.net")
        self.assertEqual(self.last("navigate")["args"]["guard"]["visit_hosts"], ["example.net"])


class DescriptionTests(unittest.TestCase):
    def test_g_tool_text_says_tabs_clean_up_and_how_to_keep(self):
        tabs_text = browser.SCHEMAS["browser_tabs"]["description"]
        self.assertIn("close by themselves", tabs_text)
        self.assertIn("keep", tabs_text)
        self.assertIn("keep", browser.SCHEMAS["browser_tabs"]["parameters"]["properties"])
        self.assertIn("keep", browser.SCHEMAS["browser_navigate"]["parameters"]["properties"])
        self.assertIn("new_tab", browser.SCHEMAS["browser_navigate"]["parameters"]["properties"])
        self.assertIn("reuses", browser.SCHEMAS["browser_navigate"]["description"])


class PersistTests(unittest.TestCase):
    def setUp(self):
        import tempfile
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.path = Path(self.tmp.name) / "tabs.json"

    def test_a_dead_process_tabs_are_adopted_and_a_live_ones_are_not(self):
        dead = tabs.Ledger(self.path, pid=2 ** 22 + 12345)
        dead.opened("s1", "d", 5)
        alive = tabs.Ledger(self.path, pid=1)
        alive.opened("s2", "d", 6)
        me = tabs.Ledger(self.path)
        self.assertEqual(me.adopt(), 1)
        self.assertIsNotNone(me.get("d", 5))
        self.assertIsNone(me.get("d", 6))
        doc = json.loads(self.path.read_text())
        self.assertEqual(sorted(t["tab"] for t in doc["tabs"]), [5, 6])
        self.assertEqual({t["tab"]: t["pid"] for t in doc["tabs"]}[5], me._pid)

    def test_the_file_is_private(self):
        led = tabs.Ledger(self.path)
        led.opened("s", "d", 1)
        self.assertEqual(self.path.stat().st_mode & 0o777, 0o600)


if __name__ == "__main__":
    unittest.main()

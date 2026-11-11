"""Session store views against a real Hermes SessionDB in a throwaway directory. Run inside the
Hermes runtime with scripts/test-plugin.sh."""
from __future__ import annotations

import importlib.util
import json
import os
import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

from fastapi import FastAPI
from starlette.testclient import TestClient

_HERE = Path(__file__).resolve().parent.parent
_spec = importlib.util.spec_from_file_location("hermes_device_insights", _HERE / "insights.py")
insights = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(insights)


def _call(cid: str, name: str, args: dict) -> dict:
    return {"id": cid, "type": "function", "function": {"name": name, "arguments": json.dumps(args)}}


class InsightsTests(unittest.TestCase):
    def setUp(self):
        from hermes_state import SessionDB
        self.tmp = tempfile.TemporaryDirectory(dir=os.environ.get("TMPDIR"))
        self.addCleanup(self.tmp.cleanup)
        self.db = SessionDB(Path(self.tmp.name) / "state.db")
        self.addCleanup(self.db.close)

    def chat(self, sid: str, title: str, source: str = "gui", **kw):
        self.db.create_session(sid, source, **kw)
        if title:
            self.db.set_session_title(sid, title)

    def test_search_groups_hits_by_chat_and_marks_matches(self):
        self.chat("s1", "Trip plans")
        self.db.append_message("s1", "user", "Book a flight to Lisbon in May")
        self.db.append_message("s1", "assistant", "Flights to Lisbon in May start at <b>300</b>")
        self.chat("s2", "Groceries")
        self.db.append_message("s2", "user", "milk and eggs")
        self.chat("w1", "", source="subagent")
        self.db.append_message("w1", "assistant", "Lisbon research notes")
        out = insights.search("lisbon", db=self.db)
        self.assertEqual([c["session_id"] for c in out["results"]], ["s1"])
        chat = out["results"][0]
        self.assertEqual(chat["title"], "Trip plans")
        self.assertEqual(len(chat["hits"]), 2)
        for hit in chat["hits"]:
            self.assertIn(">>>", hit["snippet"])
            self.assertIsInstance(hit["row_id"], int)
            self.assertEqual(hit["session_id"], "s1")
        self.assertEqual(insights.search("nothing-like-this", db=self.db)["results"], [])
        for bad in ("", "   ", "x" * 201, None):
            with self.subTest(q=bad), self.assertRaises(insights.Refused):
                insights.search(bad, db=self.db)

    def test_search_opens_the_compression_tip(self):
        self.chat("root", "Long chat")
        self.db.append_message("root", "user", "remember the kumquat")
        self.db.end_session("root", "compression")
        self.chat("tip", "Long chat 2", parent_session_id="root")
        self.db.append_message("tip", "user", "later")
        out = insights.search("kumquat", db=self.db)
        self.assertEqual(out["results"][0]["session_id"], "tip")
        self.assertEqual(out["results"][0]["hits"][0]["session_id"], "root")

    def test_usage_counts_sessions_and_models(self):
        self.chat("s1", "One", model="qwen")
        self.db.update_token_counts("s1", input_tokens=1000, output_tokens=200, model="qwen", billing_provider="ollama",
                                    api_call_count=3)
        self.chat("s2", "Two")
        out = insights.usage(7, db=self.db)
        self.assertEqual([s["session_id"] for s in out["sessions"]], ["s1"])
        s = out["sessions"][0]
        self.assertEqual((s["input"], s["output"], s["calls"], s["chat_id"]), (1000, 200, 3, "s1"))
        self.assertTrue(out["models"])
        self.assertEqual(out["models"][0]["model"], "qwen")
        self.assertEqual(out["models"][0]["input"], 1000)
        later = insights.usage(1, db=self.db, now=time.time() + 3 * 86400)
        self.assertEqual(later["sessions"], [])

    def test_changes_lists_written_files(self):
        self.chat("s1", "Code")
        patch = "*** Begin Patch\n*** Update File: src/a.py\n@@\n-x\n+y\n*** Add File: src/b.py\n+z\n*** End Patch"
        self.db.append_message("s1", "assistant", "", tool_calls=[
            _call("c1", "write_file", {"path": "notes.md", "content": "hi"}),
            _call("c2", "read_file", {"path": "secret.txt"})])
        self.db.append_message("s1", "assistant", "", tool_calls=[
            _call("c3", "patch", {"path": "notes.md", "old_string": "hi", "new_string": "ho"}),
            _call("c4", "patch", {"mode": "patch", "patch": patch})])
        out = insights.changes("s1", db=self.db)
        paths = [f["path"] for f in out["files"]]
        self.assertEqual(sorted(paths), ["notes.md", "src/a.py", "src/b.py"])
        notes = next(f for f in out["files"] if f["path"] == "notes.md")
        self.assertEqual((notes["count"], notes["tools"]), (2, ["write_file", "patch"]))
        with self.assertRaises(insights.Refused) as ctx:
            insights.changes("missing", db=self.db)
        self.assertEqual(ctx.exception.status, 404)


class InsightRouteTests(unittest.TestCase):
    def setUp(self):
        spec = importlib.util.spec_from_file_location("hermes_device_plugin_api", _HERE / "dashboard" / "plugin_api.py")
        plugin_api = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(plugin_api)
        app = FastAPI()
        app.include_router(plugin_api.router, prefix="/api/plugins/device")
        self.client = TestClient(app)
        self.addCleanup(self.client.close)
        p = mock.patch.object(plugin_api, "_insights", lambda: insights)
        p.start()
        self.addCleanup(p.stop)

    def test_errors_and_busy(self):
        with mock.patch.object(insights, "_db", side_effect=insights.Refused(503, "store_unavailable", "x")):
            r = self.client.get("/api/plugins/device/search", params={"q": "a"})
            self.assertEqual((r.status_code, r.json()["error"]), (503, "store_unavailable"))
        r = self.client.get("/api/plugins/device/search", params={"q": ""})
        self.assertEqual((r.status_code, r.json()["error"]), (400, "bad_query"))
        import threading
        busy = threading.BoundedSemaphore(1)
        busy.acquire()
        with mock.patch.object(insights, "READS", busy):
            r = self.client.get("/api/plugins/device/usage")
            self.assertEqual((r.status_code, r.json()["error"]), (429, "busy"))

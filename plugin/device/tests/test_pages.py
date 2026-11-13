"""page_publish: an HTML file on the chat's device becomes a page the hub shows. Run inside the Hermes
runtime with scripts/test-plugin.sh."""
from __future__ import annotations

import json
import os
import sys
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
from test_core import Base, core  # noqa: E402

pages = core._load_sibling("hermes_device_pages", "pages.py")

PAGE = ('<!doctype html>\n<html lang="en"><head><meta charset="utf-8"><title>Pick entries</title></head>\n'
        '<body><button onclick="hermes.send(\'hi\')">Send</button></body></html>\n')


class PublishTests(Base):
    def setUp(self):
        super().setUp()
        self.store_root = self.base / "hermes" / "cache" / "pages" / "hermes-device"
        p = mock.patch.object(pages, "pages_root", lambda: self.store_root)
        p.start()
        self.addCleanup(p.stop)
        self.bind()

    def call(self, path, sid="sess-1"):
        out = json.loads(pages._handler({"path": str(path)}, session_id=sid))
        return out

    def write(self, rel, data=PAGE):
        f = self.root / rel
        f.parent.mkdir(parents=True, exist_ok=True)
        f.write_bytes(data if isinstance(data, bytes) else data.encode())
        return f

    def test_publishes_a_device_file_and_returns_the_media_line(self):
        f = self.write("report.html")
        out = self.call(f)
        self.assertTrue(out["ok"], out)
        saved = Path(out["path"])
        self.assertEqual(saved.parent, self.store_root / "sess-1")
        self.assertEqual(saved.read_bytes(), PAGE.encode())
        self.assertEqual(out["show_user"], f"MEDIA:{saved}")
        self.assertEqual(out["title"], "Pick entries")
        self.assertEqual(out["bytes"], len(PAGE.encode()))
        self.assertTrue(saved.name.startswith("page-") and saved.suffix == ".html")
        self.assertNotIn("warning", out)
        self.assertEqual(oct(saved.stat().st_mode & 0o777), "0o600")

    def test_relative_path_starts_in_the_chats_folder(self):
        self.write("out/r.html")
        out = self.call("out/r.html")
        self.assertTrue(out["ok"], out)

    def test_path_with_white_space_is_quoted_in_the_line(self):
        with mock.patch.object(pages, "pages_root", lambda: self.base / "my store"):
            out = self.call(self.write("a.html"))
        self.assertEqual(out["show_user"], f"MEDIA:`{out['path']}`")

    def test_later_edits_do_not_change_a_page_already_published(self):
        f = self.write("p.html")
        first = self.call(f)
        f.write_text(PAGE.replace("Pick entries", "Changed"))
        self.assertEqual(Path(first["path"]).read_bytes(), PAGE.encode())

    def test_publishing_a_changed_file_makes_a_new_page(self):
        f = self.write("p.html")
        first = self.call(f)
        again = self.call(f)
        self.assertEqual(first["path"], again["path"])
        f.write_text(PAGE.replace("Pick entries", "Changed"))
        second = self.call(f)
        self.assertNotEqual(first["path"], second["path"])
        self.assertEqual(second["title"], "Changed")
        self.assertIn(b"Changed", Path(second["path"]).read_bytes())
        self.assertEqual(Path(first["path"]).read_bytes(), PAGE.encode())

    def test_missing_title_is_a_warning(self):
        out = self.call(self.write("n.html", "<!doctype html><html><body>x</body></html>"))
        self.assertTrue(out["ok"])
        self.assertIn("<title>", out["warning"])

    def test_size_cap_is_two_megabytes(self):
        self.assertEqual(pages.MAX_PAGE_BYTES, 2 * 1024 * 1024)
        pad = "x" * (pages.MAX_PAGE_BYTES - len(PAGE.encode()))
        ok = self.write("exact.html", PAGE.replace("</body>", pad + "</body>").encode())
        self.assertEqual(ok.stat().st_size, pages.MAX_PAGE_BYTES)
        self.assertTrue(self.call(ok)["ok"])
        big = self.write("big.html", PAGE.encode() + b"x" * pages.MAX_PAGE_BYTES)
        out = self.call(big)
        self.assertEqual(out["code"], "too_large", out)
        self.assertIn("2 MB", out["error"])
        self.assertEqual(len(list(self.store_root.rglob("page-*"))), 1)

    def test_non_html_is_refused(self):
        cases = {"notes.txt": b"just some notes\n", "pic.html": b"\x89PNG\r\n\x1a\n\0\0\0", "empty.html": b"",
                 "data.json": b'{"a": 1}', "latin.html": b"<html><body>caf\xe9</body></html>"}
        for name, data in cases.items():
            with self.subTest(name=name):
                out = self.call(self.write(name, data))
                self.assertIn("error", out)
                self.assertEqual(out["code"], "not_html", out)
        self.assertFalse(self.store_root.exists() and list(self.store_root.rglob("page-*")))

    def test_paths_the_file_tools_may_not_read_are_refused(self):
        secret = self.outside / "private.html"
        secret.write_text(PAGE + "TOPSECRET")
        (self.root / "link.html").symlink_to(secret)
        (self.root / "app").mkdir()
        (self.root / "app" / ".env").write_text("TOPSECRET=1\n<html>")
        (self.home / ".ssh" / "page.html").write_text(PAGE + "TOPSECRET")
        cases = {secret: "read_refused", self.root / "link.html": "read_refused",
                 self.root / "app" / ".env": "read_refused", self.home / ".ssh" / "page.html": "read_refused",
                 self.root / "nope.html": "not_found", self.root / "sub": "read_refused"}
        for path, code in cases.items():
            with self.subTest(path=str(path)):
                out = self.call(path)
                self.assertEqual(out["code"], code, out)
                self.assertNotIn("TOPSECRET", json.dumps(out))
        self.assertFalse(self.store_root.exists() and list(self.store_root.rglob("page-*")))

    def test_offline_device_is_reported(self):
        f = self.write("p.html")
        self.hub.detach(self.device.conn)
        out = self.call(f)
        self.assertEqual(out["code"], "device_offline", out)

    def test_bad_arguments(self):
        for args in ({}, {"path": ""}, {"path": 3}):
            with self.subTest(args=args):
                out = json.loads(pages._handler(args, session_id="sess-1"))
                self.assertEqual(out["code"], "bad_arguments")

    def test_session_on_the_server_reads_a_server_file_under_the_read_rules(self):
        self.store.rows["srv"] = None
        local = self.base / "srv"
        local.mkdir()
        (local / "ok.html").write_text(PAGE)
        out = self.call(local / "ok.html", sid="srv")
        self.assertTrue(out["ok"], out)
        self.assertEqual(Path(out["path"]).parent, self.store_root / "srv")
        secret = local / "creds.html"
        secret.write_text(PAGE + "TOPSECRET")
        with mock.patch("agent.file_safety.get_read_block_error", lambda p: "protected" if "creds" in p else None):
            out = self.call(secret, sid="srv")
        self.assertEqual(out["code"], "read_refused", out)
        (local / "big.html").write_bytes(PAGE.encode() + b"x" * pages.MAX_PAGE_BYTES)
        self.assertEqual(self.call(local / "big.html", sid="srv")["code"], "too_large")

    def test_session_deleted_takes_its_pages(self):
        out = self.call(self.write("p.html"))
        self.assertTrue(Path(out["path"]).is_file())
        pages.on_session_delete(session_id="sess-1", reason="delete")
        self.assertFalse(Path(out["path"]).parent.exists())

    def test_archived_session_keeps_its_pages(self):
        out = self.call(self.write("p.html"))
        pages.on_session_delete(session_id="sess-1", reason="archive")
        self.assertTrue(Path(out["path"]).is_file())

    def test_old_and_surplus_pages_are_pruned(self):
        folder = self.store_root / "s"
        folder.mkdir(parents=True)
        now = 1_000_000_000.0
        for i in range(5):
            f = folder / f"page-{i}.html"
            f.write_text("x")
            os.utime(f, (now - i * 10, now - i * 10))
        old = folder / "page-old.html"
        old.write_text("x")
        os.utime(old, (now - pages.PAGE_MAX_AGE_S - 5, now - pages.PAGE_MAX_AGE_S - 5))
        gone = pages.prune_pages(self.store_root, now=now, max_files=3)
        self.assertEqual(gone, 3)
        self.assertEqual(sorted(p.name for p in folder.iterdir()), ["page-0.html", "page-1.html", "page-2.html"])


class ToolTests(unittest.TestCase):
    def test_registers_one_tool_with_a_path_argument(self):
        ctx = mock.Mock()
        pages.register(ctx, core)
        (call,) = ctx.register_tool.call_args_list
        self.assertEqual(call.args[0], "page_publish")
        schema = call.args[2]
        self.assertEqual(schema["parameters"]["required"], ["path"])
        self.assertIn("2 MB", schema["description"])
        hooks = [c.args[0] for c in ctx.register_hook.call_args_list]
        self.assertIn("on_session_delete", hooks)

    def test_check_html(self):
        self.assertIn("Pick", pages.check_html(PAGE.encode()))
        self.assertEqual(pages.check_html(b"\xef\xbb\xbf" + PAGE.encode())[:9], "<!doctype")
        with self.assertRaises(ValueError):
            pages.check_html(b"plain text")


if __name__ == "__main__":
    unittest.main()

"""A scheduled job's device (docs/browser.md section 3.3): each run runs its commands, files and
browser on the job's computer, the user is asked once per job and computer, and a run that needs
a site approval tells the job's chat once. Run inside the Hermes runtime with
scripts/test-plugin.sh."""
from __future__ import annotations

import json
import sys
import threading
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import test_browser as tb  # noqa: E402
from test_core import core  # noqa: E402

browser = tb.browser

jobs = core._load_sibling("hermes_device_jobs", "jobs.py")


class JobDeviceTests(tb.BrowserDeviceTests):
    """Only the tests below run here; BrowserDeviceTests' own tests run in test_browser."""

    def setUp(self):
        super().setUp()
        self.created: list[dict] = []
        self.updated: list[tuple[str, dict]] = []
        self.handler = jobs.make_handler(self.hermes_cronjob)
        self.jobs: dict[str, dict] = {}
        for p in (mock.patch.object(browser, "_job_exists", lambda j: True),
                  mock.patch.object(browser, "_job_id", lambda ref: ref if ref in self.jobs else ""),
                  mock.patch("cron.jobs.get_job", lambda j: self.jobs.get(j))):
            p.start()
            self.addCleanup(p.stop)

    def hermes_cronjob(self, args, **kw):
        """Hermes's cronjob_manage, reduced to what the wrapper relies on."""
        action = args.get("action")
        if "device" in args or "device_dir" in args:
            raise AssertionError("Hermes's handler got the plugin's fields")
        if action == "create":
            job_id = f"job{len(self.jobs) + 1}"
            job = browser._make_create_job(lambda **k: {"id": job_id, **k})(prompt=args.get("prompt"),
                                                                             schedule=args.get("schedule"))
            self.jobs[job_id] = dict(job, origin={"platform": "local", "chat_id": self.session})
            self.created.append(job)
            return json.dumps({"success": True, "job_id": job_id})
        if action == "update":
            fields = {k: v for k, v in args.items() if k not in ("action", "job_id")}
            if not fields:
                return json.dumps({"success": False, "error": "No updates provided."})
            self.updated.append((args["job_id"], fields))
            return json.dumps({"success": True, "job": {"job_id": args["job_id"]}})
        if action == "list":
            return json.dumps({"success": True, "jobs": [{"job_id": j} for j in self.jobs]})
        return json.dumps({"success": True})

    def cron(self, **args):
        """pre_tool_call, then the wrapped handler, as Hermes runs cronjob_manage."""
        self.decision = browser.pre_tool_call(tool_name="cronjob_manage", args=args, task_id="t",
                                              session_id=self.session)
        if self.decision and self.decision.get("action") == "block":
            return {"error": self.decision["message"]}
        return json.loads(self.handler(args, task_id="t", session_id=self.session))

    def bound_chat(self, sid="chat"):
        self.store.rows[sid] = None
        self.session = sid
        self.hub.bind(sid, "d_pc", str(self.root / "sub"), operator=True)

    # --- binding ---------------------------------------------------------------------------------

    def test_cronjob_manage_offers_device(self):
        import tools.cronjob_tools  # noqa: F401  (registers cronjob_manage)
        from tools.registry import registry
        self.assertTrue(jobs.install())
        self.assertTrue(jobs.install(), "idempotent")
        entry = registry.get_entry("cronjob_manage")
        with mock.patch.object(entry, "check_fn", None):
            params = registry.get_definitions({"cronjob_manage"}, quiet=True)[0]["function"]["parameters"]
        self.assertIn("device", params["properties"])
        self.assertIn("device_dir", params["properties"])
        self.assertIsNotNone(getattr(entry.handler, core._WRAPPED_MARK, None))

    def test_create_in_a_bound_chat_runs_on_that_computer(self):
        self.bound_chat()
        out = self.cron(action="create", prompt="p", schedule="every 3h")
        self.assertEqual((self.decision["action"], self.decision["rule_key"]), ("approve", "browser:job-device:d_pc:new"))
        self.assertTrue(out["success"])
        rec = browser.choices().job("job1")
        self.assertEqual((rec["device_id"], rec["dir"], rec["user"]), ("d_pc", str(self.root / "sub"), "alice"))
        self.assertEqual(out["device"], {"name": "pc", "device_id": "d_pc", "dir": str(self.root / "sub")})
        self.assertNotIn("browser_note", json.loads(browser.schedule_note(
            "cronjob_manage", {"action": "create"}, json.dumps(out), self.session) or json.dumps(out)))

    def test_create_with_a_named_device_and_dir(self):
        other, _ = self.second()
        out = self.cron(action="create", prompt="p", schedule="every 3h", device="laptop", device_dir=str(self.root))
        self.assertEqual(self.decision["rule_key"], "browser:job-device:d_pc2:new")
        self.assertEqual((out["device"]["device_id"], out["device"]["dir"]), ("d_pc2", str(self.root)))
        self.assertEqual(browser.choices().job("job1")["device_id"], "d_pc2")

    def test_create_refusals_create_nothing(self):
        for args, why in (({"device": "nope"}, "none of the user's connected computers"),
                          ({"device": "pc", "device_dir": "relative"}, "absolute path"),
                          ({"device": "pc", "device_dir": str(self.outside)}, "outside the allowed roots"),
                          ({"device": "sandbox", "device_dir": str(self.root)}, "device_dir needs a device")):
            with self.subTest(args=args):
                out = self.cron(action="create", prompt="p", schedule="every 3h", **args)
                self.assertIn(why, out["error"])
        self.assertEqual(self.created, [])
        self.second(device_id="d_bob", name="bobs", user="bob")
        del self.cfg["user"]
        self.bound_chat()
        out = self.cron(action="create", prompt="p", schedule="every 3h", device="bobs")
        self.assertIn("none of the user's connected computers is named 'bobs'", out["error"])
        self.assertEqual(self.created, [])

    def test_a_run_creating_a_job_gives_it_no_computer(self):
        self.bound_chat()
        self.cron(action="create", prompt="p", schedule="every 3h")
        self.run_of("job1")
        self.store.rows["cron_run"] = None
        with mock.patch.object(browser, "no_user", lambda: True):
            out = self.cron(action="create", prompt="q", schedule="every 3h")
            self.assertIsNone(self.decision, "nothing to ask: the new job gets no computer")
            self.assertIsNone(out["device"])
            out = self.cron(action="create", prompt="q", schedule="every 3h", device="pc")
            self.assertEqual(self.decision["action"], "block")

    def test_create_in_a_sandbox_chat_has_no_device(self):
        out = self.cron(action="create", prompt="p", schedule="every 3h")
        self.assertIsNone(self.decision)
        self.assertIsNone(out["device"])
        self.assertIn("update it with device", out["device_note"])
        self.assertIsNone(browser.choices().job("job1"))

    def test_update_moves_the_job_and_asks_only_then(self):
        self.bound_chat()
        self.cron(action="create", prompt="p", schedule="every 3h")
        other, _ = self.second()
        out = self.cron(action="update", job_id="job1", device="d_pc2")
        self.assertEqual(self.decision["rule_key"], "browser:job-device:d_pc2:job1")
        self.assertTrue(out["success"], "an update of only the device is not Hermes's \"No updates provided\"")
        self.assertEqual(self.updated, [])
        self.assertEqual(browser.choices().job("job1")["device_id"], "d_pc2")
        out = self.cron(action="update", job_id="job1", prompt="new prompt")
        self.assertIsNone(self.decision, "an unrelated update asks nothing")
        self.assertEqual(self.updated, [("job1", {"prompt": "new prompt"})])
        self.assertEqual(browser.choices().job("job1")["device_id"], "d_pc2", "and keeps the device")
        out = self.cron(action="update", job_id="job1", device="laptop", schedule="every 2h")
        self.assertIsNone(self.decision, "the same device again asks nothing")
        self.assertEqual(self.updated[-1], ("job1", {"schedule": "every 2h"}))
        out = self.cron(action="update", job_id="job1", device="none")
        self.assertIsNone(self.decision)
        self.assertIsNone(out["device"])
        self.assertIsNone(browser.choices().job("job1"))
        for action in ("run", "resume", "pause"):
            self.cron(action=action, job_id="job1")
            self.assertIsNone(self.decision)

    def test_a_failed_hermes_update_changes_no_device(self):
        self.bound_chat()
        self.cron(action="create", prompt="p", schedule="every 3h")
        self.second()
        with mock.patch.object(self, "hermes_cronjob", lambda a, **k: json.dumps({"success": False, "error": "bad"})):
            self.handler = jobs.make_handler(self.hermes_cronjob)
            out = self.cron(action="update", job_id="job1", device="d_pc2", schedule="nonsense")
        self.assertEqual(out["error"], "bad")
        self.assertEqual(browser.choices().job("job1")["device_id"], "d_pc")

    def test_list_shows_the_device(self):
        self.bound_chat()
        self.cron(action="create", prompt="p", schedule="every 3h")
        out = self.cron(action="list")
        self.assertEqual(out["jobs"][0]["device"]["device_id"], "d_pc")

    def test_browser_use_device_for_a_job_sets_its_computer(self):
        self.jobs["old"] = {"id": "old"}
        browser.choices().set_job_owner("old", "alice", "fresh")
        out = self.run_tool("browser_use_device", device="pc", job_id="old")
        self.assertEqual(self.decision["rule_key"], "browser:job-device:d_pc:old")
        self.assertEqual((out["device_id"], out["dir"]), ("d_pc", str(self.root)))
        self.assertEqual(browser.choices().job("old")["dir"], str(self.root))

    # --- runs ----------------------------------------------------------------------------------

    def test_run_uses_the_jobs_computer_for_commands_files_and_browser(self):
        self.bound_chat()
        self.cron(action="create", prompt="p", schedule="every 3h")
        self.run_of("job1")
        self.device.execs.clear()
        r = self.env().execute("pwd")
        self.assertEqual((r["returncode"], r["output"].strip()), (0, str(self.root / "sub")))
        self.assertTrue(self.device.execs, "the job's computer ran it")
        self.assertEqual(self.sandbox.execs, [], "never the sandbox")
        res = core.resolve_current()
        self.assertEqual((res.state, res.sandbox, res.binding.device_id), (core.STATE_BOUND, False, "d_pc"))
        with mock.patch.object(core, "current_session_id", lambda: "cron_run"), \
                mock.patch("tools.file_tools_paths._terminal_env_type_for_task", lambda t: core.PLUGIN_ID):
            r = core.pre_tool_call(tool_name="read_file", args={"path": "x.txt"}, task_id="t")
        self.assertEqual(r, {"action": "modify", "args": {"path": str(self.root / "sub" / "x.txt")}})
        status = self.run_tool("browser_status")
        self.assertEqual((status["device_id"], status["why"]), ("d_pc", "the browser device of this scheduled job"))
        self.assertEqual(self.run_tool("browser_navigate", url="https://example.com/")["tab_id"], 8)

    def test_run_of_a_job_whose_computer_is_offline_says_so(self):
        self.bound_chat()
        self.cron(action="create", prompt="p", schedule="every 3h")
        self.run_of("job1")
        self.hub.detach(self.device.conn)
        self.sandbox.execs.clear()
        r = self.env().execute("pwd")
        self.assertNotEqual(r["returncode"], 0)
        self.assertIn("device d_pc is not connected", r["output"])
        self.assertEqual(self.sandbox.execs, [], "no silent fallback to the sandbox")

    def test_run_of_a_job_whose_computer_changed_hands_fails_closed(self):
        self.bound_chat()
        self.cron(action="create", prompt="p", schedule="every 3h")
        self.run_of("job1")
        self.device.conn.user = "bob"
        res = core.resolve_current()
        self.assertEqual(res.state, core.STATE_FAIL_CLOSED)
        self.assertIn("now belongs to another user", res.reason)

    def test_browser_only_records_keep_commands_in_the_sandbox(self):
        """A job given only a browser before jobs had computers keeps running commands in the sandbox."""
        browser.choices().set_job("job9", {"device_id": "d_pc", "name": "pc", "user": "alice"})
        self.run_of("job9")
        r = self.env().execute("pwd")
        self.assertEqual(r["output"].strip(), str(self.base / "work" / "cron_run"))

    # --- site approvals in runs ------------------------------------------------------------------

    def test_run_needing_a_site_tells_the_jobs_chat_once(self):
        self.bound_chat()
        self.cron(action="create", prompt="p", schedule="every 3h")
        agents = core._load_sibling("hermes_device_agents", "agents.py")
        sent, done = [], threading.Event()

        def call(op, caller, args, timeout=0):
            sent.append((op, caller.get("session_id"), args))
            done.set()
            return {"ok": True}
        self.run_of("job1")
        with mock.patch.object(agents.BUS, "call", call), mock.patch.object(browser, "no_user", lambda: True):
            out = self.run_tool("browser_navigate", url="https://chosozo.com/")
            self.assertIn("rule browser:visit:chosozo.com", out["error"])
            self.assertIn("The job's chat was told once", out["error"])
            self.assertTrue(done.wait(5))
            out = self.run_tool("browser_navigate", url="https://chosozo.com/")
            self.assertNotIn("told once", out["error"])
        self.assertEqual(len(sent), 1)
        op, caller, args = sent[0]
        self.assertEqual((op, caller, args["to"]), (agents.OP_SEND, "cron_run", "chat"))
        self.assertIn('browser_allow_site with rule "browser:visit:chosozo.com" and job_id "job1"', args["text"])
        self.assertIn("job1 browser:visit:chosozo.com", json.loads(self.choices_path.read_text())["job_asks"])

    def test_a_failed_notice_is_sent_again_later(self):
        self.bound_chat()
        self.cron(action="create", prompt="p", schedule="every 3h")
        agents = core._load_sibling("hermes_device_agents", "agents.py")
        tries, done = [], threading.Event()

        def call(op, caller, args, timeout=0):
            tries.append(1)
            done.set()
            raise agents.HubUnavailable("hub away")
        self.run_of("job1")
        with mock.patch.object(agents.BUS, "call", call), mock.patch.object(browser, "no_user", lambda: True):
            self.run_tool("browser_navigate", url="https://chosozo.com/")
            self.assertTrue(done.wait(5))
            for _ in range(50):
                if not json.loads(self.choices_path.read_text())["job_asks"]:
                    break
                threading.Event().wait(0.05)
            self.assertEqual(json.loads(self.choices_path.read_text())["job_asks"], {})

    def test_allow_site_asks_for_exactly_that_rule(self):
        self.session = "sess-1"
        gate = browser.pre_tool_call
        r = gate(tool_name="browser_allow_site", args={"rule": "browser:visit:chosozo.com", "job_id": "job1"},
                 session_id=self.session)
        self.assertEqual((r["action"], r["rule_key"]), ("approve", "browser:visit:chosozo.com"))
        self.assertIn("Only \"Always\" also covers scheduled runs", r["message"])
        for bad in ("browser:job-device:d_pc:job1", "browser:visit:", "rm -rf /", "browser:visit:a b"):
            self.assertIsNone(gate(tool_name="browser_allow_site", args={"rule": bad}, session_id=self.session))
            self.assertIn("error", json.loads(jobs.allow_site({"rule": bad})))
        with mock.patch.object(browser, "no_user", lambda: True):
            self.assertEqual(gate(tool_name="browser_allow_site", args={"rule": "browser:visit:x.com"},
                                  session_id=self.session)["action"], "block")
        with mock.patch.object(jobs, "permanent", lambda rule: True):
            self.assertTrue(json.loads(jobs.allow_site({"rule": "browser:visit:chosozo.com"}))["always"])
        self.assertFalse(json.loads(jobs.allow_site({"rule": "browser:visit:chosozo.com"}))["always"])


# Run only the tests defined here, not the inherited BrowserDeviceTests ones.
for _name in [n for n in dir(tb.BrowserDeviceTests) if n.startswith("test_") and n not in JobDeviceTests.__dict__]:
    setattr(JobDeviceTests, _name, None)

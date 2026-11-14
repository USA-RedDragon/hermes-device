"""Skill mirror tests: every skill_manage change lands as a commit in a git repository on the device,
through a real bash and real git; an offline device or a failed commit only queues. Run inside the
Hermes runtime with scripts/test-plugin.sh."""
from __future__ import annotations

import json
import subprocess
import sys
import threading
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
from test_core import Base, core  # noqa: E402

skillsync = core._load_sibling("hermes_device_skillsync", "skillsync.py")

README = "# skills\n\n## Skills\n\n| Skill | Category | Origin |\n|---|---|---|\n| old | devops | moved |\n\nOrigins are paths.\n"


def git(repo: Path, *args: str) -> str:
    return subprocess.run(["git", "-C", str(repo), *args], check=True, capture_output=True, text=True).stdout.strip()


def ok(op_action: str, name: str, **extra) -> tuple[dict, str]:
    return {"operations": [{"action": op_action, "name": name, **extra}]}, json.dumps({"success": True})


class MirrorBase(Base):
    def setUp(self):
        super().setUp()
        self.repo = self.root / "skills-repo"
        self.repo.mkdir()
        git(self.repo, "init", "-q", "-b", "main")
        for k, v in (("user.name", "Jacob"), ("user.email", "j@example.com"), ("commit.gpgsign", "false")):
            git(self.repo, "config", k, v)
        (self.repo / "README.md").write_text(README)
        git(self.repo, "add", "-A")
        git(self.repo, "commit", "-q", "-m", "start")
        self.pod = self.base / "pod"
        self.pod.mkdir()
        self.cfg.update(skill_repo_device="d_pc", skill_repo_path=str(self.repo))
        self.queue = self.base / "queue" / "skill-sync.json"
        for p in (mock.patch.object(skillsync, "_queue_path", lambda: self.queue),
                  mock.patch.object(skillsync, "locate", self.locate),
                  mock.patch.object(skillsync, "_ensure_worker", lambda: None)):
            p.start()
            self.addCleanup(p.stop)

    def locate(self, name, project=None):
        base = self.pod / "projects" / project if project else self.pod
        for path in base.rglob("SKILL.md"):
            if path.parent.name == name and (project or "projects" not in path.relative_to(self.pod).parts):
                return path.parent, path.parent.relative_to(self.pod).as_posix()
        return None

    def skill(self, rel: str, body: str = "v1", extra: dict | None = None) -> Path:
        d = self.pod / rel
        d.mkdir(parents=True, exist_ok=True)
        (d / "SKILL.md").write_text(f"---\nname: {d.name}\ndescription: x\n---\n{body}\n")
        for k, v in (extra or {}).items():
            (d / k).parent.mkdir(parents=True, exist_ok=True)
            (d / k).write_text(v)
        return d

    def hook(self, action: str, name: str, **extra) -> None:
        args, result = ok(action, name, **extra)
        skillsync.post_tool_call(tool_name="skill_manage", args=args, result=result)

    def subjects(self) -> list[str]:
        return git(self.repo, "log", "--format=%s").splitlines()


class MirrorTests(MirrorBase):
    def test_create_commits_the_skill_and_adds_a_readme_row(self):
        self.skill("devops/new-one", extra={"references/a.md": "ref"})
        self.hook("create", "new-one")
        self.assertTrue(skillsync.drain_once())
        self.assertEqual(self.subjects()[0], "new-one: create")
        self.assertEqual((self.repo / "devops/new-one/references/a.md").read_text(), "ref")
        rows = (self.repo / "README.md").read_text().splitlines()
        row = next(r for r in rows if r.startswith("| new-one |"))
        self.assertRegex(row, r"^\| new-one \| devops \| made by agents in Hermes \(\d{4}-\d\d-\d\d\) \|$")
        self.assertEqual(rows.index(row), rows.index("| old | devops | moved |") + 1)
        self.assertEqual(git(self.repo, "status", "--porcelain"), "")
        self.assertEqual(skillsync.pending(), [])

    def test_top_level_skill_row_says_top_level(self):
        self.skill("flat")
        self.hook("create", "flat")
        skillsync.drain_once()
        self.assertIn("| flat | (top level) |", (self.repo / "README.md").read_text())

    def test_patch_write_file_remove_file_each_commit_with_their_action(self):
        d = self.skill("devops/s", extra={"references/a.md": "a"})
        self.hook("create", "s")
        skillsync.drain_once()
        (d / "SKILL.md").write_text("---\nname: s\ndescription: x\n---\nv2\n")
        self.hook("patch", "s", old_string="v1", new_string="v2")
        skillsync.drain_once()
        (d / "references/b.md").write_text("b")
        self.hook("write_file", "s", file_path="references/b.md", file_content="b")
        skillsync.drain_once()
        (d / "references/a.md").unlink()
        self.hook("remove_file", "s", file_path="references/a.md")
        skillsync.drain_once()
        self.assertEqual(self.subjects()[:4], ["s: remove_file", "s: write_file", "s: patch", "s: create"])
        self.assertFalse((self.repo / "devops/s/references/a.md").exists())
        self.assertEqual((self.repo / "devops/s/references/b.md").read_text(), "b")
        self.assertIn("v2", (self.repo / "devops/s/SKILL.md").read_text())
        self.assertEqual(sum(r.startswith("| s |") for r in (self.repo / "README.md").read_text().splitlines()), 1)

    def test_delete_removes_the_skill_and_its_readme_row_in_one_commit(self):
        d = self.skill("devops/gone")
        self.hook("create", "gone")
        skillsync.drain_once()
        for f in d.rglob("*"):
            f.unlink()
        d.rmdir()
        self.hook("delete", "gone")
        skillsync.drain_once()
        self.assertEqual(self.subjects()[:2], ["gone: delete", "gone: create"])
        self.assertFalse((self.repo / "devops/gone").exists())
        self.assertNotIn("| gone |", (self.repo / "README.md").read_text())
        self.assertEqual(git(self.repo, "status", "--porcelain"), "")

    def test_a_skill_the_repo_never_had_deletes_without_a_commit(self):
        before = git(self.repo, "rev-parse", "HEAD")
        self.hook("delete", "never-there")
        self.assertTrue(skillsync.drain_once())
        self.assertEqual(git(self.repo, "rev-parse", "HEAD"), before)

    def test_unchanged_skill_makes_no_commit(self):
        self.skill("devops/same")
        self.hook("create", "same")
        skillsync.drain_once()
        before = git(self.repo, "rev-parse", "HEAD")
        self.hook("patch", "same")
        self.assertTrue(skillsync.drain_once())
        self.assertEqual(git(self.repo, "rev-parse", "HEAD"), before)

    def test_local_edits_in_the_repo_are_committed_before_they_are_replaced(self):
        d = self.skill("devops/mine")
        self.hook("create", "mine")
        skillsync.drain_once()
        (self.repo / "devops/mine/SKILL.md").write_text("edited by hand\n")
        (d / "SKILL.md").write_text("---\nname: mine\ndescription: x\n---\nfrom pod\n")
        self.hook("patch", "mine")
        skillsync.drain_once()
        self.assertEqual(self.subjects()[:3], ["mine: patch", "mine: save local edits", "mine: create"])
        self.assertEqual(git(self.repo, "show", "HEAD~1:devops/mine/SKILL.md"), "edited by hand")

    def test_other_staged_work_in_the_repo_is_not_swept_into_the_commit(self):
        (self.repo / "other.txt").write_text("x")
        git(self.repo, "add", "other.txt")
        self.skill("devops/clean")
        self.hook("create", "clean")
        skillsync.drain_once()
        self.assertNotIn("other.txt", git(self.repo, "show", "--stat", "--format=", "HEAD"))
        self.assertEqual(git(self.repo, "status", "--porcelain"), "A  other.txt")

    def test_changes_to_one_skill_in_a_queue_merge_into_one_commit(self):
        d = self.skill("devops/m")
        self.hook("create", "m")
        (d / "SKILL.md").write_text("---\nname: m\ndescription: x\n---\nlater\n")
        self.hook("patch", "m")
        self.hook("write_file", "m", file_path="references/x.md", file_content="x")
        skillsync.drain_once()
        self.assertEqual(self.subjects()[:2], ["m: create", "start"])

    def test_merge_rules(self):
        e = lambda a, n: {"action": a, "name": n, "at": "2026-01-01"}  # noqa: E731
        self.assertEqual([x["action"] for x in skillsync.merge([e("create", "a"), e("patch", "a")])], ["create"])
        self.assertEqual([x["action"] for x in skillsync.merge([e("patch", "a"), e("write_file", "a")])], ["write_file"])
        self.assertEqual([x["action"] for x in skillsync.merge([e("create", "a"), e("delete", "a")])], ["delete"])
        self.assertEqual([x["name"] for x in skillsync.merge([e("patch", "b"), e("patch", "a"), e("patch", "b")])], ["b", "a"])


class ProjectSkillTests(MirrorBase):
    """The pod keeps a project skill at projects/<slug>/<skill>, which is also its path in the repository."""

    def hook_project(self, action, name, slug, **extra):
        args = {"operations": [{"action": action, "name": name, "scope": "project", **extra}]}
        result = json.dumps({"success": True, "results": [{"name": name, "action": action, "scope": "project",
                                                           "project": slug}]})
        skillsync.post_tool_call(tool_name="skill_manage", args=args, result=result)

    def test_a_project_skill_is_committed_under_projects_slug(self):
        self.skill("projects/github.com-alice-demo/layout")
        self.hook_project("create", "layout", "github.com-alice-demo")
        skillsync.drain_once()
        self.assertTrue((self.repo / "projects/github.com-alice-demo/layout/SKILL.md").is_file())
        self.assertEqual(self.subjects()[0], "layout (github.com-alice-demo): create")
        self.assertIn("| layout | project: github.com-alice-demo |", (self.repo / "README.md").read_text())

    def test_project_and_global_skills_of_one_name_are_two_entries(self):
        self.skill("projects/p1/same", "project body")
        self.skill("devops/same", "global body")
        self.hook_project("create", "same", "p1")
        self.hook("create", "same")
        self.assertEqual(len(skillsync.merge(skillsync.pending())), 2)
        skillsync.drain_once()
        self.assertIn("project body", (self.repo / "projects/p1/same/SKILL.md").read_text())
        self.assertIn("global body", (self.repo / "devops/same/SKILL.md").read_text())

    def test_deleting_a_project_skill_leaves_the_global_one(self):
        self.skill("projects/p1/same", "project body")
        self.skill("devops/same", "global body")
        self.hook_project("create", "same", "p1")
        self.hook("create", "same")
        skillsync.drain_once()
        import shutil
        shutil.rmtree(self.pod / "projects/p1/same")
        self.hook_project("delete", "same", "p1")
        skillsync.drain_once()
        self.assertFalse((self.repo / "projects/p1/same").exists())
        self.assertTrue((self.repo / "devops/same/SKILL.md").is_file())

    def test_a_hostile_project_slug_is_not_queued(self):
        for slug in ("../x", "a/b", "x y", ""):
            skillsync.enqueue("create", "s", slug) if slug else None
        self.assertEqual(skillsync.pending(), [])


class QueueTests(MirrorBase):
    def test_offline_device_keeps_the_entry_and_a_later_drain_lands_it(self):
        self.skill("devops/late")
        self.cfg["skill_repo_device"] = "d_away"
        self.hook("create", "late")
        self.assertFalse(skillsync.drain_once())
        self.assertEqual([e["name"] for e in skillsync.pending()], ["late"])
        self.assertEqual(self.subjects(), ["start"])
        self.cfg["skill_repo_device"] = "d_pc"
        self.assertTrue(skillsync.drain_once())
        self.assertEqual(self.subjects()[0], "late: create")

    def test_the_queue_survives_a_restart(self):
        self.skill("devops/kept")
        self.cfg["skill_repo_device"] = "d_away"
        self.hook("create", "kept")
        raw = json.loads(self.queue.read_text())
        self.assertEqual(raw["entries"][0]["name"], "kept")
        self.cfg["skill_repo_device"] = "d_pc"
        self.assertTrue(skillsync.drain_once())

    def test_a_failing_repository_keeps_the_entry_and_does_not_raise(self):
        self.skill("devops/bad")
        self.cfg["skill_repo_path"] = str(self.root / "sub")  # not a git repository
        self.hook("create", "bad")
        self.assertFalse(skillsync.drain_once())
        self.assertEqual([e["name"] for e in skillsync.pending()], ["bad"])

    def test_the_worker_retries_until_the_device_is_back_then_stops(self):
        self.skill("devops/w")
        self.cfg["skill_repo_device"] = "d_away"
        self.hook("create", "w")
        slept = []

        def sleep(seconds):
            slept.append(seconds)
            self.cfg["skill_repo_device"] = "d_pc"
            return False

        with mock.patch.object(skillsync, "_sleep", sleep):
            t = threading.Thread(target=skillsync._run)
            t.start()
            t.join(20)
        self.assertFalse(t.is_alive())
        self.assertEqual(slept, [skillsync.RETRY_SECONDS])
        self.assertEqual(self.subjects()[0], "w: create")

    def test_a_change_queued_while_one_is_sent_stays_queued(self):
        self.skill("devops/race")
        self.hook("create", "race")
        real = skillsync.sync_one

        def sync_one(entry, device_id, repo):
            done = real(entry, device_id, repo)
            self.hook("patch", "race")  # queued while the first is in flight
            return done

        with mock.patch.object(skillsync, "sync_one", sync_one):
            skillsync.drain_once()
        self.assertEqual([e["action"] for e in skillsync.pending()], ["patch"])


class HookTests(MirrorBase):
    def test_off_without_settings(self):
        self.cfg.pop("skill_repo_device")
        self.hook("create", "x")
        self.assertFalse(self.queue.exists())

    def test_settings_need_a_device_and_an_absolute_path(self):
        self.cfg["skill_repo_path"] = "relative/path"
        self.assertIsNone(skillsync.config())
        self.cfg.update(skill_repo_path="/ok", skill_repo_device="")
        self.assertIsNone(skillsync.config())

    def test_only_successful_skill_manage_calls_count(self):
        args, _ = ok("create", "x")
        skillsync.post_tool_call(tool_name="terminal", args=args, result=json.dumps({"success": True}))
        skillsync.post_tool_call(tool_name="skill_manage", args=args, result=json.dumps({"success": False}))
        skillsync.post_tool_call(tool_name="skill_manage", args=args, result=json.dumps({"success": True, "staged": True}))
        skillsync.post_tool_call(tool_name="skill_manage", args=args, result="not json")
        self.assertEqual(skillsync.pending(), [])

    def test_batch_and_flat_shapes_queue_every_op(self):
        args = {"operations": [{"action": "create", "name": "a"}, {"action": "write_file", "name": "a"},
                               {"action": "patch", "name": "cat/b"}]}
        skillsync.post_tool_call(tool_name="skill_manage", args=args, result=json.dumps({"success": True}))
        skillsync.post_tool_call(tool_name="skill_manage", args={"action": "delete", "name": "c"},
                                 result=json.dumps({"success": True}))
        self.assertEqual([(e["action"], e["name"]) for e in skillsync.pending()],
                         [("create", "a"), ("write_file", "a"), ("patch", "b"), ("delete", "c")])

    def test_a_broken_queue_never_breaks_the_save(self):
        with mock.patch.object(skillsync, "_save", side_effect=OSError("disk full")):
            self.hook("create", "x")  # must not raise
        self.queue.parent.mkdir(parents=True, exist_ok=True)
        self.queue.write_text("{not json")
        self.assertEqual(skillsync.pending(), [])

    def test_a_hostile_name_is_not_queued(self):
        for name in ("a b", "x;rm", "$(x)", ".."):
            self.hook("create", name)
        self.assertEqual([e["name"] for e in skillsync.pending()], [])

    def test_register_adds_the_hook(self):
        hooks = {}
        ctx = mock.Mock(register_hook=lambda name, fn: hooks.setdefault(name, fn))
        skillsync.register(ctx, core)
        self.assertIs(hooks["post_tool_call"], skillsync.post_tool_call)


if __name__ == "__main__":
    unittest.main()

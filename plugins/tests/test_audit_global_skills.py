import importlib.util
import json
from pathlib import Path

SCRIPT = Path(__file__).resolve().parent.parent / "scripts" / "audit-global-skills.py"
spec = importlib.util.spec_from_file_location("audit_global_skills", SCRIPT)
audit = importlib.util.module_from_spec(spec)
spec.loader.exec_module(audit)


def skill(root, rel, body):
    d = root / rel
    d.mkdir(parents=True)
    (d / "SKILL.md").write_text(f"---\nname: {d.name}\ndescription: x\n---\n{body}\n")


def test_flags_paths_repos_hosts_and_project_names_but_not_generic_skills(tmp_path):
    skills, mem = tmp_path / "skills", tmp_path / "mem"
    skill(skills, "devops/deploy", "Edit /home/alice/Projects/cluster/app.yaml then ssh box1.lan and push to git@github.com:alice/cluster.git")
    skill(skills, "software/generic", "Run the tests, then open a pull request. Use the project's own commands.")
    skill(skills, "software/named", "The widgetron service needs a restart.")
    skill(skills, ".archive/old", "/home/alice/x")
    (mem / "alice-widgetron").mkdir(parents=True)
    (mem / "alice-widgetron" / "meta.json").write_text(json.dumps({"identity": "github.com/alice/widgetron", "name": "widgetron"}))
    rows = audit.audit(skills, mem)
    assert [r["skill"] for r in rows] == ["deploy", "named"]
    kinds = {h["kind"] for h in rows[0]["hits"]}
    assert {"path", "repo", "host"} <= kinds
    assert rows[1]["hits"][0]["kind"] == "project"


def test_it_only_reads(tmp_path, capsys):
    skills = tmp_path / "skills"
    skill(skills, "a/one", "see /home/bob/code/thing")
    before = sorted(p.name for p in skills.rglob("*"))
    assert audit.main(["--skills", str(skills), "--memories", str(tmp_path / "none")]) == 0
    assert "a/one" in capsys.readouterr().out
    assert before == sorted(p.name for p in skills.rglob("*"))

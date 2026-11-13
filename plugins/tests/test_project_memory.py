import json

import pytest


@pytest.fixture
def mem(pm, hermes_home, monkeypatch):
    monkeypatch.setattr(pm, "_config", lambda: {})
    monkeypatch.setattr(pm, "_approval_on", lambda: False)
    monkeypatch.setattr(pm, "_unattended_review", lambda: False)
    return pm


def project(mem, identity="github.com/alice/demo", kind="remote", root=""):
    with mem._locked():
        return mem.attach({"identity": identity, "kind": kind, "name": identity.rsplit("/", 1)[-1], "root": root})


@pytest.mark.parametrize("url,want", [
    ("git@github.com:Alice/Demo.git", "github.com/alice/demo"),
    ("https://user:pw@github.com/alice/demo.git/", "github.com/alice/demo"),
    ("ssh://git@host.example:2222/team/repo", "host.example/team/repo"),
    ("/srv/git/demo", None),
    ("file:///srv/git/demo", None),
    ("", None),
    (None, None),
    ("https://github.com/../x", None),
])
def test_normalize_remote(pm, url, want):
    assert pm.normalize_remote(url) == want


def test_parse_probe(pm):
    out = "noise\n@@pm top /home/alice/demo\n@@pm origin git@github.com:alice/demo.git\n@@pm root " + "a" * 40
    assert pm.parse_probe(out) == {"top": "/home/alice/demo", "origin": "git@github.com:alice/demo.git",
                                   "root": "a" * 40}
    assert pm.parse_probe("@@pm nogit") == {"nogit": "1"}
    assert pm.parse_probe("@@pm error boom") == {"error": "boom"}


def test_identity_order(pm):
    sha = "b" * 40
    probe = {"top": "/w/demo", "upstream": "git@github.com:alice/demo.git", "root": sha}
    assert pm.identity_from_probe(probe, "dev1", "/w/demo")["identity"] == "github.com/alice/demo"
    no_remote = {"top": "/w/demo", "root": sha}
    assert pm.identity_from_probe(no_remote, "dev1", "/w/demo") == {
        "identity": f"root:{sha}", "kind": "root", "name": "demo", "root": sha}
    assert pm.identity_from_probe({"nogit": "1"}, "dev1", "/w/demo/")["identity"] == "folder:dev1:/w/demo"
    assert pm.identity_from_probe({"error": "x"}, "d", "/w") == {"error": "x"}
    assert pm.identity_from_probe({}, "d", "/w") is None


def test_slugs_are_safe(pm):
    for ident in ("github.com/alice/demo", "root:" + "c" * 40, "folder:dev1:/home/alice/demo", "x/" + "y" * 400):
        slug = pm.slug_for(ident)
        assert pm.SLUG_RE.fullmatch(slug) and ".." not in slug and len(slug) <= 160
    assert pm.slug_for("github.com/alice/demo") == "github.com-alice-demo"


def test_mirrors_share_one_project(mem):
    sha = "d" * 40
    a = project(mem, "github.com/alice/demo", root=sha)
    b = project(mem, "gitlab.example/alice/demo", root=sha)
    assert a == b
    assert mem.read_meta(a)["aliases"] == ["gitlab.example/alice/demo"]
    other = project(mem, "github.com/alice/other", root="e" * 40)
    assert other != a


def test_notes_add_edit_remove(mem):
    slug = project(mem)
    assert mem.notes(slug)["targets"]["memory"]["entries"] == []
    mem.add(slug, "Tests run with make test")
    mem.add(slug, "Deploys come from main")
    entries = mem.notes(slug)["targets"]["memory"]["entries"]
    assert entries == ["Tests run with make test", "Deploys come from main"]
    mem.edit(slug, "Deploys come from main", "Deploys come from the release branch")
    mem.remove(slug, "Tests run with make test")
    assert mem.entries(slug) == ["Deploys come from the release branch"]
    assert mem.list_projects()["projects"][0]["count"] == 1


def test_limits_and_errors(mem, monkeypatch):
    slug = project(mem)
    monkeypatch.setattr(mem, "char_limit", lambda cfg=None: 200)
    with pytest.raises(mem.Refused) as exc:
        mem.add(slug, "x" * 400)
    assert exc.value.status == 409 and exc.value.code == "over_limit"
    with pytest.raises(mem.Refused) as exc:
        mem.add("no-such-project", "x")
    assert exc.value.status == 404
    with pytest.raises(mem.Refused):
        mem.add(slug, "   ")


def test_replace_all(mem):
    slug = project(mem)
    mem.add(slug, "one")
    mem.add(slug, "two")
    mem.replace_all(slug, "two\n§\nthree", ["one", "two"])
    assert mem.entries(slug) == ["two", "three"]
    with pytest.raises(mem.Refused) as exc:
        mem.replace_all(slug, "", ["two", "three"])
    assert exc.value.code == "would_empty"


def test_char_limit_setting(pm):
    assert pm.char_limit({}) == 2200
    assert pm.char_limit({"memory": {"memory_char_limit": 8000}}) == 3000  # capped by the prompt section
    cfg = {"plugins": {"entries": {"project-memory": {"settings": {"char_limit": 500}}}}}
    assert pm.char_limit(cfg) == 500
    assert pm.char_limit({"plugins": {"entries": {"project-memory": {"settings": {"char_limit": 5}}}}}) == 200


def test_prompt_block(mem):
    slug = project(mem)
    assert "(no project notes yet)" in mem.render_block(slug)
    mem.add(slug, "Use tabs")
    block = mem.render_block(slug)
    assert "PROJECT MEMORY (github.com/alice/demo)" in block and "Use tabs" in block


def test_tool_flow(mem, monkeypatch):
    slug = project(mem)
    monkeypatch.setattr(mem, "project_for_session", lambda sid, live: (slug, ""))
    monkeypatch.setattr(mem, "children_blocked", lambda: False)
    out = json.loads(mem.handle({"action": "write", "operations": [{"action": "add", "content": "fact one"}]},
                                session_id="s1"))
    assert out["success"] and out["project"] == "github.com/alice/demo"
    read = json.loads(mem.handle({"action": "read"}, session_id="s1"))
    assert read["entries"] == ["fact one"]
    bad = json.loads(mem.handle({"action": "write", "operations": [{"action": "zap"}]}, session_id="s1"))
    assert not bad["success"]
    none = json.loads(mem.handle({"action": "delete"}, session_id="s1"))
    assert not none["success"]


def test_subagents_cannot_write(mem, monkeypatch):
    slug = project(mem)
    monkeypatch.setattr(mem, "project_for_session", lambda sid, live: (slug, ""))
    mem.note_child("child-1")
    out = json.loads(mem.handle({"action": "write", "operations": [{"action": "add", "content": "x"}]},
                                session_id="child-1"))
    assert not out["success"] and "Subagents may not" in out["error"]
    assert json.loads(mem.handle({"action": "read"}, session_id="child-1"))["success"]


def test_no_project_is_an_error(mem, monkeypatch):
    monkeypatch.setattr(mem, "project_for_session", lambda sid, live: (None, "not bound"))
    assert json.loads(mem.handle({"action": "read"}, session_id="s"))["error"] == "not bound"


def test_write_waits_for_approval(mem, monkeypatch):
    """With memory.write_approval on, a project write is staged, and the staged write applies on approval."""
    from tools import write_approval as wa

    slug = project(mem)
    monkeypatch.setattr(mem, "project_for_session", lambda sid, live: (slug, ""))
    monkeypatch.setattr(mem, "children_blocked", lambda: False)
    monkeypatch.setattr(mem, "_approval_on", lambda: True)
    staged = []

    class Decision:
        allow, blocked, message = False, False, "Staged for approval."

    monkeypatch.setattr(wa, "evaluate_gate", lambda *a, **k: Decision())
    monkeypatch.setattr(wa, "current_origin", lambda: "test")
    monkeypatch.setattr(wa, "stage_write", lambda kind, payload, **k: staged.append(payload) or {"id": "p1"})
    out = json.loads(mem.handle({"action": "write", "operations": [{"action": "add", "content": "later"}]},
                                session_id="s1"))
    assert out["staged"] and out["pending_id"] == "p1"
    assert mem.entries(slug) == []
    payload = staged[0]
    assert payload["target"] == "project" and payload["project"] == slug

    seen = {}

    def original(p, store):
        seen["target"] = p["target"]
        return store.apply_batch("memory", p["operations"])

    assert mem.apply_pending(payload, original)["success"]
    assert seen["target"] == "memory" and mem.entries(slug) == ["later"]
    gone = mem.apply_pending({**payload, "project": "missing"}, original)
    assert not gone["success"]

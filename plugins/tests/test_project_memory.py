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
    assert pm.char_limit({"memory": {"memory_char_limit": 8000}}) == 8000
    assert pm.char_limit({"memory": {"memory_char_limit": 100000}}) == pm.MAX_CHAR_LIMIT == 32000
    cfg = {"plugins": {"entries": {"project-memory": {"settings": {"char_limit": 500}}}}}
    assert pm.char_limit(cfg) == 500
    assert pm.char_limit({"plugins": {"entries": {"project-memory": {"settings": {"char_limit": 5}}}}}) == 200


def test_long_entry_under_global_limit(mem, monkeypatch):
    monkeypatch.setattr(mem, "_config", lambda: {"memory": {"memory_char_limit": 8000}})
    slug = project(mem)
    mem.add(slug, "x" * 6000)
    assert len(mem.entries(slug)[0]) == 6000
    assert mem.session_store("s1", slug).memory_char_limit == 8000
    monkeypatch.setattr(mem, "_config", lambda: {"memory": {"memory_char_limit": 9000}})
    assert mem.session_store("s1", slug).memory_char_limit == 9000  # a live session picks up a new limit
    block = mem.render_block(slug)
    assert "[66%, 6,000/9,000 chars]" in block and "1 more project notes are not shown" in block


def _notes_8000(mem, monkeypatch):
    monkeypatch.setattr(mem, "_config", lambda: {"memory": {"memory_char_limit": 8000}})
    slug = project(mem)
    for i in range(8):
        mem.add(slug, f"note {i} " + "x" * 989)  # 8 entries of 1,000 chars: 8,000 with the delimiters
    return slug


def test_8000_chars_of_notes_fit_when_hermes_grants_room(mem, monkeypatch):
    slug = _notes_8000(mem, monkeypatch)
    monkeypatch.setattr(mem, "_granted", 12000)
    block = mem.render_block(slug)
    assert "more project notes are not shown" not in block
    assert all(f"note {i} " in block for i in range(8))
    assert len(block) <= mem.section_budget(8000) < 12000


def test_notes_are_cut_at_what_hermes_grants(mem, monkeypatch):
    slug = _notes_8000(mem, monkeypatch)
    monkeypatch.setattr(mem, "_granted", 4000)
    block = mem.render_block(slug)
    assert len(block) <= 3990 and "more project notes are not shown" in block
    assert mem.section_budget(8000) == 3990


def test_request_section_chars_follows_the_hermes_ceiling(mem, monkeypatch, caplog):
    import sys
    import types
    monkeypatch.setattr(mem, "_config", lambda: {"memory": {"memory_char_limit": 8000}})
    fake = types.ModuleType("hermes_cli.plugins")
    fake.system_prompt_section_ceiling = lambda: 12000
    monkeypatch.setitem(sys.modules, "hermes_cli.plugins", fake)
    assert mem.request_section_chars() == 12000 and mem.section_budget() == 8990
    assert "plugins.max_section_chars" not in caplog.text
    fake.system_prompt_section_ceiling = lambda: 100000
    assert mem.request_section_chars() == 33000  # the largest note set plus the header
    fake.system_prompt_section_ceiling = lambda: 4000
    with caplog.at_level("WARNING", logger="project_memory"):
        assert mem.request_section_chars() == 4000
    assert "plugins.max_section_chars to 9000" in caplog.text
    del fake.system_prompt_section_ceiling  # a Hermes without the setting keeps 4,000
    assert mem.request_section_chars() == 4000 and mem.section_budget() == 3990


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


def _cfg(value):
    settings = {} if value is None else {"write_approval": value}
    return {"plugins": {"entries": {"project-memory": {"settings": settings}}}}


@pytest.mark.parametrize("setting,global_on,want", [
    (None, True, True), (None, False, False),
    (False, True, False), (True, False, True),
    (True, True, True), (False, False, False),
    ("no", True, True),
])
def test_approval_switch(pm, monkeypatch, setting, global_on, want):
    from tools import write_approval as wa
    monkeypatch.setattr(pm, "_config", lambda: _cfg(setting))
    monkeypatch.setattr(wa, "write_approval_enabled", lambda kind: global_on)
    assert pm._approval_on() is want


def _write_with(pm, hermes_home, monkeypatch, setting, global_on):
    """One real-gate project write, with a fake approval queue; returns (result, staged payloads, slug)."""
    from tools import write_approval as wa
    monkeypatch.setattr(pm, "_unattended_review", lambda: False)
    monkeypatch.setattr(pm, "_config", lambda: _cfg(setting))
    slug = project(pm)
    monkeypatch.setattr(pm, "project_for_session", lambda sid, live: (slug, ""))
    monkeypatch.setattr(pm, "children_blocked", lambda: False)
    monkeypatch.setattr(wa, "write_approval_enabled", lambda kind: global_on)

    class Decision:
        allow, blocked, message = False, False, "Staged for approval."

    staged = [{"id": "old", "target": "project"}]
    monkeypatch.setattr(wa, "evaluate_gate", lambda *a, **k: Decision())
    monkeypatch.setattr(wa, "current_origin", lambda: "test")
    monkeypatch.setattr(wa, "stage_write", lambda kind, payload, **k: staged.append(payload) or {"id": "new"})
    out = json.loads(pm.handle({"action": "write", "operations": [{"action": "add", "content": "now"}]},
                               session_id=f"sw-{hermes_home.parent.name}"))
    return out, staged, slug


def test_false_saves_directly_with_global_on(pm, hermes_home, monkeypatch):
    out, staged, slug = _write_with(pm, hermes_home, monkeypatch, False, True)
    assert out["success"] and not out.get("staged")
    assert pm.entries(slug) == ["now"]
    assert staged == [{"id": "old", "target": "project"}]  # the already staged entry is untouched


def test_true_stages_with_global_off(pm, hermes_home, monkeypatch):
    out, staged, slug = _write_with(pm, hermes_home, monkeypatch, True, False)
    assert out["staged"] and pm.entries(slug) == []
    assert len(staged) == 2 and staged[0]["id"] == "old"


def test_unset_follows_global(pm, hermes_home, monkeypatch):
    out, _, slug = _write_with(pm, hermes_home, monkeypatch, None, True)
    assert out["staged"] and pm.entries(slug) == []


def test_unset_global_off_saves(pm, hermes_home, monkeypatch):
    out, _, slug = _write_with(pm, hermes_home, monkeypatch, None, False)
    assert out["success"] and pm.entries(slug) == ["now"]

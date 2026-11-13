import pytest

CONFIG = {
    "backends": {
        "model_map": {
            "default": "mid",
            "guidance": {
                "picker": "cheap routes for volume, strong ones for hard work",
                "note": "cheap for volume work, strong for hard work",
                "description": "Prefer cheap routes for volume work.",
            },
            "options": [
                {"name": "cheap", "title": "Cheap One", "description": "Fast.", "provider": "acme", "model": "m-1"},
                {"name": "mid", "title": "Mid", "description": "Normal.", "provider": "acme", "model": "m-2",
                 "via": "the Acme pool"},
                {"name": "strong", "title": "Strong", "description": "Hard.", "provider": "acme", "model": "m-3",
                 "helper": True},
                {"name": "local", "title": "Local box", "description": "Free.", "base_url": "http://x/v1",
                 "model": "q", "line": "Every subagent runs on q on the lab GPU.", "emphasis": "q"},
                {"name": "paid", "title": "Paid", "description": "Pay per token.", "provider": "other",
                 "model": "p", "auto": False},
            ],
        }
    }
}


def test_options_keep_order_and_skip_bad_entries(routes):
    cfg = {"backends": {"model_map": {"options": [
        {"name": "a", "description": "d", "model": "m", "provider": "p"},
        {"name": "a", "description": "dup", "model": "m", "provider": "p"},
        {"name": "auto", "description": "reserved", "model": "m", "provider": "p"},
        {"name": "help-x", "description": "reserved", "model": "m", "provider": "p"},
        {"name": "nomodel", "description": "d", "provider": "p"},
        {"name": "noroute", "description": "d", "model": "m"},
        "junk",
        {"name": "b", "description": "d", "model": "m", "base_url": "http://x"},
    ]}}}
    assert list(routes.options(cfg)) == ["a", "b"]
    assert routes.options({}) == {}
    assert routes.options(None) == {}


def test_route_fields_are_passed_through(routes):
    opts = routes.options(CONFIG)
    assert opts["cheap"]["route"] == {"provider": "acme", "model": "m-1"}
    assert opts["paid"]["auto"] is False
    assert opts["strong"]["helper"] is True


def test_default_route_must_exist(routes):
    opts = routes.options(CONFIG)
    assert routes.default_route(CONFIG, opts) == "mid"
    bad = {"backends": {"model_map": {"default": "nope", "options": CONFIG["backends"]["model_map"]["options"]}}}
    assert routes.default_route(bad, routes.options(bad)) == ""


def test_valid_choice(routes):
    opts = routes.options(CONFIG)
    for good in ("auto", "none", "cheap", "help-strong"):
        assert routes.valid_choice(good, opts)
    for bad in ("help-cheap", "help-nope", "nope", ""):
        assert not routes.valid_choice(bad, opts)


def test_choices_use_config_texts(routes):
    opts = routes.options(CONFIG)
    rows = {r["id"]: r for r in routes.choices(opts, routes.guidance(CONFIG))}
    assert rows["auto"]["line"] == ("The model picks a route for each subagent: "
                                    "cheap routes for volume, strong ones for hard work.")
    assert rows["cheap"]["line"] == "Every subagent runs on Cheap One, through acme."
    assert rows["mid"]["line"] == "Every subagent runs on Mid, through the Acme pool."
    assert rows["local"]["line"] == "Every subagent runs on q on the lab GPU."
    assert rows["local"]["em"] == "q"
    assert rows["help-strong"]["label"] == "Strong when stuck"
    assert "help-cheap" not in rows
    ids = list(rows)
    assert ids[0] == "auto" and ids[-1] == "none"


def test_choices_without_guidance_are_generic(routes):
    opts = routes.options(CONFIG)
    rows = routes.choices(opts)
    assert rows[0]["line"] == "The model picks a route for each subagent: from the route descriptions."
    plain = routes.options({"backends": {"model_map": {"options": [
        {"name": "x", "title": "X", "description": "d", "model": "m", "base_url": "http://x"}]}}})
    assert routes.choices(plain)[1]["line"] == "Every subagent runs on X."


def test_note_texts(routes):
    opts = routes.options(CONFIG)
    g = routes.guidance(CONFIG)
    assert routes.note("none", opts).endswith("do the work yourself.]")
    assert "route `cheap`" in routes.note("cheap", opts)
    assert "option `strong` (Strong)" in routes.note("help-strong", opts)
    assert routes.note("auto", opts, g).endswith("`option`: cheap for volume work, strong for hard work.]")
    assert routes.note("auto", opts).endswith("`option`: by the route descriptions.]")


def test_guidance_missing_is_empty(routes):
    assert routes.guidance({}) == {"picker": "", "note": "", "description": ""}
    assert routes.guidance(CONFIG)["description"] == "Prefer cheap routes for volume work."


@pytest.fixture
def store(routes, hermes_home, monkeypatch):
    # the session database is Hermes's; without one every session is its own tip with no parents
    monkeypatch.setattr(routes, "tip", lambda sid: sid)
    monkeypatch.setattr(routes, "lineage", lambda sid: [sid] if sid else [])
    routes._cache.update(key=None, data={})
    return routes


def test_choice_roundtrip_and_forget(store):
    assert store.stored_choice("s1") == (None, ["s1"])
    store.set_choice("s1", "cheap")
    assert store.stored_choice("s1")[0] == "cheap"
    path = store.store_path()
    assert path.parent.name == "subagent-routes" and oct(path.stat().st_mode & 0o777) == "0o600"
    store.forget("s1")
    assert store.stored_choice("s1")[0] is None


def test_choice_follows_compression_lineage(store, monkeypatch):
    monkeypatch.setattr(store, "tip", lambda sid: "child")
    monkeypatch.setattr(store, "lineage", lambda sid: ["child", "parent"] if sid == "child" else [sid])
    store.set_choice("parent", "mid")
    assert store.stored_choice("child")[0] == "mid"


def test_choice_for_falls_back_to_auto(store):
    opts = store.options(CONFIG)
    store.set_choice("s2", "gone-route")
    assert store.choice_for("s2", opts) == "auto"
    store.set_choice("s2", "strong")
    assert store.choice_for("s2", opts) == "strong"


def test_unreadable_store_means_auto(store):
    path = store.store_path()
    path.parent.mkdir(parents=True)
    path.write_text("{not json")
    assert store.stored_choice("s3")[0] is None

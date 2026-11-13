"use strict";
// HubUsers is the Users tab of Settings, for admins: who may log in to the
// hub, which backends each person may use, and the actions on a person.
// ctx gives it the app's api and el helpers, the /api/me answer, and the
// pane's element. What a row says and which actions the hub refuses are
// worked out in authrules.js.
window.HubUsers = (ctx) => {
  const { api, el } = ctx;
  const A = window.HubAuth;
  const pane = () => document.getElementById("set-users");
  const fmt = {
    day: (s) => new Date(s * 1000).toLocaleDateString(undefined, { month: "short", day: "numeric" }),
    time: (s) => new Date(s * 1000).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" }),
  };
  const icons = {
    info: "M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18zM12 11v5M12 8h.01",
    warn: "M12 3l9 16H3zM12 10v4M12 17h.01",
  };

  let data = null;
  let open = {};
  let msg = {};
  let confirming = "";
  let adding = null;
  let err = "";
  let busy = false;
  let focus = "";

  const local = () => data && data.mode === "local";

  function button(cls, text, fn) {
    const b = el("button", cls, text);
    b.type = "button";
    b.disabled = busy;
    b.onclick = fn;
    return b;
  }

  const svg = (path) => {
    const s = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    s.setAttribute("viewBox", "0 0 24 24");
    s.setAttribute("aria-hidden", "true");
    const p = document.createElementNS("http://www.w3.org/2000/svg", "path");
    p.setAttribute("d", path);
    s.append(p);
    return s;
  };

  async function load() {
    try {
      data = await api("GET", "/api/admin/users");
      err = "";
    } catch (e) {
      err = "Could not load the people: " + e.message;
    }
    draw();
  }

  // run does one change, then reloads the list. A refusal from the hub
  // shows under the person it was for.
  async function run(name, call, ok) {
    busy = true;
    msg[name] = null;
    draw();
    try {
      await call();
      if (ok) msg[name] = { ok: true, text: ok };
    } catch (e) {
      msg[name] = { ok: false, text: A.sentence(e.message) };
    }
    busy = false;
    try { data = await api("GET", "/api/admin/users"); } catch (e) { err = "Could not load the people: " + e.message; }
    draw();
  }

  const post = (name, body) => api("POST", "/api/admin/users/" + encodeURIComponent(name), body);

  function note(name) {
    const m = msg[name];
    if (!m) return null;
    const p = el("p", m.ok ? "ok" : "err", m.text);
    p.setAttribute("role", m.ok ? "status" : "alert");
    return p;
  }

  // backends is the checklist of what a person may use. A backend the
  // config gives them shows checked and locked, with the reason.
  // onChange gets the ids an admin picked.
  function backends(u, checked, onChange) {
    const box = el("div", "checks");
    box.setAttribute("role", "group");
    box.setAttribute("aria-label", "Backends");
    for (const b of data.backends) {
      const why = A.lock(b, u);
      const l = el("label", "check");
      const c = el("input");
      c.type = "checkbox";
      c.checked = !!why || checked.includes(b.id);
      c.disabled = !!why || busy || u.source === "config";
      c.onchange = () => onChange(data.backends.filter((x) => !A.lock(x, u) && (x.id === b.id ? c.checked : checked.includes(x.id))).map((x) => x.id));
      l.append(c, document.createTextNode(b.name || b.id));
      const reason = why || ((b.groups || []).length ? "also by group " + b.groups.join(", ") + " (config)" : "");
      if (reason) l.append(el("span", "why", reason));
      box.append(l);
    }
    return box;
  }

  function toggle(label, hint, on, disabled, fn) {
    const l = el("label", "switch");
    const g = el("span", "grow", label);
    g.append(el("small", "", hint));
    const t = el("input", "toggle");
    t.type = "checkbox";
    t.setAttribute("role", "switch");
    t.checked = on;
    t.disabled = disabled || busy;
    t.onchange = () => fn(t.checked);
    l.append(g, t);
    return l;
  }

  function secretInput(placeholder, label) {
    const w = el("div", "pw");
    const i = el("input");
    Object.assign(i, { type: "password", placeholder, autocomplete: "new-password", autocapitalize: "off", spellcheck: false });
    i.setAttribute("aria-label", label);
    const s = el("button", "ghost", "Show");
    s.type = "button";
    s.onclick = () => {
      const on = i.type === "password";
      i.type = on ? "text" : "password";
      s.textContent = on ? "Hide" : "Show";
    };
    w.append(i, s);
    return { wrap: w, input: i };
  }

  function detail(u, r, g) {
    const box = el("div", "set-detail");
    box.id = "user-" + u.name;
    const who = u.display || u.name;
    if (u.source === "config") {
      const dl = el("dl", "kv");
      dl.append(el("dt", "", "Set in"), el("dd", "", "users[] in the config"), el("dt", "", "Identity"), el("dd", "", u.identity.replace(/^[a-z]+:/, "")));
      box.append(dl, el("h3", "", "Backends"), backends(u, [], () => {}), el("p", "hint", "This person comes from the config. Change the person and their backends there."));
      return box;
    }
    if (r.waiting) {
      const dl = el("dl", "kv");
      const dd = el("dd", "", "The first login whose user is ");
      dd.append(el("code", "", u.name));
      if (u.email) {
        dd.append(document.createTextNode(data.mode === "oidc" ? " or whose verified email is " : " or whose email is "), el("code", "", u.email));
      }
      dl.append(el("dt", "", "Matches"), dd);
      box.append(dl);
    }
    box.append(el("h3", "", "Backends"),
      backends(u, u.backends, (ids) => run(u.name, () => post(u.name, { backends: ids }), "Saved.")));
    box.append(toggle("Admin", "Can add and change people", u.admin, !!g.demote && u.admin,
      (on) => run(u.name, () => post(u.name, { admin: on }), "Saved.")));
    if (u.admin && g.demote) box.append(el("p", "hint", g.demote + "."));
    if (local()) {
      box.append(el("h3", "", "Password"));
      const acts = el("div", "acts");
      const pw = secretInput("New password", "New password for " + u.name);
      const set = button("", "Set password", async () => {
        const bad = A.passwordProblem(pw.input.value, u.name);
        if (bad) { msg[u.name] = { ok: false, text: A.sentence(bad) }; draw(); return; }
        await run(u.name, () => api("POST", "/api/admin/users/" + encodeURIComponent(u.name) + "/password", { password: pw.input.value }),
          "Saved. " + who + " is logged out everywhere.");
      });
      pw.input.onkeydown = (e) => { if (e.key === "Enter") { e.preventDefault(); set.click(); } };
      acts.append(pw.wrap, set);
      box.append(acts, el("p", "hint", "Setting a password also unlocks the account and logs " + who + " out everywhere."));
    }
    const m = note(u.name);
    if (m) box.append(m);
    if (confirming === u.name) {
      const c = el("div", "confirm");
      c.setAttribute("role", "alertdialog");
      c.setAttribute("aria-label", "Delete " + u.name);
      c.append(el("p", "", "Delete " + u.name + "? Their chats stay in Hermes; their drafts, saved prompts and Telegram link on the hub go."),
        button("danger", "Delete", () => { confirming = ""; run(u.name, () => api("POST", "/api/admin/users/" + encodeURIComponent(u.name) + "/delete"), ""); }),
        button("", "Cancel", () => { confirming = ""; focus = "del-" + u.name; draw(); }));
      box.append(c);
      return box;
    }
    const acts = el("div", "acts");
    const dis = button("", u.disabled ? "Enable" : "Disable", () => run(u.name, () => post(u.name, { disabled: !u.disabled }), "Saved."));
    dis.disabled = busy || (!u.disabled && !!g.disable);
    if (!u.disabled && g.disable) dis.title = g.disable;
    const del = button("ghost danger-text", "Delete", () => { confirming = u.name; focus = "confirm-" + u.name; draw(); });
    del.id = "del-" + u.name;
    del.disabled = busy || !!g.remove;
    if (g.remove) del.title = g.remove;
    acts.append(dis, del);
    box.append(acts);
    const why = [];
    if (!u.disabled && g.disable) why.push(g.disable);
    if (g.remove && !why.includes(g.remove)) why.push(g.remove);
    if (why.length) box.append(el("p", "hint", why.join(". ") + "."));
    if (u.name === data.me && local()) box.append(el("p", "hint", "Change your own password in Account."));
    return box;
  }

  function personRow(u) {
    const g = A.guards(data.me, u, data.users);
    const r = A.row(u, { me: data.me, mode: data.mode, backends: data.backends, fmt });
    const row = el("div", "set-row");
    const head = el("button", "set-rowhead", "");
    head.type = "button";
    head.id = "urow-" + u.name;
    head.setAttribute("aria-expanded", String(!!open[u.name]));
    head.setAttribute("aria-controls", "user-" + u.name);
    const st = el("span", "st " + r.state);
    st.setAttribute("aria-hidden", "true");
    const top = el("span", "set-top");
    const nm = el("span", "nm", u.name + " ");
    nm.title = u.name;
    if (r.display) nm.append(el("small", "", r.display));
    top.append(st, nm);
    for (const [cls, text] of r.pills) top.append(el("span", "pill " + (cls || "quiet"), text));
    const meta = el("span", "meta");
    for (const m of r.meta) {
      const s = el("span", "", m);
      s.title = m;
      meta.append(s);
    }
    head.append(top, meta);
    head.onclick = () => { open[u.name] = !open[u.name]; if (!open[u.name] && confirming === u.name) confirming = ""; focus = "urow-" + u.name; draw(); };
    row.append(head);
    if (open[u.name]) row.append(detail(u, r, g));
    return row;
  }

  function addForm() {
    const f = el("form", "addform");
    const d = adding;
    const name = el("input");
    Object.assign(name, { type: "text", value: d.name, autocomplete: "off", autocapitalize: "off", spellcheck: false, required: true, maxLength: 64 });
    name.id = "add-name";
    const display = el("input");
    Object.assign(display, { type: "text", value: d.display, autocomplete: "off", maxLength: 100 });
    const g = el("div", "grid2");
    const ln = el("label", "field", "");
    ln.append(el("span", "", "Name"), name);
    const ld = el("label", "field", "");
    ld.append(el("span", "", "Display name"), display);
    g.append(ln, ld);
    f.append(g);
    const shown = () => (d.display.trim() || d.name.trim() || "");
    let pw = null, email = null, addBtn = null, pwNote = null;
    const pwHint = () => { if (pwNote) pwNote.textContent = "At least " + A.MIN + " characters. " + (shown() || "They") + " can change it after logging in."; };
    if (local()) {
      pw = secretInput("", "First password");
      pw.input.value = d.password;
      const l = el("label", "field", "");
      pwNote = el("small", "", "");
      l.append(el("span", "", "First password"), pw.wrap, pwNote);
      f.append(l);
    } else {
      email = el("input");
      Object.assign(email, { type: "email", value: d.email, autocomplete: "off", autocapitalize: "off", spellcheck: false, maxLength: 254 });
      const l = el("label", "field", "");
      l.append(el("span", "", "Email (optional)"), email, el("small", "", "The account waits for the first login with this user name or this email. Then it is theirs."));
      f.append(l);
    }
    f.append(el("h3", "", "Backends"));
    const checks = el("div", "checks");
    for (const b of data.backends) {
      const why = A.lock(b, { name: d.name.trim().toLowerCase(), groups: [] });
      const l = el("label", "check");
      const c = el("input");
      c.type = "checkbox";
      c.checked = !!why || d.backends.includes(b.id);
      c.disabled = !!why;
      c.onchange = () => { d.backends = c.checked ? [...d.backends, b.id] : d.backends.filter((x) => x !== b.id); };
      l.append(c, document.createTextNode(b.name || b.id));
      const reason = why || ((b.groups || []).length ? "also by group " + b.groups.join(", ") + " (config)" : "");
      if (reason) l.append(el("span", "why", reason));
      checks.append(l);
    }
    pwHint();
    f.append(checks, toggle("Admin", "Can add and change people", d.admin, false, (on) => { d.admin = on; }));
    const e = el("p", "err", d.err);
    e.hidden = !d.err;
    e.setAttribute("role", "alert");
    addBtn = el("button", "primary", "Add " + (shown() || "person"));
    addBtn.type = "submit";
    addBtn.disabled = busy;
    const cancel = button("", "Cancel", () => { adding = null; focus = "add-open"; draw(); });
    const acts = el("div", "acts");
    acts.append(addBtn, cancel);
    f.append(e, acts);
    const keep = () => {
      d.name = name.value; d.display = display.value;
      if (pw) d.password = pw.input.value;
      if (email) d.email = email.value;
    };
    for (const i of [name, display, pw && pw.input, email].filter(Boolean)) i.oninput = () => { keep(); addBtn.textContent = "Add " + (shown() || "person"); pwHint(); };
    f.onsubmit = async (ev) => {
      ev.preventDefault();
      keep();
      d.err = "";
      const nm = A.cleanName(d.name);
      d.err = A.nameProblem(nm) || (local() ? A.passwordProblem(d.password, nm) : "");
      if (d.err) { d.err = A.sentence(d.err); draw(); return; }
      busy = true;
      draw();
      try {
        const body = { name: nm, display: d.display.trim(), admin: d.admin, backends: d.backends };
        if (local()) body.password = d.password;
        else if (d.email.trim()) body.email = d.email.trim();
        await api("POST", "/api/admin/users", body);
        open[nm] = true;
        msg[nm] = { ok: true, text: "Added " + (d.display.trim() || nm) + "." };
        adding = null;
        focus = "urow-" + nm;
      } catch (x) {
        d.err = A.sentence(x.message);
      }
      busy = false;
      try { data = await api("GET", "/api/admin/users"); } catch (x) { err = "Could not load the people: " + x.message; }
      draw();
    };
    return f;
  }

  function draw() {
    const p = pane();
    if (!p) return;
    const hadFocus = p.contains(document.activeElement);
    p.replaceChildren();
    if (err) p.append(el("p", "err", err));
    if (!data) return;
    const b = A.banner(data.mode, data.login);
    if (b) {
      const box = el("div", "set-banner" + (b.kind === "warn" ? " warn" : ""));
      const text = el("div", "");
      for (const part of b.parts) text.append(typeof part === "string" ? document.createTextNode(part) : el("code", "", part.code));
      box.append(svg(icons[b.kind]), text);
      p.append(box);
    }
    const head = el("div", "acts");
    head.append(el("h2", "grow", "People"));
    if (!adding) {
      const add = button("primary", "Add a person", () => {
        adding = { name: "", display: "", password: "", email: "", admin: false, backends: [], err: "" };
        draw();
        document.getElementById("add-name")?.focus();
      });
      add.id = "add-open";
      head.append(add);
    }
    p.append(head);
    if (local() && !adding) p.append(el("p", "hint", "People log in to this hub with a name and password. Pick which Hermes backends each person may use."));
    if (adding) p.append(addForm());
    const rows = el("div", "set-rows");
    for (const u of data.users) rows.append(personRow(u));
    if (rows.children.length) p.append(rows);
    else p.append(el("p", "none", "Nobody yet."));
    if (hadFocus && focus) {
      const t = document.getElementById(focus) || p.querySelector(".confirm button");
      if (t) t.focus();
    }
    focus = "";
  }

  // reset forgets what the last visit left open.
  function reset() {
    open = {};
    msg = {};
    confirming = "";
    adding = null;
    err = "";
  }

  return { load, draw, reset };
};

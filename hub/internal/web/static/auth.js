"use strict";
// The login page (/login) and the first-user page (/setup). Both ask
// /auth/info how the hub logs people in, so they need nothing from the
// server's HTML. The rules are in authrules.js.
(() => {
  const A = window.HubAuth;
  const page = document.body.dataset.page;
  const box = document.getElementById("box");
  const q = new URLSearchParams(location.search);
  const next = A.cleanNext(q.get("next") || "/");

  const el = (tag, cls, text) => {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined && text !== null) e.textContent = text;
    return e;
  };

  // field is a label with an input and, optionally, a small note.
  function field(label, attrs, note) {
    const l = el("label", "field");
    const input = el("input");
    Object.assign(input, { type: "text", autocapitalize: "off", spellcheck: false }, attrs);
    l.append(el("span", "", label), input);
    if (note) l.append(el("small", "", note));
    return { label: l, input };
  }

  // secret is a password input with a Show button.
  function secret(label, attrs, extra) {
    const l = el("label", "field");
    const wrap = el("div", "pw");
    const input = el("input");
    Object.assign(input, { type: "password", autocapitalize: "off", spellcheck: false }, attrs);
    const show = el("button", "ghost", "Show");
    show.type = "button";
    show.setAttribute("aria-pressed", "false");
    show.onclick = () => {
      const on = input.type === "password";
      input.type = on ? "text" : "password";
      show.textContent = on ? "Hide" : "Show";
      show.setAttribute("aria-pressed", String(on));
    };
    wrap.append(input, show);
    l.append(el("span", "", label), wrap);
    for (const x of extra || []) l.append(x);
    return { label: l, input };
  }

  // alertBox is an error message that screen readers read out when it fills.
  function alertBox() {
    const e = el("div", "err");
    e.setAttribute("role", "alert");
    e.hidden = true;
    e.set = (msg) => { e.textContent = msg ? A.sentence(msg) : ""; e.hidden = !msg; };
    return e;
  }

  function submitButton(text) {
    const b = el("button", "primary", text);
    b.type = "submit";
    return b;
  }

  async function post(path, body) {
    let r;
    try {
      r = await fetch(path, { method: "POST", credentials: "same-origin", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
    } catch (_) {
      return { status: 0, error: "Could not reach the hub. Check your connection and try again" };
    }
    let data = {};
    try { data = await r.json(); } catch (_) { /* no body */ }
    return { status: r.status, data, error: data.error || r.statusText };
  }

  function loginForm() {
    const form = el("form", "card");
    form.noValidate = true;
    const bad = alertBox(), many = alertBox();
    const name = field("Name", { autocomplete: "username", required: true, name: "username" });
    const pw = secret("Password", { autocomplete: "current-password", required: true, name: "password" });
    const acts = el("div", "acts");
    const go = submitButton("Log in");
    acts.append(go);
    form.append(el("h1", "", "Log in"), bad, name.label, pw.label, acts, many,
      el("p", "hint", "Forgot your password? Ask an admin of this hub to set a new one."));
    form.onsubmit = async (e) => {
      e.preventDefault();
      bad.set(""); many.set("");
      if (!name.input.value.trim() || !pw.input.value) {
        bad.set("Type your name and password");
        (name.input.value.trim() ? pw.input : name.input).focus();
        return;
      }
      go.disabled = true;
      const r = await post("/auth/password", { name: name.input.value, password: pw.input.value, next });
      go.disabled = false;
      if (r.status === 200) {
        location.href = A.cleanNext(r.data.next);
        return;
      }
      pw.input.value = "";
      if (r.status === 429) many.set(r.error);
      else bad.set(r.error);
      pw.input.focus();
    };
    return { node: form, focus: () => name.input.focus() };
  }

  function setupForm() {
    const form = el("form", "card");
    form.noValidate = true;
    const err = alertBox();
    const name = field("Your name", { autocomplete: "username", required: true, name: "username", maxLength: 64 }, "Lowercase letters, digits, dots, dashes. You log in with it.");
    const display = field("Display name (optional)", { autocomplete: "name", maxLength: 100 });
    const meter = el("div", "meter");
    const bar = el("i");
    meter.append(bar);
    const pw = secret("Password", { autocomplete: "new-password", required: true, name: "password" },
      [meter, el("small", "", "At least " + A.MIN + " characters. A few words are easy to type on a phone.")]);
    const again = field("Password again", { type: "password", autocomplete: "new-password", required: true });
    const go = submitButton("Make the account");
    const acts = el("div", "acts");
    acts.append(go);
    form.append(el("h1", "", "Set up this hub"),
      el("p", "hint", "Make the first account. It is an admin: it can add other people later in Settings > Users."),
      err, name.label, display.label, pw.label, again.label, acts);
    const draw = () => {
      const s = A.strength(pw.input.value);
      bar.style.width = s.pct + "%";
      bar.className = s.level;
    };
    draw();
    pw.input.oninput = draw;
    form.onsubmit = async (e) => {
      e.preventDefault();
      err.set("");
      const problem = A.nameProblem(name.input.value);
      if (problem) { err.set(problem); name.input.focus(); return; }
      const weak = A.passwordProblem(pw.input.value, name.input.value);
      if (weak) { err.set(weak); pw.input.focus(); return; }
      if (pw.input.value !== again.input.value) { err.set("The two passwords are not the same"); again.input.focus(); return; }
      go.disabled = true;
      const r = await post("/auth/setup", { name: name.input.value, display: display.input.value.trim(), password: pw.input.value });
      go.disabled = false;
      if (r.status === 201) { location.href = "/"; return; }
      if (r.status === 409) { done(); return; }
      err.set(r.error);
    };
    return { node: form, focus: () => name.input.focus() };
  }

  // done is the setup page of a hub that has its first account.
  function done() {
    const card = el("div", "card");
    const go = el("a", "button primary", "Go to login");
    go.href = "/login";
    const acts = el("div", "acts");
    acts.append(go);
    card.append(el("h1", "", "This hub is set up"), el("p", "hint", "It already has an account. Log in, or ask its admin to add you."), acts);
    box.replaceChildren(card);
    go.focus();
  }

  async function start() {
    let info = { mode: "local", setup: false };
    try {
      const r = await fetch("/auth/info", { credentials: "same-origin" });
      if (r.ok) info = await r.json();
    } catch (_) { /* show the form anyway */ }
    if (info.mode !== "local") {
      location.replace(page === "login" ? "/auth/login?next=" + encodeURIComponent(next) : "/");
      return;
    }
    if (page === "login") {
      if (info.setup) { location.replace("/setup"); return; }
      const f = loginForm();
      box.replaceChildren(f.node, el("p", "foot", "Talaria hub"));
      f.focus();
      return;
    }
    if (!info.setup) { done(); return; }
    const f = setupForm();
    box.replaceChildren(f.node);
    f.focus();
  }

  start();
})();

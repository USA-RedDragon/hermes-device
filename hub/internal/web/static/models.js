"use strict";
// HubModels is "Connect a model": the first-run card in the chat area when
// Hermes has no model, the Connect a model dialog (a server of one's own,
// an API key, or a device-code sign-in), and Settings > Models. Everything
// goes through the hub's /api/models/<backend>/... routes, which call
// Hermes's own setup routes; Hermes keeps the providers, keys and default.
// Messages from Hermes are shown as Hermes wrote them; the hub adds words
// only for the localhost hint, a key Hermes cannot check, a model window
// under Hermes's minimum, and a Hermes it cannot reach.
// The pure helpers (isLocal, serverVerdict, keyVerdict, signInState,
// minutesLeft, windowWarning, findProviders, next) run against
// internal/web/testdata/models/run.js.
// ctx: api, el, me() (the /api/me answer), backend() (the backend to set
// up), toast(text), connected() (called after a model was connected or
// the default changed), startChat().
window.HubModels = (() => {
  const minWindow = 64000;
  const docs = "https://usa-reddragon.github.io/talaria/deploy/models.html";

  // serverCommand is where and what to run for the agent's own model setup, for the install type that the
  // hub's ui.install names (compose, helm, aio, unraid, truenas); a generic container form for any other.
  function serverCommand(install) {
    return ({
      compose: ["In the folder with compose.yaml, run:", "docker compose exec -u hermes hermes hermes model"],
      helm: ["Run, with your namespace and release name:", "kubectl -n <namespace> exec -it deploy/<release>-hermes -c gateway -- hermes model"],
      aio: ["Run:", "docker exec -it -u hermes talaria hermes model"],
      unraid: ["In Unraid's terminal, run:", "docker exec -it -u hermes Talaria hermes model"],
      truenas: ["In the TrueNAS shell, run:", "sudo docker exec -it -u hermes $(sudo docker ps -qf name=talaria) hermes model"],
    })[install] || ["Run, with the name of Talaria's agent container:", "docker exec -it -u hermes <container> hermes model"];
  }

  // isLocal reports an address that names the Hermes container itself.
  function isLocal(address) {
    let host = "";
    try {
      host = new URL(String(address || "").trim()).hostname;
    } catch (_) {
      return false;
    }
    host = host.replace(/^\[|\]$/g, "");
    return host === "localhost" || host === "0.0.0.0" || host === "::1" || /^127\./.test(host);
  }

  // split cuts a message after its first sentence, for a bold lead.
  function split(msg) {
    const m = String(msg || "");
    const cut = m.search(/\.\s/);
    return cut > 0 && cut < m.length - 2 ? [m.slice(0, cut + 1), m.slice(cut + 2)] : [m, ""];
  }

  // serverVerdict reads Hermes's answer to an address test. tone is good,
  // warn or bad; title and text are Hermes's message split at its first
  // sentence; hint is Talaria's own localhost note.
  function serverVerdict(res, address) {
    const r = res || {};
    const models = r.models || [];
    const base = r.resolved_base_url || String(address || "").trim();
    if (r.ok) return { tone: "good", title: "Found " + models.length + (models.length === 1 ? " model" : " models"), at: base, models, base };
    const msg = String(r.message || "");
    const [title, text] = split(msg);
    const out = { tone: "bad", title, text, hint: "", models, base, needMode: false, noModels: false };
    if (!r.reachable && isLocal(address)) {
      out.hint = "Hermes runs in its own container, so localhost is that container, not your computer. Use the address of the machine that runs the server, " +
        "such as http://192.168.1.10:11434/v1. Ollama must listen on that address: set OLLAMA_HOST=0.0.0.0:11434 there.";
    }
    if (/Pick the API mode/.test(msg)) out.needMode = true;
    else if (r.reachable && !models.length && !/rejected|HTTP \d/.test(msg)) {
      out.tone = "warn";
      out.noModels = true;
    }
    return out;
  }

  // keyVerdict reads Hermes's answer to a key check: accepted (the
  // provider took it), unchecked (Hermes has no check for this provider),
  // offline (the provider could not be asked; saving is still allowed), or
  // rejected (Hermes's words; nothing is saved).
  function keyVerdict(res, name) {
    const r = res || {};
    if (r.ok && r.reachable) return { kind: "accepted", save: true, tone: "good", title: name + " accepted the key." };
    if (r.ok) {
      return { kind: "unchecked", save: true, tone: "warn", title: name + " key saved, but not checked.",
        text: "Hermes can check keys for OpenRouter, OpenAI, xAI and Google AI Studio before it saves them, and not yet for this provider. " +
          "If the first reply fails with an authentication error, the key is wrong: edit it in Settings, Models." };
    }
    if (!r.reachable) return { kind: "offline", save: true, tone: "warn", title: r.message || "Could not check the key.", text: "Hermes saved it anyway." };
    const [title, lead] = split(r.message || "The key was not accepted.");
    return { kind: "rejected", save: false, tone: "bad", title, lead, text: "Nothing was saved." };
  }

  // signInState reads a sign-in poll: approved, pending, expired, or failed
  // with Hermes's message.
  function signInState(res) {
    const r = res || {};
    if (r.status === "approved") return { kind: "approved" };
    if (r.status === "pending") return { kind: "pending" };
    if (r.status === "expired" || r.status === "cancelled") return { kind: "expired" };
    const msg = r.error_message || (r.status === "denied" ? "The sign-in was turned down." : "The sign-in did not finish.");
    return { kind: "failed", message: msg };
  }

  // minutesLeft is how many whole minutes a code still works.
  function minutesLeft(until, now) {
    return Math.max(0, Math.ceil((until - now) / 60000));
  }

  // windowWarning is the warning for a default model whose window is under
  // Hermes's minimum, or null.
  function windowWarning(info) {
    const n = info && info.effective_context_length;
    if (!n || n >= minWindow) return null;
    return { title: info.model + " has a window of " + n.toLocaleString("en-US") + " tokens.",
      text: "Hermes needs at least 64,000, so chats with it will fail or lose context early." };
  }

  // findProviders filters the API-key providers by a search.
  function findProviders(list, query) {
    const q = String(query || "").trim().toLowerCase();
    return (list || []).filter((p) => !q || p.name.toLowerCase().includes(q) || p.provider.includes(q));
  }

  // next is the dialog's flow: from a state and an event, the next state.
  // Steps: kind, server, key, signin, pick, confirm, window, done.
  function next(st, ev) {
    const s = Object.assign({}, st);
    switch (ev.type) {
      case "kind": return { step: ev.kind };
      case "back": return { step: "kind" };
      case "tested": return Object.assign(s, { test: ev.verdict, models: ev.verdict.models || [], model: s.model || (ev.verdict.models || [])[0] || "" });
      case "saved-server": return ev.warning ? Object.assign(s, { step: "window", warning: ev.warning }) : Object.assign(s, { step: "done" });
      // With ev.pinned (the deployment sets the default) there is no
      // default to pick.
      case "key-checked":
        if (!ev.verdict.save) return Object.assign(s, { verdict: ev.verdict });
        return Object.assign(s, { step: ev.pinned ? "done" : "pick", verdict: ev.verdict, provider: ev.provider, name: ev.name });
      case "code": return Object.assign(s, { code: ev.code, signin: "pending" });
      case "polled":
        if (ev.state.kind === "approved") {
          return Object.assign(s, { step: ev.pinned ? "done" : "pick", provider: ev.provider, name: ev.name,
            verdict: { tone: "good", title: "Signed in to " + ev.name + "." } });
        }
        if (ev.state.kind === "pending") return s;
        return Object.assign(s, { signin: ev.state.kind, message: ev.state.message || "" });
      case "confirm": return Object.assign(s, { step: "confirm", confirm: ev.message, model: ev.model });
      case "default-set": return Object.assign(s, { step: "done", model: ev.model, skipped: !!ev.skipped });
      // Hermes kept its default: say so where the pick was made.
      case "default-kept": return Object.assign(s, { step: "pick", kept: ev.message });
      default: return s;
    }
  }

  function create(ctx) {
    const { api, el } = ctx;
    const I = window.HubIcons;
    const path = (b, rest) => "/api/models/" + encodeURIComponent(b) + rest;

    function btn(cls, text, fn, icon) {
      const b = el("button", cls, "");
      b.type = "button";
      if (icon) b.append(I.icon(icon));
      b.append(text);
      if (fn) b.onclick = fn;
      return b;
    }

    // say is a message box: tone good, warn or bad; title in bold, then
    // lines; acts are buttons under it.
    function say(tone, title, lines, acts) {
      const box = el("div", "say " + tone);
      box.setAttribute("role", tone === "good" ? "status" : "alert");
      box.append(I.icon(tone === "good" ? "checkcircle" : "alert"));
      const body = el("div", "");
      const p = el("p", "");
      if (title) p.append(el("b", "", title));
      // A line that starts with ~ is a quiet hint; a node is shown as is.
      for (const [i, l] of (lines || []).entries()) {
        if (!l) continue;
        if (typeof l !== "string") body.append(l);
        else if (i === 0 && title && !l.startsWith("~")) p.append(" ", l);
        else body.append(el("p", l.startsWith("~") ? "hint" : "", l.replace(/^~/, "")));
      }
      body.prepend(p);
      if (acts && acts.length) {
        const row = el("div", "row");
        row.append(...acts);
        body.append(row);
      }
      box.append(body);
      return box;
    }

    function field(label, input, small) {
      const f = el("label", "field");
      f.append(el("span", "", label), input);
      if (small) f.append(typeof small === "string" ? el("small", "", small) : small);
      return f;
    }

    function input(type, value, placeholder) {
      const i = document.createElement("input");
      i.type = type;
      i.value = value || "";
      if (placeholder) i.placeholder = placeholder;
      i.autocomplete = "off";
      i.spellcheck = false;
      return i;
    }

    // secret is a password field with a Show button. Its value is sent in
    // one request and the field is cleared after.
    function secret(placeholder) {
      const wrap = el("span", "pw");
      const i = input("password", "", placeholder);
      const show = btn("", "Show", () => {
        i.type = i.type === "password" ? "text" : "password";
        show.textContent = i.type === "password" ? "Show" : "Hide";
      });
      wrap.append(i, show);
      wrap.input = i;
      return wrap;
    }

    function radios(name, list, current, onPick, tags) {
      const box = el("div", "models");
      box.setAttribute("role", "radiogroup");
      box.setAttribute("aria-label", "Default model");
      for (const m of list) {
        const l = el("label", "");
        const r = document.createElement("input");
        r.type = "radio";
        r.name = name;
        r.value = m;
        r.checked = m === current;
        r.onchange = () => onPick(m);
        l.append(r, el("span", "nm", m));
        if (tags && tags[m]) l.append(el("span", "tag", tags[m]));
        box.append(l);
      }
      return box;
    }

    // ---- The dialog ----
    let dlg = null;
    let st = { step: "kind" };
    let backend = "";
    let catalog = null;
    let busy = false;
    let pollSeq = 0;

    function ensureDialog() {
      if (dlg) return dlg;
      dlg = document.createElement("dialog");
      dlg.id = "connect";
      dlg.className = "picker connect";
      dlg.setAttribute("aria-labelledby", "connect-title");
      dlg.addEventListener("cancel", (e) => { e.preventDefault(); close(); });
      document.body.append(dlg);
      return dlg;
    }

    // pinned is the default model the deployment sets ({ model, provider,
    // source }), or null when the default can be picked here.
    function pinned() {
      return (catalog && catalog.pinned) || null;
    }

    // pinnedNote says where the default is set, for a pinned default.
    function pinnedNote(p) {
      const n = el("p", "hint", "New chats keep starting with ");
      n.append(el("code", "", p.model || p.provider), ". The default model is set in the deployment config");
      if (p.source) n.append(" (", el("code", "", p.source), ")");
      n.append(", so it is changed there.");
      return n;
    }

    function close() {
      cancelSignIn();
      if (dlg && dlg.open) dlg.close();
      if (st.changed) ctx.connected(backend);
      st = { step: "kind" };
    }

    function go(ev) {
      st = Object.assign(next(st, ev), { changed: st.changed || ev.changed });
      draw();
    }

    async function open(b, step) {
      backend = b || ctx.backend();
      ensureDialog();
      st = { step: step || "kind" };
      catalog = null;
      draw();
      if (!dlg.open) dlg.showModal();
      try {
        catalog = await api("GET", path(backend, "/catalog"));
      } catch (e) {
        catalog = { error: e.message };
      }
      draw();
    }

    function head(title, back) {
      const h = el("h2", "");
      if (back) {
        const bb = I.button("chevleft", "Back", "ghost ib");
        bb.onclick = () => { cancelSignIn(); go({ type: "back" }); };
        h.append(bb);
      }
      const t = el("span", "grow", title);
      t.id = "connect-title";
      h.append(t);
      const x = I.button("close", "Close", "ghost ib");
      x.onclick = close;
      h.append(x);
      return h;
    }

    function foot(...items) {
      const f = el("div", "pfoot");
      f.append(...items);
      return f;
    }

    function draw() {
      if (!dlg) return;
      const focusId = document.activeElement && dlg.contains(document.activeElement) ? document.activeElement.id : "";
      const views = { kind: kindView, server: serverView, key: keyView, signin: signInView, pick: pickView, confirm: pickView, window: windowView, done: doneView };
      dlg.replaceChildren(...(views[st.step] || kindView)());
      if (focusId) document.getElementById(focusId)?.focus();
    }

    function hermesDown(e) {
      return say("bad", e.status === 502 && /^Could not reach Hermes/.test(e.message) ? "Could not reach Hermes." : e.message,
        e.status === 502 && /^Could not reach Hermes/.test(e.message) ? ["It may be restarting. Try again in a moment."] : []);
    }

    function kindView() {
      const body = el("div", "pbody");
      const choice = (icon, title, text, kind) => {
        const b = el("button", "choice", "");
        b.type = "button";
        const tx = el("span", "tx");
        tx.append(el("b", "", title), el("span", "", text));
        const ico = I.icon(icon);
        ico.classList.add("lead");
        const go2 = I.icon("chevright");
        go2.classList.add("go");
        b.append(ico, tx, go2);
        b.onclick = () => go({ type: "kind", kind });
        return b;
      };
      body.append(
        choice("server", "On my own computer or network", "Ollama, LM Studio, vLLM, llama.cpp, or any server that speaks the OpenAI API. Your messages go only to that address.", "server"),
        choice("key", "A provider, with an API key", "OpenRouter, OpenAI, Anthropic, Google AI Studio, DeepSeek, xAI and more. The provider bills you.", "key"),
        choice("user", "A subscription account", "Sign in with ChatGPT or Codex, Nous Portal, xAI Grok or MiniMax. You approve on the provider's own page.", "signin"));
      if (catalog && catalog.error) body.append(say("bad", catalog.error, []));
      if (pinned()) body.append(pinnedNote(pinned()));
      body.append(notListed());
      const intro = el("p", "intro", "Where does the model run?");
      return [head("Connect a model"), intro, body];
    }

    function notListed() {
      const d = el("details", "adv");
      d.append(el("summary", "", "Not listed here?"));
      const inner = el("div", "inner");
      const [where, cmd] = serverCommand(ctx.me().install);
      const p = el("p", "hint", "Anthropic account sign-in, Qwen sign-in, AWS Bedrock and Google Vertex AI are set up on the server, not here. " + where);
      const a = el("a", "", "Commands for other installs");
      a.href = docs + "#set-a-model-on-the-server";
      a.target = "_blank";
      a.rel = "noopener";
      inner.append(p, el("pre", "cmd", cmd), a);
      d.append(inner);
      // On a phone the opened part is below the fold of the dialog.
      d.ontoggle = () => { if (d.open) inner.scrollIntoView({ block: "nearest" }); };
      return d;
    }

    // ---- Own server ----
    function serverView() {
      const f = st.form || (st.form = { name: "Ollama", address: "", model: "", mode: "", ctxLen: "", sync: true });
      const body = el("div", "pbody");
      const name = input("text", f.name);
      name.id = "cm-name";
      name.oninput = () => { f.name = name.value; };
      const addr = input("text", f.address, "http://192.168.1.10:11434/v1");
      addr.id = "cm-addr";
      addr.inputMode = "url";
      addr.setAttribute("autocapitalize", "off");
      addr.oninput = () => { f.address = addr.value; };
      const addrNote = el("small", "", "The address of the machine that runs the server, ending in ");
      addrNote.append(el("code", "", "/v1"), " for Ollama and LM Studio. From inside Talaria, ", el("code", "", "localhost"),
        " is the Hermes container, not your computer. With Talaria's Compose ", el("code", "", "ollama"), " profile, use ", el("code", "", "http://ollama:11434/v1"), ".");
      const key = secret("Leave empty for Ollama and LM Studio");
      key.input.id = "cm-key";
      f.keyInput = key.input;
      body.append(field("Name", name, "Your models show under this name in the chat's model menu."), field("Address", addr, addrNote),
        field("API key (only if the server asks for one)", key));
      const test = btn("", busy ? "Testing..." : "Test address", () => testServer());
      test.disabled = busy || !f.address.trim();
      addr.addEventListener("input", () => { test.disabled = busy || !addr.value.trim(); });
      const acts = el("div", "acts");
      acts.append(test);
      body.append(acts);
      const v = st.test;
      if (v) body.append(serverSay(v));
      if (v && v.models && v.models.length) {
        body.append(el("div", "label", pinned() ? "Its model" : "Start new chats with"), radios("cm-model", v.models, st.model, (m) => { st.model = m; }),
          el("p", "hint", "Hermes needs a model with a window of at least 64,000 tokens. It asks the server for each model's window when you save."));
      } else if (v && v.noModels) {
        const typed = input("text", st.model, "The name the server uses, such as llama3.1:8b");
        typed.id = "cm-typed";
        typed.oninput = () => { st.model = typed.value.trim(); saveBtn.disabled = busy || !st.model; };
        body.append(field("Model name", typed));
      }
      body.append(advanced(f, v && v.needMode));
      if (st.error) body.append(st.error);
      const saveBtn = btn("primary", busy ? "Saving..." : pinned() ? "Save" : "Save and use", () => saveServer());
      saveBtn.disabled = busy || !v || !st.model || (v.tone === "bad" && !v.noModels);
      return [head("On my own computer or network", true), body, foot(btn("", "Back", () => go({ type: "back" })), el("span", "sp"), saveBtn)];
    }

    function serverSay(v) {
      if (v.tone === "good") {
        const p = el("p", "");
        p.append(el("b", "", v.title), " at " + v.at);
        const box = say("good", "", []);
        box.querySelector("div").replaceChildren(p, el("p", "hint", "New models you add to the server appear here on their own."));
        return box;
      }
      const acts = [btn("", "Test again", () => testServer())];
      if (v.noModels && !st.typing) acts.push(btn("", "Type a model name instead", () => { st.typing = true; draw(); document.getElementById("cm-typed")?.focus(); }));
      return say(v.tone, v.title, [v.text, v.hint], acts);
    }

    function advanced(f, open) {
      const d = el("details", "adv");
      d.open = !!open || !!f.advOpen;
      d.ontoggle = () => { f.advOpen = d.open; };
      d.append(el("summary", "", "Advanced"));
      const inner = el("div", "inner");
      const sel = document.createElement("select");
      sel.className = "f";
      sel.id = "cm-mode";
      for (const [v, t] of [["", "Detect it (recommended)"], ["chat_completions", "Chat Completions"], ["codex_responses", "Responses API"], ["anthropic_messages", "Anthropic Messages"]]) {
        const o = el("option", "", t);
        o.value = v;
        o.selected = v === f.mode;
        sel.append(o);
      }
      sel.onchange = () => { f.mode = sel.value; };
      const cl = input("number", f.ctxLen, "Ask the server");
      cl.id = "cm-ctx";
      cl.min = "1";
      cl.oninput = () => { f.ctxLen = cl.value; };
      const sync = document.createElement("input");
      sync.type = "checkbox";
      sync.checked = f.sync;
      sync.onchange = () => { f.sync = sync.checked; };
      const syncL = el("label", "check");
      const syncT = el("span", "", "Keep the model list in sync with the server");
      syncT.append(el("br"), el("small", "", "Turn off to use only the model you picked above."));
      syncL.append(sync, syncT);
      inner.append(field("API type", sel, "Only if the server speaks just one of them."), field("Context window in tokens", cl), syncL);
      d.append(inner);
      return d;
    }

    function endpointBody(f, model) {
      const b = { name: f.name.trim() || "Local server", base_url: (st.test && st.test.base) || f.address.trim(), model: model || "" };
      if (f.id) b.id = f.id;
      if (f.mode) b.api_mode = f.mode;
      if (f.ctxLen) b.context_length = Number(f.ctxLen);
      b.discover_models = !!f.sync;
      const k = f.keyInput ? f.keyInput.value : "";
      if (k) b.api_key = k;
      return b;
    }

    async function testServer() {
      const f = st.form;
      busy = true;
      st.error = null;
      st.test = null;
      draw();
      try {
        const b = endpointBody(f, st.model);
        b.base_url = f.address.trim();
        let r = await api("POST", path(backend, "/endpoints/test"), b);
        // Hermes probes the chat route with the model it is given; a guess
        // that the server does not have reads as a missing route.
        if (!r.ok && r.models && r.models.length && !r.models.includes(b.model)) {
          b.model = r.models[0];
          r = await api("POST", path(backend, "/endpoints/test"), b);
        }
        const v = serverVerdict(r, f.address);
        go({ type: "tested", verdict: v });
        if (v.models.length && !v.models.includes(st.model)) st.model = v.models[0];
      } catch (e) {
        st.error = hermesDown(e);
      }
      busy = false;
      draw();
    }

    async function saveServer() {
      const f = st.form;
      busy = true;
      st.error = null;
      draw();
      try {
        const b = endpointBody(f, st.model);
        // A default the deployment sets stays; the server is added next to it.
        b.make_default = !pinned();
        if (st.test && st.test.models) b.models = st.test.models;
        await api("POST", path(backend, "/endpoints"), b);
        if (f.keyInput) f.keyInput.value = "";
        st.changed = true;
        st.name = b.name;
        st.count = (st.test && st.test.models.length) || 1;
        let warning = null;
        try {
          if (!pinned()) warning = windowWarning(await api("GET", path(backend, "/info")));
        } catch (_) { /* the window is a hint only */ }
        busy = false;
        go({ type: "saved-server", warning, changed: true });
        return;
      } catch (e) {
        st.error = say("bad", e.message, []);
      }
      busy = false;
      draw();
    }

    function windowView() {
      const w = st.warning;
      const body = el("div", "pbody");
      const p2 = el("p", "", "Pick another model, or raise the server's window (for Ollama, ");
      p2.append(el("code", "", "OLLAMA_CONTEXT_LENGTH=65536"), " on the Ollama container).");
      body.append(say("warn", w.title, [w.text, p2], [
        btn("primary", "Pick another model", () => { st.step = "server"; draw(); }),
        btn("", "Use it anyway", () => { st.step = "done"; draw(); })]));
      return [head("Check the model's window"), body];
    }

    // ---- API key ----
    function keyView() {
      const body = el("div", "pbody");
      if (!catalog) {
        body.append(el("p", "wait", "Loading providers..."));
        return [head("A provider, with an API key", true), body];
      }
      if (catalog.error) {
        body.append(say("bad", catalog.error, []));
        return [head("A provider, with an API key", true), body];
      }
      const keys = catalog.keys || [];
      if (!st.provider) st.provider = (keys.find((k) => k.common) || keys[0] || {}).provider || "";
      const search = input("search", st.query || "", "OpenRouter, OpenAI, Anthropic...");
      search.id = "cm-find";
      search.oninput = () => { st.query = search.value; st.all = true; draw(); };
      body.append(field("Find a provider", search));
      const chosen = keys.find((k) => k.provider === st.provider);
      if (!st.all) {
        body.append(el("div", "label", "Common"));
        const chips = el("div", "chipset");
        chips.setAttribute("role", "group");
        chips.setAttribute("aria-label", "Providers");
        for (const k of keys.filter((x) => x.common)) chips.append(chip(k.name, k.provider === st.provider, () => { st.provider = k.provider; st.verdict = null; draw(); }));
        chips.append(chip("All providers", false, () => { st.all = true; draw(); document.getElementById("cm-find")?.focus(); }));
        body.append(chips);
      } else {
        const list = el("div", "plist");
        list.setAttribute("role", "listbox");
        list.setAttribute("aria-label", "All providers");
        const found = findProviders(keys, st.query);
        for (const k of found) {
          const b = el("button", k.provider === st.provider ? "on" : "", k.name);
          b.type = "button";
          b.setAttribute("role", "option");
          b.setAttribute("aria-selected", String(k.provider === st.provider));
          if (k.set) b.append(el("span", "tag", k.deployment ? "set in the deployment" : "connected"));
          b.onclick = () => { st.provider = k.provider; st.verdict = null; st.all = false; st.query = ""; draw(); };
          list.append(b);
        }
        if (!found.length) list.append(el("p", "hint", "No provider by that name. See Not listed here? on the first step."));
        body.append(list);
      }
      if (chosen && chosen.deployment) {
        const n = el("p", "hint");
        n.append(I.icon("lock"), " The " + chosen.name + " key is set in the deployment, so it is changed where the deployment sets it, not here. Its models are in the chat's model menu.");
        body.append(n);
      } else if (chosen) {
        const key = secret(chosen.set ? "Saved (" + chosen.preview + "). Type a new key to replace it." : "");
        key.input.id = "cm-apikey";
        st.keyInput = key.input;
        if (st.verdict && st.verdict.tone === "bad") key.input.classList.add("badf");
        const small = el("small", "", chosen.description ? chosen.description.replace(/\s*\(.*$/, "") + ". " : "");
        if (chosen.url) {
          const a = el("a", "", chosen.url.replace(/^https?:\/\//, "").replace(/\/$/, ""));
          a.href = chosen.url;
          a.target = "_blank";
          a.rel = "noopener";
          small.append("Get a key at ", a, ".");
        }
        body.append(field(chosen.name + " API key", key, small));
        const lock = el("p", "hint");
        lock.append(I.icon("lock"), " Hermes keeps the key in its own data folder on the server. Talaria does not store it, and nobody can read it back here, only a masked preview such as ",
          el("code", "", "sk-o...a1b2"), ".");
        body.append(lock);
      }
      if (st.verdict) body.append(say(st.verdict.tone, st.verdict.title, [st.verdict.lead, st.verdict.text ? "~" + st.verdict.text : ""]));
      if (st.error) body.append(st.error);
      const go2 = btn("primary", busy ? "Checking..." : "Check and connect", () => checkKey(chosen));
      go2.disabled = busy || !chosen || !!chosen.deployment;
      return [head("A provider, with an API key", true), body, foot(btn("", "Back", () => go({ type: "back" })), el("span", "sp"), go2)];
    }

    function chip(text, on, fn) {
      const b = el("button", "", text);
      b.type = "button";
      b.setAttribute("aria-pressed", String(on));
      b.onclick = fn;
      return b;
    }

    async function checkKey(p) {
      const value = st.keyInput ? st.keyInput.value.trim() : "";
      if (!value) {
        st.verdict = { tone: "bad", title: "Enter the key first." };
        draw();
        return;
      }
      busy = true;
      st.error = null;
      st.verdict = null;
      draw();
      try {
        const v = keyVerdict(await api("POST", path(backend, "/keys/test"), { key: p.key_env, value }), p.name);
        if (v.save) {
          await api("PUT", path(backend, "/keys"), { key: p.key_env, value });
          st.changed = true;
        }
        if (st.keyInput) st.keyInput.value = "";
        busy = false;
        go({ type: "key-checked", verdict: v, provider: p.provider, name: p.name, changed: v.save, pinned: !!pinned() });
        if (v.save && !pinned()) loadPick();
        return;
      } catch (e) {
        st.error = hermesDown(e);
        if (st.keyInput) st.keyInput.value = "";
      }
      busy = false;
      draw();
    }

    // ---- Sign-in ----
    function signInView() {
      const body = el("div", "pbody");
      if (!catalog || catalog.error) {
        body.append(catalog ? say("bad", catalog.error, []) : el("p", "wait", "Loading accounts..."));
        return [head("A subscription account", true), body];
      }
      const accounts = (catalog.accounts || []).filter((a) => a.here);
      if (!st.account) {
        body.append(el("p", "", "Which account?"));
        const chips = el("div", "chipset");
        for (const a of accounts) chips.append(chip(niceName(a.name), false, () => { st.account = a.id; st.name = niceName(a.name); startSignIn(); }));
        body.append(chips, notListed());
        return [head("A subscription account", true), body];
      }
      const title = "Sign in with " + st.name;
      if (st.signin === "expired") {
        body.append(say("bad", "The sign-in code ran out.", ["Codes work for 15 minutes."], [btn("primary", "Get a new code", () => startSignIn()), btn("", "Cancel", () => go({ type: "back" }))]));
        return [head(title, true), body];
      }
      if (st.signin === "failed" || st.error) {
        body.append(st.error || say("bad", st.message, []), el("div", "acts"));
        body.lastChild.append(btn("", "Try again", () => startSignIn()));
        return [head(title, true), body];
      }
      if (!st.code) {
        body.append(el("p", "wait", "Asking for a code..."));
        return [head(title, true), body];
      }
      body.append(el("p", "", "Open the sign-in page, then enter this code:"));
      const code = el("div", "code-big", st.code.user_code);
      code.setAttribute("aria-label", "Sign-in code");
      body.append(code);
      const acts = el("div", "acts");
      const link = el("a", "button primary", "");
      link.href = st.code.verification_url;
      link.target = "_blank";
      link.rel = "noopener";
      let where = st.code.verification_url;
      try { where = new URL(where).host; } catch (_) { /* shown as given */ }
      link.append(I.icon("open"), "Open " + where);
      acts.append(link, btn("", "Copy code", () => { navigator.clipboard?.writeText(st.code.user_code); ctx.toast("Code copied"); }, "copy"));
      body.append(acts);
      const wait = el("div", "wait");
      wait.setAttribute("role", "status");
      const left = minutesLeft(st.until, Date.now());
      wait.append(el("span", "spin"), "Waiting for you to approve it. The code works for " + left + " more minute" + (left === 1 ? "" : "s") + ".");
      body.append(wait, el("p", "hint", "You sign in on the provider's own page. Talaria and Hermes never see your password."));
      return [head(title, true), body, foot(btn("", "Cancel sign-in", () => { cancelSignIn(); go({ type: "back" }); }))];
    }

    // niceName drops Hermes's notes in parentheses and its OAuth tags.
    function niceName(n) {
      return String(n || "").replace(/ Subscription$/, "").replace(/\s*OAuth\b/, "").replace(/\s*\(OAuth\)$/, "").replace(/\s+\/\s+/g, " or ");
    }

    async function startSignIn() {
      cancelSignIn();
      st.code = null;
      st.signin = "";
      st.error = null;
      draw();
      const seq = ++pollSeq;
      try {
        const c = await api("POST", path(backend, "/signin/" + encodeURIComponent(st.account)));
        if (seq !== pollSeq) return;
        st.until = Date.now() + (c.expires_in || 900) * 1000;
        go({ type: "code", code: c });
        poll(seq, c.session_id);
      } catch (e) {
        if (seq === pollSeq) {
          st.error = hermesDown(e);
          draw();
        }
      }
    }

    async function poll(seq, session) {
      while (seq === pollSeq && dlg.open) {
        let r;
        try {
          r = await api("GET", path(backend, "/signin/" + encodeURIComponent(st.account) + "/" + encodeURIComponent(session)));
        } catch (e) {
          if (seq !== pollSeq) return;
          st.error = hermesDown(e);
          draw();
          return;
        }
        if (seq !== pollSeq) return;
        const s = signInState(r);
        if (s.kind === "pending") {
          draw();
          continue;
        }
        pollSeq++;
        st.session = "";
        if (s.kind === "approved") st.changed = true;
        go({ type: "polled", state: s, provider: st.account, name: st.name, changed: s.kind === "approved", pinned: !!pinned() });
        if (s.kind === "approved" && !pinned()) loadPick();
        return;
      }
    }

    function cancelSignIn() {
      if (st.code && st.code.session_id && st.signin === "pending") {
        api("DELETE", path(backend, "/signin/" + encodeURIComponent(st.account) + "/" + encodeURIComponent(st.code.session_id))).catch(() => {});
      }
      pollSeq++;
    }

    // ---- Pick the default ----
    async function loadPick() {
      st.pick = null;
      draw();
      try {
        const [opts, rec] = await Promise.all([api("GET", path(backend, "/options?refresh=1")),
          api("GET", path(backend, "/recommended?provider=" + encodeURIComponent(st.provider))).catch(() => ({}))]);
        const row = (opts.providers || []).find((p) => p.id === st.provider);
        st.pick = { models: (row && row.models) || [], suggested: rec.model || "" };
        st.model = st.pick.suggested || st.pick.models[0] || "";
        if (st.pick.suggested && !st.pick.models.includes(st.pick.suggested)) st.pick.models.unshift(st.pick.suggested);
      } catch (e) {
        st.pick = { models: [], error: e.message };
      }
      draw();
    }

    function pickView() {
      const body = el("div", "pbody");
      if (st.verdict) body.append(say(st.verdict.tone, st.verdict.title, [st.verdict.text ? "~" + st.verdict.text : ""]));
      if (!st.pick) body.append(el("p", "wait", "Asking Hermes for its models..."));
      else if (st.pick.error) body.append(say("bad", st.pick.error, []));
      else if (!st.pick.models.length) body.append(el("p", "hint", "Hermes lists no models for this provider yet. Pick one in a chat's model menu once it shows there."));
      else {
        const tags = st.pick.suggested ? { [st.pick.suggested]: "suggested" } : {};
        body.append(radios("cm-pick", st.pick.models.slice(0, 12), st.model, (m) => { st.model = m; }, tags),
          el("p", "hint", "A short list from Hermes. Every model of the provider is in the chat's model menu once you connect; each chat can use a different one."));
      }
      if (st.kept) body.append(say("warn", st.kept, []));
      if (st.step === "confirm") {
        body.append(say("warn", "", [st.confirm, "Use it as the default for new chats?"], [
          btn("primary", "Use it anyway", () => setDefault(true)), btn("", "Choose another", () => { st.step = "pick"; draw(); })]));
      }
      if (st.error) body.append(st.error);
      const use = btn("primary", busy ? "Saving..." : "Use this model", () => setDefault(false));
      use.disabled = busy || !st.model || st.step === "confirm";
      const h = el("h2", "");
      const t = el("span", "grow", "Pick the model new chats start with");
      t.id = "connect-title";
      h.append(t);
      return [h, body, foot(btn("", "Skip, I will pick in a chat", () => go({ type: "default-set", model: "", skipped: true })), el("span", "sp"), use)];
    }

    async function setDefault(confirm) {
      busy = true;
      st.error = null;
      draw();
      try {
        const r = await api("POST", path(backend, "/default"), { provider: st.provider, model: st.model, confirm });
        busy = false;
        if (r && r.confirm_required) {
          go({ type: "confirm", message: r.confirm_message, model: st.model });
          return;
        }
        if (r && r.unchanged) {
          go({ type: "default-kept", message: r.message });
          return;
        }
        go({ type: "default-set", model: (r && r.model) || st.model, changed: true });
        return;
      } catch (e) {
        st.error = hermesDown(e);
      }
      busy = false;
      draw();
    }

    function doneView() {
      const body = el("div", "pbody done");
      const badge = el("div", "badge-ic ok");
      badge.append(I.icon("check"));
      const name = st.form ? st.form.name || "Your server" : st.name || "The provider";
      const h = el("h2", "", name + " is connected");
      h.id = "connect-title";
      const p = el("p", "hint");
      if (pinned()) {
        p.append("Pick its models in a chat's model menu.");
        body.append(badge, h, p, pinnedNote(pinned()));
        return [body, foot(btn("", "Connect another", () => { const c = st.changed; st = { step: "kind", changed: c }; draw(); }), el("span", "sp"),
          btn("primary", "Start a chat", () => { close(); ctx.startChat(); }))];
      }
      if (st.model && !st.skipped) {
        p.append("New chats start with ", el("code", "", st.model), ".");
        if (st.form) p.append(" You can pick any of its " + (st.count || 1) + " model" + (st.count === 1 ? "" : "s") + " in a chat's model menu.");
        else p.append(" You can pick any of its models in a chat's model menu.");
      } else p.append("Pick its models in a chat's model menu.");
      body.append(badge, h, p);
      return [body, foot(btn("", "Connect another", () => { const c = st.changed; st = { step: "kind", changed: c }; draw(); }), el("span", "sp"),
        btn("primary", "Start a chat", () => { close(); ctx.startChat(); }))];
    }

    // ---- First-run card ----
    // firstRun draws the card into box for status st0 ({ ready, admin }).
    function firstRun(box, st0, check) {
      const card = el("div", "empty");
      card.setAttribute("role", "status");
      const badge = el("div", "badge-ic");
      badge.append(I.icon("chip"));
      card.append(badge);
      const acts = el("div", "acts");
      if (st0.admin) {
        card.append(el("h1", "", "Talaria has no model yet"),
          el("p", "", "Its agent needs a model before it can chat. Connect one you run yourself, or a provider you have a key or an account for. It takes about a minute."));
        const a = el("a", "", "What can I connect?");
        a.href = docs;
        a.target = "_blank";
        a.rel = "noopener";
        acts.append(btn("primary", "Connect a model", () => open(st0.backend), "plug"), a);
        card.append(acts, el("p", "fine", "You can add more later in Settings, Models."));
      } else {
        card.append(el("h1", "", "No model is set up yet"),
          el("p", "", "This agent cannot chat until an admin connects a model. Ask whoever runs this Talaria to open Settings, then Models."));
        acts.append(btn("", "Check again", check));
        card.append(acts);
      }
      const bar = el("div", "composer off");
      bar.setAttribute("aria-hidden", "true");
      bar.append(el("div", "ta", st0.admin ? "Connect a model to chat" : "Waiting for an admin to connect a model"));
      const send = btn("primary ib send", "", null, "send");
      send.disabled = true;
      bar.append(send);
      box.replaceChildren(card);
      return bar;
    }

    return { open, close, firstRun, settings: (pane) => window.HubModelSettings({ api, el, me: ctx.me, connect: open, pane, say, btn, field, secret, input, path, hermesDown, toast: ctx.toast, changed: ctx.connected }) };
  }

  return { create, split, serverCommand, isLocal, serverVerdict, keyVerdict, signInState, minutesLeft, windowWarning, findProviders, next };
})();

// HubModelSettings is Settings > Models: what Hermes has connected, with
// edit, test again, make default and remove. ctx comes from HubModels.
window.HubModelSettings = (ctx) => {
  const { api, el, say, btn, field, secret, input, path } = ctx;
  const I = window.HubIcons;
  let backends = [];
  let backend = "";
  let data = null;
  let err = null;
  let open = {};
  let editing = "";
  let removing = "";
  let note = null;
  let busy = false;
  let insteadModel = "";
  // removeConfirm is Hermes's price warning for the default a removal
  // picks instead, until the admin answers it.
  let removeConfirm = "";
  const deployedConfig = "Set in the deployment config";
  const deployed = "Set in the deployment";
  const apiTypes = { chat_completions: "OpenAI Chat Completions", codex_responses: "OpenAI Responses", anthropic_messages: "Anthropic Messages" };

  // pinned is the default the deployment sets, or null.
  const pinned = () => (data && data.pinned) || null;

  async function load() {
    if (!backends.length) {
      try {
        backends = await api("GET", "/api/backends");
      } catch (_) {
        backends = [];
      }
      // Only the agents this admin connects models to (models.admins).
      const mine = ctx.me().models_admin_backends;
      if (Array.isArray(mine)) backends = backends.filter((b) => mine.includes(b.id));
      backend = backend || (ctx.me().default_backend && backends.some((b) => b.id === ctx.me().default_backend) ? ctx.me().default_backend : (backends[0] || {}).id || "");
    }
    try {
      data = await api("GET", path(backend, "/connected"));
      err = null;
    } catch (e) {
      data = null;
      err = ctx.hermesDown(e);
    }
    draw();
  }

  function rows() {
    if (!data) return [];
    const out = [];
    const st = data.status || {};
    const models = (id) => ((data.providers || []).find((p) => p.id === id) || {}).models || [];
    // The address shows once, in the row's details.
    for (const e of data.endpoints || []) {
      const ms = models(e.id).length ? models(e.id) : e.models || [];
      out.push({ key: "ep:" + e.id, kind: "endpoint", id: e.id, name: e.name || e.id, provider: e.id, e, models: ms, managed: !!e.managed,
        isDefault: !!e.is_current || st.provider === e.id,
        meta: [e.managed ? deployedConfig : "Your own server", ms.length ? ms.length + (ms.length === 1 ? " model" : " models") : ""] });
    }
    for (const k of data.keys || []) {
      out.push({ key: "key:" + k.key_env, kind: "key", id: k.key_env, name: k.name, provider: k.provider, k, models: models(k.provider),
        managed: !!k.deployment, isDefault: st.provider === k.provider, meta: [k.deployment ? deployed : "API key saved (" + k.preview + ")"] });
    }
    for (const a of data.accounts || []) {
      out.push({ key: "acct:" + a.id, kind: "account", id: a.id, name: a.name, provider: a.id, a, models: models(a.id), isDefault: st.provider === a.id, meta: ["Signed in"] });
    }
    return out;
  }

  function draw() {
    const p = ctx.pane;
    p.replaceChildren(el("h2", "", "Models"),
      el("p", "hint", "What Hermes can use. New chats start with the default; each chat can pick any model from these in its model menu. Hermes keeps these on the server."));
    if (backends.length > 1) {
      const sel = document.createElement("select");
      sel.id = "set-models-agent";
      sel.className = "f";
      for (const b of backends) {
        const o = el("option", "", b.name || b.id);
        o.value = b.id;
        o.selected = b.id === backend;
        sel.append(o);
      }
      sel.onchange = () => { backend = sel.value; data = null; open = {}; load(); };
      p.append(field("Agent", sel));
    }
    const acts = el("div", "acts");
    acts.append(btn("primary", "Connect a model", () => ctx.connect(backend), "plus"));
    p.append(acts);
    if (err) p.append(err);
    if (note) p.append(note);
    if (!data) {
      if (!err) p.append(el("p", "none", "Loading..."));
      return;
    }
    const pin = pinned();
    if (pin) {
      const n = el("p", "hint pinned");
      n.append(I.icon("lock"), " New chats start with ", el("code", "", pin.model || pin.provider), ". The default model is set in the deployment config");
      if (pin.source) n.append(" (", el("code", "", pin.source), ")");
      n.append(", so it is changed there, not here.");
      p.append(n);
    }
    const list = rows();
    if (!list.length) {
      p.append(el("p", "none", "No model is connected yet."));
      return;
    }
    const box = el("div", "set-rows");
    for (const r of list) box.append(row(r));
    p.append(box);
  }

  function row(r) {
    const wrap = el("div", "set-row");
    const head = el("button", "set-rowhead", "");
    head.type = "button";
    head.setAttribute("aria-expanded", String(!!open[r.key]));
    const st = el("span", r.managed ? "st lock" : "st on");
    st.setAttribute("aria-hidden", "true");
    const top = el("span", "set-top");
    top.append(st, el("span", "nm", r.name));
    if (r.isDefault) top.append(el("span", "pill okp", "Default"));
    const meta = el("span", "meta");
    for (const m of r.meta) if (m) meta.append(el("span", "", m));
    head.append(top, meta);
    head.onclick = () => { open[r.key] = !open[r.key]; if (!open[r.key]) { editing = ""; removing = ""; } draw(); };
    wrap.append(head);
    if (removing === r.key) wrap.append(removeAsk(r));
    else if (open[r.key]) wrap.append(detail(r));
    return wrap;
  }

  function detail(r) {
    const box = el("div", "set-detail");
    if (editing === r.key) return editForm(r, box);
    const dl = el("dl", "kv");
    const add = (k, v) => { if (v) dl.append(el("dt", "", k), el("dd", "", v)); };
    const def = (data.status || {}).model;
    const ms = r.models.map((m) => (r.isDefault && m === def ? m + " (default)" : m)).join(", ");
    if (r.kind === "endpoint") {
      add("Address", r.e.base_url);
      add("API key", r.e.has_api_key ? r.e.api_key_preview || "Saved" : "None");
      // Only an API type someone chose; Hermes finds out the others.
      add("API type", apiTypes[r.e.api_mode] || "");
    } else if (r.kind === "key") add("API key", r.k.deployment ? deployed : r.k.preview);
    else add("Account", "Signed in");
    add("Models", ms);
    box.append(dl);
    if (r.managed) {
      const n = el("p", "hint");
      n.append(I.icon("lock"), r.kind === "key" ? " " + deployed + ", so it is changed where the deployment sets it, not here." : " " + deployedConfig + ", so it is changed there, not here.");
      box.append(n);
    }
    const acts = el("div", "acts");
    if (r.kind !== "account" && !r.managed) acts.append(btn("", "Edit", () => { editing = r.key; note = null; draw(); }, "edit"));
    if (r.kind === "endpoint") acts.append(btn("", "Test again", () => testAgain(r), "retry"));
    if (!r.isDefault && !pinned()) acts.append(btn("", "Make default for new chats", () => makeDefault(r)));
    if ((r.kind !== "account" || r.a.removable) && !r.managed) acts.append(btn("danger-text", "Remove", () => { removing = r.key; insteadModel = ""; removeConfirm = ""; draw(); }, "trash"));
    if (acts.childNodes.length) box.append(acts);
    return box;
  }

  function editForm(r, box) {
    box.append(el("h3", "", "Edit " + r.name));
    if (r.kind === "key") {
      const k = secret("Saved (" + r.k.preview + "). Type a new key to replace it.");
      k.input.id = "edit-key";
      box.append(field("API key", k, "Leave empty to keep the saved key."));
      const acts = el("div", "acts");
      acts.append(btn("primary", busy ? "Checking..." : "Check and save", async () => {
        const value = k.input.value.trim();
        k.input.value = "";
        if (!value) { editing = ""; draw(); return; }
        await run(async () => {
          const v = window.HubModels.keyVerdict(await api("POST", path(backend, "/keys/test"), { key: r.id, value }), r.name);
          if (!v.save) { note = say("bad", v.title, [v.lead, "~" + v.text]); return false; }
          await api("PUT", path(backend, "/keys"), { key: r.id, value });
          note = say(v.tone, v.kind === "accepted" ? r.name + " accepted the new key." : v.title, [v.text ? "~" + v.text : ""]);
          return true;
        });
      }), btn("", "Cancel", () => { editing = ""; draw(); }));
      box.append(acts);
      return box;
    }
    const e = r.e;
    const name = input("text", e.name);
    const addr = input("text", e.base_url);
    const k = secret(e.has_api_key ? "Saved. Type a new key to replace it." : "Leave empty if the server needs none");
    box.append(field("Name", name), field("Address", addr), field("API key", k, e.has_api_key ? "Leave empty to keep the saved key." : ""));
    const acts = el("div", "acts");
    acts.append(btn("primary", busy ? "Saving..." : "Test and save", async () => {
      const key = k.input.value;
      k.input.value = "";
      await run(async () => {
        const body = { id: e.id, name: name.value.trim() || e.name, base_url: addr.value.trim(), model: e.model || r.models[0] || "", make_default: false };
        if (e.api_mode) body.api_mode = e.api_mode;
        if (key) body.api_key = key;
        const t = await api("POST", path(backend, "/endpoints/test"), body);
        const v = window.HubModels.serverVerdict(t, body.base_url);
        if (v.tone === "bad") { note = say("bad", v.title, [v.text, v.hint]); return false; }
        if (t.resolved_base_url) body.base_url = t.resolved_base_url;
        if (t.models && t.models.length) body.models = t.models;
        await api("POST", path(backend, "/endpoints"), body);
        note = say("good", r.name + " is saved.", []);
        return true;
      });
    }), btn("", "Cancel", () => { editing = ""; draw(); }));
    box.append(acts);
    return box;
  }

  // run does one change, then reloads the list.
  async function run(fn) {
    busy = true;
    note = null;
    draw();
    let ok = false;
    try {
      ok = await fn();
    } catch (e) {
      note = ctx.hermesDown(e);
    }
    busy = false;
    if (ok) {
      editing = "";
      ctx.changed(backend);
    }
    await load();
  }

  async function testAgain(r) {
    await run(async () => {
      const t = await api("POST", path(backend, "/endpoints/test"), { id: r.e.id, name: r.e.name, base_url: r.e.base_url, model: r.e.model || "" });
      const v = window.HubModels.serverVerdict(t, r.e.base_url);
      note = v.tone === "good" ? say("good", r.name + ": " + v.title.toLowerCase() + " at " + v.at, []) : say(v.tone, v.title, [v.text, v.hint]);
      return false;
    });
  }

  // kept is the note for a default Hermes did not change.
  function kept(res) {
    return say("warn", res.message || "Hermes kept its default model.", []);
  }

  async function makeDefault(r, model, confirm) {
    await run(async () => {
      if (r.kind === "endpoint" && !model) {
        const res = await api("POST", path(backend, "/endpoints/" + encodeURIComponent(r.id) + "/use"));
        if (res && res.unchanged) { note = kept(res); return false; }
        note = say("good", "New chats start with " + r.name + ".", []);
        return true;
      }
      let pick = model;
      if (!pick) {
        const rec = await api("GET", path(backend, "/recommended?provider=" + encodeURIComponent(r.provider))).catch(() => ({}));
        pick = rec.model || r.models[0];
      }
      if (!pick) { note = say("warn", "Hermes lists no models for " + r.name + " yet.", []); return false; }
      const res = await api("POST", path(backend, "/default"), { provider: r.provider, model: pick, confirm: !!confirm });
      if (res && res.confirm_required) {
        // Hermes's price warning, answered here before anything changes.
        note = say("warn", "", [res.confirm_message, "Use it as the default for new chats?"], [
          btn("primary", "Use it anyway", () => makeDefault(r, pick, true)), btn("", "Keep the current default", () => { note = null; draw(); })]);
        return false;
      }
      if (res && res.unchanged) { note = kept(res); return false; }
      note = say("good", "New chats start with " + ((res && res.model) || pick) + ".", []);
      return true;
    });
  }

  function removeAsk(r) {
    const box = el("div", "set-alert");
    box.setAttribute("role", "alertdialog");
    const what = { endpoint: "Hermes forgets this server and its key.", key: "Hermes deletes the key.", account: "Hermes forgets the sign-in." }[r.kind];
    const t = el("span", "");
    t.append(el("b", "", "Remove " + r.name + "?"), " " + what + " Chats that use its models stop until you pick another model.");
    box.append(t);
    const others = [];
    if (r.isDefault && !pinned()) {
      for (const p of data.providers || []) {
        if (p.id === r.provider) continue;
        for (const m of p.models) others.push({ provider: p.id, name: p.name, model: m });
      }
      if (others.length) {
        const sel = document.createElement("select");
        sel.className = "f";
        for (const o of others) {
          const opt = el("option", "", o.name + ": " + o.model);
          opt.value = o.provider + "\n" + o.model;
          sel.append(opt);
        }
        insteadModel = insteadModel || sel.options[0].value;
        sel.value = insteadModel;
        sel.onchange = () => { insteadModel = sel.value; };
        box.append(field("It is the default. New chats start with this instead:", sel));
      } else box.append(el("p", "hint", "It is the default and the only model connected: chats cannot start until you connect another."));
    }
    const acts = el("div", "acts");
    if (removeConfirm) {
      // The model picked instead needs the same yes as Make default.
      box.append(say("warn", "", [removeConfirm, "Use it as the default for new chats, and remove " + r.name + "?"]));
      acts.append(btn("danger", busy ? "Removing..." : "Use it and remove", () => remove(r, insteadModel, true)),
        btn("", "Choose another", () => { removeConfirm = ""; draw(); }));
    } else {
      acts.append(btn("danger", busy ? "Removing..." : "Remove", () => remove(r, others.length ? insteadModel : "")), btn("", "Keep it", () => { removing = ""; draw(); }));
    }
    box.append(acts);
    return box;
  }

  async function remove(r, instead, confirm) {
    await run(async () => {
      if (instead) {
        const [provider, model] = instead.split("\n");
        const res = await api("POST", path(backend, "/default"), { provider, model, confirm: !!confirm });
        if (res && res.confirm_required) {
          removeConfirm = res.confirm_message || "This model needs a confirmation.";
          return false;
        }
        if (res && res.unchanged) {
          // Removing it now would leave the default without its provider.
          note = kept(res);
          removing = "";
          return false;
        }
      }
      removeConfirm = "";
      if (r.kind === "endpoint") await api("DELETE", path(backend, "/endpoints/" + encodeURIComponent(r.id)));
      else if (r.kind === "key") await api("DELETE", path(backend, "/keys/" + encodeURIComponent(r.id)));
      else await api("DELETE", path(backend, "/signin/" + encodeURIComponent(r.id)));
      removing = "";
      delete open[r.key];
      note = say("good", r.name + " is removed.", []);
      return true;
    });
  }

  return { load, draw };
};

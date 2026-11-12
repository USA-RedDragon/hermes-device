"use strict";
// HubClaude shows the Claude accounts of a backend with the claude-accounts
// Hermes plugin, and the ChatGPT plan that Hermes itself signs in to: the
// account and its usage on the line under a chat's
// title (for a chat on the Claude pool, the account the pool runs it on),
// a panel on the Usage page with each account's API credit estimate, the
// notice in a chat whose account lost its login, and the sign-in dialog. Only the backend's Claude admins see
// the sign-in controls; the hub refuses everyone else too. The sign-in
// link and code stay in the dialog: the code input is cleared as soon as it
// is sent, and neither is stored. ctx gives it the app's api and el
// helpers, the signed-in user (me) and redraw, which draws the status line
// again.
window.HubClaude = (ctx) => {
  const { api, el } = ctx;
  const apiProvider = "anthropic";
  const apiName = "Claude API";
  const poolProvider = "claude-pool";
  const chatgptProvider = "openai-codex";
  const creditsPrefix = "claude-api-";
  // cache maps a backend ID to { at, data } (data null: no plugin) while
  // a load runs or after it ended.
  const cache = new Map();
  const loading = new Map();
  // pools maps "backend\nsession" to { at, data, model } for chats on the
  // pool: the account the pool runs the chat's next turn on.
  const pools = new Map();
  const minute = 60000;
  const me = () => (ctx.me() || {});

  const base = (backend) => "/api/claude/" + encodeURIComponent(backend) + "/accounts";

  // describe names an account and says why it needs a sign-in, as the hub's
  // notifications do.
  function describe(label, email, reason) {
    return label + (email ? " (last signed in as " + email + ")" : "") +
      (reason === "first" ? " needs a sign-in: this is its first sign-in." : " needs a sign-in: the login expired or was revoked.");
  }

  async function load(backend, force) {
    const c = cache.get(backend);
    if (!force && c && Date.now() - c.at < minute) return c.data;
    if (loading.has(backend)) return loading.get(backend);
    const p = (async () => {
      let data = null;
      try {
        data = await api("GET", base(backend) + "?usage=1");
      } catch (e) {
        if (!/no Claude accounts plugin/.test(e.message) && c) data = c.data;
      }
      cache.set(backend, { at: Date.now(), data });
      loading.delete(backend);
      return data;
    })();
    loading.set(backend, p);
    return p;
  }

  // poolAccount stands in for "the Claude pool" in a chat's status line: one
  // account-like object whose week is the pool's average, so the line shows
  // "Claude" and one week meter, not the account the chat runs on.
  function poolAccount(backend) {
    const c = cache.get(backend);
    const p = c && c.data && c.data.pool;
    if (!p || typeof p.used_percent !== "number") return null;
    const w = { kind: "weekly_all", label: "Current week", used_percent: p.used_percent };
    return { id: "", label: "Claude", provider: poolProvider, signed_in: true, pool: p, usage: { windows: [w] } };
  }

  function accountFor(backend, provider, session) {
    const c = cache.get(backend);
    if (!c || !c.data || !provider) return null;
    if (provider === poolProvider) {
      const p = pools.get(backend + "\n" + session);
      const id = p && p.data && p.data.account;
      return id ? (c.data.accounts || []).find((a) => a.id === id) || null : null;
    }
    return (c.data.accounts || []).find((a) => a.provider === provider) || null;
  }

  // pool reads which account the pool runs a chat's next turn on, at most
  // once a minute unless force. It answers null when that is unknown.
  async function pool(backend, session, model, force) {
    const key = backend + "\n" + session;
    const p = pools.get(key);
    if (!force && p && p.model === model && (p.loading || Date.now() - p.at < minute)) return p.loading || p.data;
    const entry = { at: Date.now(), data: p ? p.data : null, model };
    entry.loading = (async () => {
      try {
        entry.data = await api("GET", "/api/claude/" + encodeURIComponent(backend) + "/pool?session=" +
          encodeURIComponent(session) + "&model=" + encodeURIComponent(model || ""));
      } catch (_) { /* the next read tries again */ }
      entry.at = Date.now();
      entry.loading = null;
      return entry.data;
    })();
    pools.set(key, entry);
    return entry.loading;
  }

  // route is the line under the model picker: the account a chat on the
  // pool runs on, or what an API credit provider bills. Null for others.
  async function route(backend, session, provider, model) {
    if (provider === poolProvider) {
      const p = await pool(backend, session, model);
      if (!p) return null;
      if (!p.account) return { bad: true, parts: [p.message || "No Claude account can run this chat now."] };
      const tag = el("span", "tag", p.resets_text ? "resets " + p.resets_text : "");
      return { parts: ["Runs on ", el("b", "", p.account), " ", ...(p.resets_text ? [tag] : []),
        p.pinned ? "" : ", the soonest reset with room."] };
    }
    if (provider && provider.startsWith(creditsPrefix)) return { parts: ["Bills that account's monthly API credits."] };
    return null;
  }

  // money is a dollar amount as the credits line shows it: whole dollars
  // from $10 up, cents below.
  const money = (v) => "$" + (v >= 10 ? Math.round(v).toString() : v.toFixed(2));

  // creditsLine is an account's API credit estimate for the Usage page.
  function creditsLine(c) {
    if (c.error || typeof c.spent_usd !== "number") return "API credits: no estimate" + (c.error ? " (" + c.error + ")" : "") + ".";
    const resets = c.resets_text ? ", resets " + c.resets_text : "";
    if (typeof c.left_usd === "number" && typeof c.monthly_usd === "number") {
      return "API credits: about " + money(c.left_usd) + " of " + money(c.monthly_usd) + " left" + resets + ".";
    }
    return "API credits: about " + money(c.spent_usd) + " spent this month" + resets + ".";
  }

  const pct = (w) => Math.round(w.used_percent) + "%";
  // reported are the windows an account's CLI reports with a share used.
  // Some plans have no 5-hour window; it is left out, never shown empty.
  const reported = (u) => (u && u.windows || []).filter((w) => typeof w.used_percent === "number");
  const windowOf = (u, kind) => reported(u).find((w) => w.kind === kind);

  // parts returns the status line's spans for a chat: "Claude API" for the
  // API key route, and the account with its 5-hour and weekly use for a
  // subscription account.
  function parts(backend, info) {
    const provider = info.provider || "";
    if (provider === apiProvider) {
      const s = el("span", "claude", apiName);
      s.title = "Anthropic API with an API key";
      return [s];
    }
    if (!backend || !provider || !me().claude) return [];
    const c = cache.get(backend);
    if (!c || Date.now() - c.at >= minute) load(backend).then(() => ctx.redraw());
    const a = provider === poolProvider ? poolAccount(backend) : accountFor(backend, provider);
    if (!a) return [];
    const out = [];
    const name = el("span", "claude", a.label);
    name.title = a.email ? a.label + ": " + a.email : a.label;
    out.push(name);
    if (!a.signed_in) {
      out.push(el("span", "claude-off", "not signed in"));
      return out;
    }
    for (const [kind, short, long] of [["session", "5h", "5-hour session"], ["weekly_all", "Week", "Week"]]) {
      const w = windowOf(a.usage, kind);
      if (!w) continue;
      const sp = el("span", "claude-use" + (w.used_percent >= 80 ? " full" : ""),
        short + " " + pct(w) + (w.resets_text ? " (resets " + w.resets_text + ")" : ""));
      sp.title = long + ": " + pct(w) + " used" + (w.resets_text ? ", resets " + w.resets_text : "");
      out.push(sp);
    }
    return out;
  }

  // note fills the bar under the chat's title when the chat's account is
  // signed out.
  function note(box, backend, info) {
    box.replaceChildren();
    const a = accountFor(backend, info.provider || "");
    box.hidden = !a || a.signed_in;
    if (box.hidden) return;
    box.append(el("span", "", describe(a.label, a.last_email || a.email, a.last_email || a.email ? "expired" : "first") + " "));
    box.append(signInControl(backend, a.id, a.signed_in));
  }

  // afterReply reads the usage of the chat's account again, and for a chat
  // on the pool which account it is on.
  async function afterReply(backend, provider, session, model) {
    if (provider === poolProvider) {
      await pool(backend, session, model, true);
      await load(backend, true);
      ctx.redraw();
      return;
    }
    const a = accountFor(backend, provider, session);
    if (!a) return;
    try {
      const u = await api("GET", base(backend) + "/" + encodeURIComponent(a.id) + "/usage?fresh=1");
      a.usage = u;
      ctx.redraw();
    } catch (_) { /* the next minute's load tries again */ }
  }

  function signInControl(backend, account, signedIn) {
    const c = cache.get(backend);
    if (!c || !c.data || !c.data.admin) {
      return el("span", "hint", "Ask a Claude admin of this Hermes to sign it in.");
    }
    const b = el("button", "", signedIn ? "Sign in again" : "Sign in");
    b.type = "button";
    b.onclick = () => signIn(backend, account);
    return b;
  }

  // chatNotice is the note a chat gets when a turn failed because its
  // Claude account lost its login.
  function chatNotice(backend, p) {
    cache.delete(backend);
    const box = el("div", "note bad claude-notice");
    box.append(el("span", "", describe(p.label || p.account, p.email_hint, p.reason) + " "));
    load(backend, true).then(() => {
      box.append(signInControl(backend, p.account, false));
      ctx.redraw();
    });
    return box;
  }

  let dlg = null;
  let dlgBackend = "";

  // signIn runs the sign-in dialog of one account.
  async function signIn(backend, account) {
    if (!dlg) {
      dlg = el("dialog", "picker claude-dialog");
      dlg.setAttribute("aria-labelledby", "claude-dialog-title");
      document.body.append(dlg);
      dlg.addEventListener("close", () => { dlg.replaceChildren(); load(dlgBackend, true).then(() => ctx.redraw()); });
    }
    dlgBackend = backend;
    const path = base(backend) + "/" + encodeURIComponent(account) + "/signin";
    const head = el("h2", "", "Sign in to Claude");
    head.id = "claude-dialog-title";
    const body = el("div", "pbody");
    const foot = el("div", "pfoot");
    const close = el("button", "", "Close");
    close.type = "button";
    close.onclick = () => dlg.close();
    dlg.replaceChildren(head, body, foot);
    foot.append(close);
    body.append(el("p", "", "Starting the sign-in..."));
    if (!dlg.open) dlg.showModal();
    let si;
    try {
      si = await api("POST", path);
    } catch (e) {
      body.replaceChildren(el("p", "err", "Could not start the sign-in: " + e.message));
      return;
    }
    show(si);

    function show(s) {
      body.replaceChildren();
      foot.replaceChildren();
      body.append(el("p", "claude-who", describe(s.label || account, s.email_hint, s.reason)));
      if (s.state === "succeeded") {
        body.append(el("p", "ok", "Signed in" + (s.email ? " as " + s.email : "") + "."));
        foot.append(close);
        return;
      }
      if (s.state === "failed" || s.state === "cancelled" || s.state === "timed_out") {
        const why = s.state === "cancelled" ? "The sign-in was cancelled." : s.state === "timed_out" ?
          "The sign-in took too long and was stopped." : "The sign-in failed.";
        body.append(el("p", "err", why + (s.message ? " " + s.message : "")));
        const again = el("button", "primary", "Start again");
        again.type = "button";
        again.onclick = () => signIn(backend, account);
        foot.append(again, close);
        return;
      }
      if (s.message) body.append(el("p", "hint", s.message));
      const step1 = el("p", "", "1. Open this link and sign in to Claude with the account named above:");
      body.append(step1);
      if (typeof s.url === "string" && s.url.startsWith("https://")) {
        const a = el("a", "claude-link", "Open the Claude sign-in page");
        a.href = s.url;
        a.target = "_blank";
        a.rel = "noopener noreferrer";
        const line = el("p");
        line.append(a);
        body.append(line);
      } else {
        body.append(el("p", "err", "Hermes has not shown the sign-in link yet. Close this and try again in a moment."));
      }
      body.append(el("p", "", "2. Copy the code the Claude page shows and paste it here:"));
      const input = el("input", "claude-code");
      input.type = "text";
      input.autocomplete = "off";
      input.spellcheck = false;
      input.setAttribute("autocapitalize", "off");
      input.setAttribute("aria-label", "Code from the Claude page");
      const status = el("p", "hint", "");
      status.setAttribute("aria-live", "polite");
      body.append(input, status);
      const send = el("button", "primary", "Send code");
      send.type = "button";
      const cancel = el("button", "danger", "Cancel sign-in");
      cancel.type = "button";
      send.onclick = async () => {
        const code = input.value.trim();
        input.value = "";
        if (!code) { status.textContent = "Paste the code first."; return; }
        send.disabled = cancel.disabled = true;
        status.textContent = "Checking the code...";
        try {
          show(await api("POST", path + "/code", { signin_id: s.id, code }));
        } catch (e) {
          status.textContent = e.message;
          send.disabled = cancel.disabled = false;
        }
      };
      cancel.onclick = async () => {
        send.disabled = cancel.disabled = true;
        try { show(await api("DELETE", path)); } catch (e) {
          status.textContent = "Could not cancel: " + e.message;
          send.disabled = cancel.disabled = false;
        }
      };
      foot.append(cancel, send, close);
      input.focus();
    }
  }

  // panel is the Usage page's "Subscription accounts" section of a
  // backend, or null when its Hermes has neither the claude-accounts plugin
  // nor a ChatGPT plan.
  async function panel(backend) {
    const s = el("section", "usage-sec claude-panel");
    s.append(el("h2", "", "Subscription accounts"));
    let data;
    try {
      data = await api("GET", base(backend) + "?usage=1");
      cache.set(backend, { at: Date.now(), data });
    } catch (e) {
      if (/no Claude accounts plugin|not set up/.test(e.message)) return null;
      s.append(el("p", "err", "Could not read the subscription accounts: " + e.message));
      return s;
    }
    s.append(el("p", "mem-hint", "The Claude and ChatGPT subscription accounts this Hermes can use, with the share of each usage window used. " +
      "Reset times are in " + (data.time_zone || "UTC") + " time. API credits are an estimate: the credit the plan includes each " +
      "month, less what Hermes spent through the account's API key since the month started. Anthropic does not report the balance, " +
      "so use outside Hermes is not counted."));
    if (!(data.accounts || []).length) s.append(el("p", "none", "No accounts."));
    const cards = el("div", "claude-cards");
    const poolCard = data.pool ? poolSummary(data.pool, cards) : null;
    if (poolCard) s.append(poolCard, cards);
    else s.append(cards);
    for (const a of data.accounts || []) {
      const box = el("div", "claude-account");
      const top = el("div", "claude-top");
      top.append(el("strong", "", a.label));
      top.append(el("span", a.signed_in ? "ok" : "err", a.signed_in ? "Signed in" : "Not signed in"));
      box.append(top);
      const who = [];
      if (a.email) who.push((a.signed_in ? "" : "Last signed in as ") + a.email);
      if (a.plan) who.push(a.plan + " plan");
      if (who.length) box.append(el("p", "claude-meta", who.join(", ")));
      if (a.status_error) box.append(el("p", "err", a.status_error));
      const u = a.usage;
      if (u && u.unavailable) box.append(el("p", "hint", "Usage not available: " + u.unavailable));
      const rows = reported(u).map((w) => [w.label || w.kind, pct(w) + " used", w.resets_text ? "resets " + w.resets_text : ""]);
      if (rows.length) {
        const list = el("ul", "claude-windows");
        for (const r of rows) list.append(el("li", "", r.filter(Boolean).join(", ")));
        box.append(list);
      }
      for (const d of (u && u.details) || []) box.append(el("p", "hint", d));
      if (a.credits) {
        const line = el("p", "claude-meta", creditsLine(a.credits));
        line.title = "An estimate that counts only Hermes's own use of this account's API key" +
          (a.credits.unpriced ? "; " + a.credits.unpriced + " calls had no known price and are not counted" : "");
        box.append(line);
      }
      if (data.admin && !a.no_signin) {
        const run = a.signin && (a.signin.state === "waiting_code" || a.signin.state === "starting" || a.signin.state === "verifying");
        const b = el("button", "", run ? "Continue sign-in" : a.signed_in ? "Sign in again" : "Sign in");
        b.type = "button";
        b.onclick = () => signIn(backend, a.id);
        box.append(b);
      }
      (data.pool && !a.no_signin && a.provider !== chatgptProvider ? cards : s).append(box);
    }
    return s;
  }

  const storeKey = "hub.claude.accounts";
  const accountsShown = () => { try { return localStorage.getItem(storeKey) === "open"; } catch (_) { return false; } };

  // shortName is an account's own name, "caam" for "Claude (caam)".
  const shortName = (label) => (/^Claude \((.+)\)$/.exec(label || "") || [])[1] || label;

  // poolSummary is the card at the top of the section: the pool's week as
  // one number, with the per-account cards behind "Show accounts".
  function poolSummary(p, cards) {
    const box = el("div", "claude-account claude-pool");
    const top = el("div", "claude-top");
    top.append(el("strong", "", "Claude pool"), el("span", "claude-meta", "(" + p.accounts + (p.accounts === 1 ? " account)" : " accounts)")));
    box.append(top);
    if (typeof p.used_percent !== "number") {
      box.append(el("p", "hint", "No account's usage could be read (0 of " + p.accounts + " accounts read)."));
    } else {
      const used = Math.round(p.used_percent);
      const row = el("div", "claude-poolrow");
      const bar = el("span", "usage-bar");
      const fill = el("span", "fill");
      fill.style.width = Math.min(100, used) + "%";
      bar.append(fill);
      row.append(el("span", "", "Week: " + used + "% used"), bar);
      box.append(row);
      const room = Math.round(p.room_accounts * 10) / 10;
      box.append(el("p", "claude-meta", "About " + room.toFixed(1) + (room === 1 ? " account" : " accounts") + " of room left"));
      if (p.next_account) {
        box.append(el("p", "claude-meta", "Next room back: " + (p.next_text || "unknown") + " (" + shortName(p.next_account) + " resets)"));
      }
      if (p.read < p.accounts) box.append(el("p", "hint", "(" + p.read + " of " + p.accounts + " accounts read)"));
    }
    const toggle = el("button", "claude-toggle");
    toggle.type = "button";
    const show = (open) => {
      cards.hidden = !open;
      toggle.textContent = open ? "Hide accounts" : "Show accounts";
      toggle.setAttribute("aria-expanded", String(open));
    };
    show(accountsShown());
    toggle.onclick = () => {
      const open = cards.hidden;
      show(open);
      try { localStorage.setItem(storeKey, open ? "open" : "closed"); } catch (_) { /* per-browser only */ }
    };
    box.append(toggle);
    return box;
  }

  return { parts, note, afterReply, chatNotice, signIn, panel, apiName, apiProvider, chatgptProvider, route,
    account: (backend, provider, session, model) => {
      parts(backend, { provider });
      if (provider === poolProvider && session && me().claude) {
        const p = pools.get(backend + "\n" + session);
        if (!p || (!p.loading && (p.model !== model || Date.now() - p.at >= minute))) pool(backend, session, model).then(() => ctx.redraw());
      }
      return provider === poolProvider ? poolAccount(backend) : accountFor(backend, provider, session);
    } };
};

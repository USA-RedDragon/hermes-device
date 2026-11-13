"use strict";
// HubAccount draws the Account section of Settings for the way this hub
// logs people in: who you are, how, Log out, and in auth.mode local a
// form to change your password.
window.HubAccount = (ctx) => {
  const { api, el } = ctx;
  const A = window.HubAuth;

  const kv = (dl, k, v) => {
    if (v === "" || v === null || v === undefined) return null;
    const dd = el("dd", "");
    if (typeof v === "string") dd.textContent = v;
    else dd.append(...v);
    dl.append(el("dt", "", k), dd);
    return dd;
  };

  const code = (t) => el("code", "", t);

  // who says how this login came about, as parts for a line.
  function how(me) {
    switch (me.auth) {
      case "proxy": return me.identity ? ["The proxy in front of this hub, as ", code(me.identity)] : ["The proxy in front of this hub"];
      case "oidc": return [(me.issuer || "Your identity provider") + " (OpenID Connect)"];
      default: return ["Password on this hub"];
    }
  }

  // hint is the line under Log out.
  function hint(me) {
    if (me.auth === "proxy") {
      return me.logout_url
        ? "Log out also logs you out of the proxy (auth.proxy.logout_url). Change your password where you log in to the proxy."
        : "Log out ends your session on this hub. The proxy may log you straight back in; log out of the proxy to stop that. Change your password where you log in to the proxy.";
    }
    if (me.auth === "oidc") return "Change your password at your identity provider.";
    return "";
  }

  // passwordForm is the form of auth.mode local. A change logs out every
  // other browser; this one gets a new session from the hub.
  function passwordForm(me) {
    const f = el("form", "set-form");
    f.style.display = "flex";
    f.style.flexDirection = "column";
    f.style.gap = ".8rem";
    const input = (label, auto, id) => {
      const l = el("label", "field", "");
      const i = el("input");
      Object.assign(i, { type: "password", autocomplete: auto, autocapitalize: "off", spellcheck: false, required: true, id });
      l.append(el("span", "", label), i);
      return { l, i };
    };
    const cur = input("Current password", "current-password", "pw-current");
    const nw = input("New password", "new-password", "pw-new");
    nw.l.append(el("small", "", "At least " + A.MIN + " characters."));
    const again = input("New password again", "new-password", "pw-again");
    const status = el("span", "");
    status.setAttribute("role", "status");
    const go = el("button", "primary", "Change password");
    go.type = "submit";
    const acts = el("div", "acts");
    acts.append(go, status);
    f.append(cur.l, nw.l, again.l, acts);
    const say = (ok, text) => { status.className = ok ? "ok" : "err"; status.textContent = text; status.setAttribute("role", ok ? "status" : "alert"); };
    f.onsubmit = async (e) => {
      e.preventDefault();
      say(true, "");
      const bad = A.passwordProblem(nw.i.value, me.user);
      if (!cur.i.value) { say(false, "Type your current password."); cur.i.focus(); return; }
      if (bad) { say(false, A.sentence(bad)); nw.i.focus(); return; }
      if (nw.i.value !== again.i.value) { say(false, "The two passwords are not the same."); again.i.focus(); return; }
      go.disabled = true;
      try {
        await api("POST", "/api/account/password", { current: cur.i.value, password: nw.i.value });
        cur.i.value = nw.i.value = again.i.value = "";
        say(true, "Saved. Other browsers are logged out.");
        await ctx.refreshMe();
      } catch (x) {
        say(false, A.sentence(x.message));
      }
      go.disabled = false;
    };
    return f;
  }

  // draw fills the pane p. extra rows (default assistant, version) come
  // from the caller as [label, node] pairs.
  function draw(p, me, extra) {
    const dl = el("dl", "kv");
    const name = me.display && me.display !== me.user ? me.display + " (" + me.user + ")" : me.user || me.display || "";
    kv(dl, "Logged in as", name);
    kv(dl, "Login", how(me));
    if (me.auth === "local" || !me.auth) kv(dl, "Role", me.admin ? "Admin" : "Member");
    else if ((me.groups || []).length) kv(dl, "Groups", me.groups.join(", "));
    const rows = {};
    for (const [k, v] of extra) rows[k] = kv(dl, k, v);
    p.append(dl);
    const out = el("div", "acts");
    const lo = el("button", "", "Log out");
    lo.type = "button";
    lo.onclick = () => ctx.logout();
    out.append(lo);
    p.append(out);
    const h = hint(me);
    if (h) p.append(el("p", "hint", h));
    if (me.password) {
      p.append(el("h2", "", "Change password"), passwordForm(me), el("p", "hint", "Two-factor codes are not built yet."));
    }
    return rows;
  }

  return { draw };
};

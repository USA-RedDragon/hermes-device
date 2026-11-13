"use strict";
// HubAuth holds the rules of the login, setup and Users pages that do not
// touch the page: what a good name and password are (the same rules as
// the hub's server, which has the last word), the strength meter, the
// next address after a login, and what an admin may do to a person.
window.HubAuth = (() => {
  const MIN = 12;
  const MAX_BYTES = 1024;
  const namePattern = /^[a-z0-9][a-z0-9._-]{0,63}$/;

  // cleanName is what the hub does to a typed name: trimmed, lowercase.
  const cleanName = (name) => String(name || "").trim().toLowerCase();

  function nameProblem(name) {
    if (namePattern.test(cleanName(name))) return "";
    return "A name has 1 to 64 lowercase letters, digits, dots, dashes or underscores, and starts with a letter or digit";
  }

  const bytes = (s) => new TextEncoder().encode(s).length;
  const chars = (s) => [...s].length;

  // passwordProblem is "" for a password the hub accepts for an account
  // called name.
  function passwordProblem(pw, name) {
    pw = String(pw || "");
    if (bytes(pw) > MAX_BYTES) return "A password may have at most " + MAX_BYTES + " bytes";
    if (chars(pw) < MIN || pw.toLowerCase() === cleanName(name)) return "A password needs at least " + MIN + " characters";
    return "";
  }

  // strength gives the meter: pct of its width and a level for its color.
  // Length is what counts: a few words are as good as a jumble.
  function strength(pw) {
    const n = chars(String(pw || ""));
    return { pct: Math.min(100, Math.round(n * 100 / 30)), level: n < MIN ? "bad" : n < 16 ? "warn" : "ok" };
  }

  // cleanNext keeps only a path on this site, as the hub does.
  function cleanNext(next) {
    if (typeof next !== "string" || !next.startsWith("/") || next.startsWith("//") || next.startsWith("/\\")) return "/";
    return next;
  }

  // sentence ends a message from the hub with a period.
  const sentence = (msg) => (/[.!?]$/.test(msg) ? msg : msg + ".");

  // guards says what an admin may not do to person u while logged in as
  // me. Each is "" when allowed, else the reason. The hub checks the same
  // rules; this only keeps the page from offering what would fail.
  function guards(me, u, users) {
    const self = u.name === me;
    const others = users.filter((x) => x.name !== u.name && x.admin && !x.disabled).length;
    const last = u.admin && !u.disabled && others === 0;
    const own = "You cannot do this to yourself";
    return {
      demote: self ? "You cannot take away your own admin rights" : last ? "The hub needs at least one admin" : "",
      disable: self ? "You cannot disable yourself" : last ? "The hub needs at least one admin" : "",
      remove: self ? "You cannot delete your own account" : last ? "The hub needs at least one admin" : "",
      own,
    };
  }

  // lock says why a backend is on for person u by the config: "" when an
  // admin may change it. u has name and groups.
  function lock(b, u) {
    if (b.everyone) return b.id === "hermes" ? "everyone (hermes.url)" : "everyone (config)";
    if ((b.users || []).includes(u.name)) return "named in the config";
    const g = (b.groups || []).find((x) => (u.groups || []).includes(x));
    return g ? "by group " + g + " (config)" : "";
  }

  const mayUse = (b, u) => !!lock(b, u) || (u.backends || []).includes(b.id);

  // backendNames lists the backends u may use, by name.
  const backendNames = (backends, u) => backends.filter((b) => mayUse(b, u)).map((b) => b.name || b.id);

  // row describes one person's line in the list. fmt gives it the date
  // and time words so tests do not depend on the machine's locale.
  function row(u, ctx) {
    const { me, mode, backends, fmt } = ctx;
    const pills = [];
    if (u.admin) pills.push(["admin", "Admin"]);
    if (u.locked) pills.push(["warn", "Locked"]);
    if (u.disabled) pills.push(["", "Disabled"]);
    if (u.source === "config") pills.push(["", "From config"]);
    const waiting = u.source === "account" && mode !== "local" && !u.identity && !u.password;
    if (waiting) pills.push(["warn", "Not logged in yet"]);
    const names = backendNames(backends, u).join(", ") || "No backends";
    const meta = [];
    const idText = u.identity ? u.identity.replace(/^[a-z]+:/, "") : "";
    if (u.source === "config") {
      meta.push(mode + ": " + idText, "users[] in the config; change it there");
    } else if (waiting) {
      meta.push("waits for " + (u.email || "a login named " + u.name), names);
    } else if (mode !== "local") {
      meta.push(mode + ": " + (u.email || idText), names);
    } else {
      meta.push(names);
      if (u.locked) meta.push((u.failures ? u.failures + " wrong passwords" : "too many wrong passwords") + ", unlocks at " + fmt.time(u.locked_until));
      else if (!u.disabled && u.created) meta.push("since " + fmt.day(u.created));
    }
    const shown = u.display && u.display !== u.name ? u.display : "";
    return {
      name: u.name,
      display: (shown + (u.name === me ? (shown ? " (you)" : "(you)") : "")).trim(),
      state: u.locked ? "lock" : u.disabled || waiting ? "off" : "on",
      pills, meta, waiting, config: u.source === "config",
    };
  }

  // banner is the note above the list that says how people get in: a kind
  // and parts, which are text or {code: text} for a setting's name.
  function banner(mode, login) {
    const l = login || {};
    if (mode === "proxy") {
      const how = l.jwt ? ["a signed token from the proxy"] : [{ code: l.header || "Remote-User" }].concat((l.trusted || []).length ? [" from " + l.trusted.join(", ")] : []);
      return { kind: "info", parts: ["People log in through the proxy in front of this hub (", ...how, "). ",
        l.auto_create ? "New people get an account at their first login (auto_create)." : "Only people in the config or added here can log in."] };
    }
    if (mode === "oidc") {
      const who = [];
      if ((l.domains || []).length) who.push("a verified email at " + l.domains.join(", "));
      if ((l.groups || []).length) who.push("a group in " + l.groups.join(", "));
      return { kind: "warn", parts: ["People log in with your identity provider (" + (l.issuer || "OpenID Connect") + "). ",
        l.auto_create && who.length ? "New people with " + who.join(" or ") + " get an account." : "Only people in the config or added here can log in."] };
    }
    return null;
  }

  return { MIN, MAX_BYTES, cleanName, nameProblem, passwordProblem, strength, cleanNext, sentence, guards, lock, mayUse, backendNames, row, banner };
})();

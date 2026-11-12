"use strict";
// HubSlash.match(commands, query) picks the commands that a typed "/query"
// offers, best first: a command whose name or alias is the query, then one
// that starts with it, then skills that start with it, then names that
// contain it, then descriptions that contain it. Each result is
// {c, alias}: alias is the alias that matched when the name did not.
window.HubSlash = (() => {
  const bare = (n) => String(n || "").replace(/^\//, "").toLowerCase();

  function rank(c, q) {
    const names = [bare(c.name)].concat((c.aliases || []).map(bare));
    if (!q) return { tier: 2, alias: "" };
    const at = (test) => names.findIndex(test);
    let i = at((n) => n === q);
    if (i >= 0) return { tier: 0, alias: i ? c.aliases[i - 1] : "" };
    i = at((n) => n.startsWith(q));
    if (i >= 0) return { tier: c.skill ? 2 : 1, alias: i ? c.aliases[i - 1] : "" };
    i = at((n) => n.includes(q));
    if (i >= 0) return { tier: 3, alias: i ? c.aliases[i - 1] : "" };
    if (String(c.description || "").toLowerCase().includes(q)) return { tier: 4, alias: "" };
    return null;
  }

  function match(commands, query) {
    const q = bare(query);
    const out = [];
    (commands || []).forEach((c, order) => {
      const r = rank(c, q);
      if (r) out.push({ c, alias: r.alias, tier: r.tier, order });
    });
    out.sort((a, b) => a.tier - b.tier || a.order - b.order);
    return out.map((m) => ({ c: m.c, alias: m.alias }));
  }

  return { match };
})();

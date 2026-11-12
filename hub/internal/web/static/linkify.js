"use strict";
// Links the bare http, https and www. addresses of a plain text. It builds
// text nodes and anchors only, and keeps no other scheme, so text can never
// inject HTML or a script link.
window.HubLinkify = (() => {
  const FIND = /(?<![\p{L}\p{N}_/@.])(?:https?:\/\/|www\.)[^\s<>]+/giu;
  const PAIRS = { ")": "(", "]": "[", "}": "{" };

  // trim drops what a sentence puts after an address: closing punctuation,
  // quotes, and a bracket that has no opener inside the address.
  function trim(u) {
    for (;;) {
      const last = u[u.length - 1];
      if (".,;:!?\"'".includes(last)) { u = u.slice(0, -1); continue; }
      const open = PAIRS[last];
      if (open && u.split(open).length < u.split(last).length) { u = u.slice(0, -1); continue; }
      return u;
    }
  }

  function href(u) {
    const full = /^www\./i.test(u) ? "https://" + u : u;
    try {
      const url = new URL(full);
      if (url.protocol !== "http:" && url.protocol !== "https:") return null;
      if (!url.hostname.includes(".") && !/^https?:\/\/localhost\b/i.test(full)) return null;
      return url.href;
    } catch (_) {
      return null;
    }
  }

  // split cuts text into strings and {text, href} links.
  function split(text) {
    const out = [];
    let at = 0;
    for (const m of text.matchAll(FIND)) {
      const u = trim(m[0]);
      const h = href(u);
      if (!h) continue;
      if (m.index > at) out.push(text.slice(at, m.index));
      out.push({ text: u, href: h });
      at = m.index + u.length;
    }
    if (at < text.length) out.push(text.slice(at));
    return out;
  }

  function append(parent, text) {
    for (const p of split(text)) {
      if (typeof p === "string") {
        parent.append(document.createTextNode(p));
        continue;
      }
      const a = document.createElement("a");
      a.className = "autolink";
      a.href = p.href;
      a.target = "_blank";
      a.rel = "noopener noreferrer";
      a.textContent = p.text;
      parent.append(a);
    }
    return parent;
  }

  return { split, append };
})();

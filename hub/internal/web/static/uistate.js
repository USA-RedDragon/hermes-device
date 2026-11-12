"use strict";
// HubUIState keeps what the reader opened or closed in a chat's log (tool
// groups, tool cards, notice cards, "Show full result", thinking blocks)
// until the reader changes it, whatever the page does to the nodes after
// that: a snapshot that draws the chat again, cards regrouped, a turn that
// ends. Each such control belongs to an owner node with a data-ukey that
// does not change when the node is drawn again (a tool call's ID, a hash
// of a notice's text), and is known by its place among the owner's
// controls.
window.HubUIState = (log) => {
  const saved = new Map();
  let applying = false;
  const sel = "summary, [aria-expanded], .notice-more";

  const hash = (s) => {
    let h = 5381;
    for (let i = 0; i < s.length; i++) h = ((h * 33) ^ s.charCodeAt(i)) >>> 0;
    return h.toString(36);
  };

  // own gives a thinking block its key from the start of its text, once
  // it has some.
  const own = (d) => {
    if (d.dataset.ukey) return d.dataset.ukey;
    if (!d.classList.contains("reasoning")) return "";
    const text = (d.textContent || "").slice(0, 120);
    if (text.length < 20) return "";
    d.dataset.ukey = "r:" + hash(text);
    return d.dataset.ukey;
  };

  const owner = (n) => {
    for (let o = n.closest("details.reasoning, [data-ukey]"); o; o = o.parentElement && o.parentElement.closest("details.reasoning, [data-ukey]")) {
      if (own(o)) return o;
    }
    return null;
  };

  const controls = (o) => Array.from(o.querySelectorAll(sel)).filter((c) => owner(c) === o && (c.tagName !== "SUMMARY" || c.parentNode === o));
  const keyOf = (c) => {
    const o = owner(c);
    if (!o) return "";
    const i = controls(o).indexOf(c);
    return i < 0 ? "" : own(o) + "|" + i;
  };
  const read = (c) => {
    if (c.tagName === "SUMMARY") return c.parentNode.open;
    if (c.classList.contains("notice-more")) return !/^Show (full|more)/.test(c.textContent);
    return c.getAttribute("aria-expanded") === "true";
  };
  const write = (c, v) => {
    if (read(c) === v) return;
    if (c.tagName === "SUMMARY") c.parentNode.open = v;
    else c.click();
  };

  // record is for a click in the log: it keeps what the click left.
  function record(e) {
    if (applying) return;
    const c = e.target.closest && e.target.closest(sel);
    if (!c || !log.contains(c)) return;
    setTimeout(() => {
      const k = keyOf(c);
      if (k) saved.set(k, read(c));
    }, 0);
  }

  // apply puts the kept state on root and what is inside it.
  function apply(root) {
    if (!saved.size || root.nodeType !== 1) return;
    applying = true;
    try {
      const owners = Array.from(root.querySelectorAll("[data-ukey], details.reasoning"));
      if (root.dataset.ukey || root.matches("details.reasoning")) owners.unshift(root);
      for (const o of owners) {
        const k = own(o);
        if (!k) continue;
        controls(o).forEach((c, i) => {
          const v = saved.get(k + "|" + i);
          if (v !== undefined) write(c, v);
        });
      }
    } finally {
      applying = false;
    }
  }

  // closeUnlessKept closes a thinking block the page closes by itself,
  // when the reader has not opened or closed it.
  function closeUnlessKept(d) {
    const k = own(d);
    const v = k ? saved.get(k + "|0") : undefined;
    if (v === undefined) d.open = false;
  }

  const expanded = (n) => {
    const h = n.querySelector && n.querySelector("[aria-expanded]");
    return !!h && h.getAttribute("aria-expanded") === "true";
  };

  return { busy: () => applying, record, apply, closeUnlessKept, expanded, hash, saved };
};

"use strict";
// HubWorksWith is the "Works with" setting: the chats the user marked as
// working together (the hub lets such a pair message each other in chains
// of up to 30 back-and-forths, not 6). It adds a row to a chat's options
// menu that opens a panel (a bottom sheet on a phone) to list, add and
// remove partners; a link icon next to the partners of the open chat in
// the list; chips on the card of an agent message (the pair, and the hop
// of the chain); and a link under a refusal that tells the agent the user
// can mark chats as working together.
// The pure helpers (countText, hopText, chips, mentions, partnersOf,
// candidates) run against internal/web/testdata/workswith/run.js.
// ctx: api, el, toast, banner(text, bad), me(), backends() and
// sessions() (the chat list), where(s) (a chat's computer and folder),
// current() ({ b, s } of the open chat or null), redraw() (the chat
// list), phone() (true when the panel is a sheet).
window.HubWorksWith = (() => {
  const note = "These chats may message each other in longer chains: up to 30 back-and-forths, not 6.";
  const refusalRE = /mark these chats as working together/i;
  const idRE = /^am_[0-9a-f]{16}$/;

  // countText is the right side of the menu row.
  const countText = (n) => (n > 0 ? n + (n === 1 ? " chat" : " chats") : "None");
  // hopText is the hop chip, or "" when the hop is not known.
  const hopText = (info) => (info && info.hop > 0 && info.max_hop > 0 ? "hop " + info.hop + " of " + info.max_hop : "");
  // chips lists the chips under an agent message's sender.
  function chips(info) {
    const out = [];
    if (!info) return out;
    if (info.partner) out.push({ kind: "partner", text: "Works with this chat", icon: "link" });
    const h = hopText(info);
    if (h) out.push({ kind: "hop", text: h });
    return out;
  }
  // mentions reports whether a refusal says the user can mark chats as
  // working together.
  const mentions = (text) => refusalRE.test(String(text || ""));
  // partnersOf lists the session IDs that work with id.
  function partnersOf(pairs, id) {
    const out = [];
    for (const p of pairs || []) {
      if (p.a === id) out.push(p.b);
      else if (p.b === id) out.push(p.a);
    }
    return out;
  }
  // candidates are the chats the picker offers for chat self of a backend:
  // the others that are not archived or already partners, best match of the
  // query first, then the newest.
  function candidates(sessions, backend, self, partners, query) {
    const q = String(query || "").trim().toLowerCase();
    const have = new Set(partners);
    const hit = (s) => !q || (s.title || "New chat").toLowerCase().includes(q);
    return (sessions || [])
      .filter((s) => s.backend_id === backend && s.id !== self && !s.archived && !have.has(s.id) && hit(s))
      .sort((x, y) => Date.parse(y.updated_at || 0) - Date.parse(x.updated_at || 0));
  }

  function create(ctx) {
    const { api, el } = ctx;
    const I = window.HubIcons;
    const pairs = new Map(); // backend -> pairs; a backend without the setting has no entry
    const seen = new Map(); // backend/session/id -> info of an agent message
    const cards = new Set(); // { box, id, backend, session }
    let queue = new Set();
    let timer = 0;
    let layer = null;
    let opener = null;
    let layerID = 0;

    const base = (b) => "/api/backends/" + encodeURIComponent(b) + "/partners";
    const enabled = () => !!(ctx.me() && ctx.me().partners);
    const available = (b) => pairs.has(b);
    const mine = (b, id) => partnersOf(pairs.get(b), id);

    // load reads the pairs of each backend; a backend that answers an
    // error has no such setting (several users, or no owner here).
    async function load() {
      if (!enabled()) return;
      const ids = (ctx.backends() || []).map((b) => b.id);
      await Promise.all(ids.map(async (b) => {
        try {
          pairs.set(b, (await api("GET", base(b))) || []);
        } catch (e) {
          if (e.status === 404 || e.status === 403) pairs.delete(b);
        }
      }));
      ctx.redraw();
      decorateAll();
    }

    // isPartner: is s a partner of the open chat? That is the chat the
    // list marks with a link icon.
    function isPartner(s) {
      const cur = ctx.current();
      return !!cur && cur.b === s.backend_id && cur.s !== s.id && mine(s.backend_id, cur.s).includes(s.id);
    }

    function mark(s) {
      if (!isPartner(s)) return [];
      const m = el("span", "ww-mark");
      m.title = "Works with the open chat";
      m.append(I.icon("link"));
      return [m];
    }

    // menuItems is the Works with row of a chat's options menu.
    function menuItems(s, close) {
      if (!enabled() || !available(s.backend_id)) return [];
      const b = el("button", "ww-row-btn");
      b.type = "button";
      b.setAttribute("role", "menuitem");
      b.append(I.icon("link"), el("span", "", "Works with"), el("span", "r", countText(mine(s.backend_id, s.id).length)));
      b.onclick = (e) => { e.stopPropagation(); close(); open(s, b); };
      return [b];
    }

    // Agent message cards: the chips come from the hub, which knows the
    // hop and the sender; the card text does not carry them.
    function decorate(box, id, backend, session) {
      if (!enabled() || !idRE.test(id || "")) return;
      cards.add({ box, id, backend, session });
      ask(backend, session, id);
    }

    function decorateAll() {
      for (const c of cards) {
        if (!c.box.isConnected) { cards.delete(c); continue; }
        seen.delete(c.backend + "\n" + c.session + "\n" + c.id);
        ask(c.backend, c.session, c.id);
      }
    }

    function ask(backend, session, id) {
      const key = backend + "\n" + session + "\n" + id;
      if (seen.has(key)) { paint(key); return; }
      queue.add(key);
      if (!timer) timer = setTimeout(flush, 60);
    }

    async function flush() {
      timer = 0;
      const keys = [...queue];
      queue = new Set();
      const groups = new Map();
      for (const k of keys) {
        const [b, s, id] = k.split("\n");
        const g = b + "\n" + s;
        if (!groups.has(g)) groups.set(g, []);
        groups.get(g).push(id);
      }
      for (const [g, ids] of groups) {
        const [b, s] = g.split("\n");
        for (let i = 0; i < ids.length; i += 50) {
          const part = ids.slice(i, i + 50);
          try {
            const got = await api("GET", "/api/backends/" + encodeURIComponent(b) + "/agent-messages?session=" +
              encodeURIComponent(s) + "&ids=" + part.join(","));
            for (const id of part) {
              seen.set(b + "\n" + s + "\n" + id, got && got[id] ? got[id] : null);
              paint(b + "\n" + s + "\n" + id);
            }
          } catch (_) {
            /* the card shows without chips */
          }
        }
      }
    }

    function paint(key) {
      const info = seen.get(key);
      for (const c of cards) {
        if (c.backend + "\n" + c.session + "\n" + c.id !== key || !c.box.isConnected) continue;
        const head = c.box.querySelector(".notice-head");
        if (!head) continue;
        for (const old of head.querySelectorAll(".ww-chips")) old.remove();
        const list = chips(info);
        if (!list.length) continue;
        const row = el("span", "ww-chips");
        for (const x of list) {
          const chip = el("span", "ww-chip " + x.kind);
          if (x.icon) chip.append(I.icon(x.icon));
          chip.append(x.text);
          row.append(chip);
        }
        head.append(row);
      }
    }

    // refusal adds the link to this setting under the card of a message
    // that the hub refused because of the limits.
    function refusal(card, text, backend, session) {
      if (!enabled() || !mentions(text) || !available(backend) || card.querySelector(".ww-refusal")) return;
      const row = el("div", "ww-refusal");
      const a = el("button", "link", "Mark these chats as working together");
      a.type = "button";
      a.prepend(I.icon("link"));
      a.onclick = () => {
        const s = (ctx.sessions() || []).find((x) => x.backend_id === backend && x.id === session) || { backend_id: backend, id: session };
        open(s, a);
      };
      row.append(a);
      card.append(row);
    }

    // The panel, or on a phone the sheet.
    function close() {
      if (!layer) return;
      layer.remove();
      layer = null;
      document.removeEventListener("keydown", onKey, true);
      const o = opener;
      opener = null;
      if (o && o.isConnected) o.focus();
    }

    function onKey(e) {
      if (e.key !== "Escape") return;
      e.preventDefault();
      e.stopPropagation();
      close();
    }

    const titleOf = (s) => (s && s.title) || "New chat";

    function open(s, from) {
      close();
      opener = from || null;
      const id = "ww-title-" + ++layerID;
      layer = el("div", "ww-layer");
      const bg = el("div", "ww-bg");
      bg.onclick = close;
      const panel = el("div", "ww-panel");
      panel.setAttribute("role", "dialog");
      panel.setAttribute("aria-modal", "true");
      panel.setAttribute("aria-labelledby", id);
      layer.append(bg, panel);
      document.body.append(layer);
      document.addEventListener("keydown", onKey, true);
      const st = { pick: !ctx.phone(), query: "", busy: false, err: "" };
      draw(panel, s, id, st);
      const first = panel.querySelector(".ww-close");
      if (first) first.focus();
    }

    function draw(panel, s, id, st) {
      const backend = s.backend_id;
      const mineIDs = mine(backend, s.id);
      const all = ctx.sessions() || [];
      const redo = () => draw(panel, s, id, st);
      const head = el("header", "ww-head");
      const h = el("b", "", "Works with");
      h.id = id;
      const x = I.button("close", "Close", "ghost ib ww-close");
      x.onclick = close;
      head.append(el("span", "ww-grab"), h, x);
      const intro = el("p", "ww-note", note);
      const list = el("div", "ww-list");
      for (const pid of mineIDs) {
        const p = all.find((y) => y.backend_id === backend && y.id === pid);
        const row = el("div", "ww-row");
        const t = el("div", "t");
        t.append(el("b", "", p ? titleOf(p) : "A chat that is gone"));
        const where = p ? ctx.where(p) : "";
        if (where) t.append(el("span", "", where));
        const rm = I.button("close", "Remove " + (p ? titleOf(p) : "this chat"), "ghost ib ww-rm");
        rm.disabled = st.busy;
        rm.onclick = () => change(panel, s, st, "DELETE", base(backend) + "/" + encodeURIComponent(s.id) + "/" + encodeURIComponent(pid), undefined, redo);
        row.append(I.icon("message"), t, rm);
        list.append(row);
      }
      if (!mineIDs.length) list.append(el("p", "ww-none", "No chats marked yet."));
      panel.replaceChildren(head, intro, list);
      if (st.err) panel.append(el("p", "ww-err", st.err));
      if (!st.pick) {
        const add = el("button", "ww-add");
        add.type = "button";
        add.append(I.icon("plus"), "Add a chat");
        add.onclick = () => { st.pick = true; redo(); const q = panel.querySelector(".ww-q input"); if (q) q.focus(); };
        panel.append(add);
        return;
      }
      const picker = el("div", "ww-picker");
      const q = el("label", "ww-q");
      const input = el("input");
      input.type = "search";
      input.placeholder = "Search chats";
      input.value = st.query;
      input.autocomplete = "off";
      input.setAttribute("aria-label", "Search chats to add");
      q.append(I.icon("search"), input);
      const opts = el("div", "ww-opts");
      const fill = () => {
        opts.replaceChildren();
        const found = candidates(all, backend, s.id, mineIDs, st.query);
        for (const c of found.slice(0, 30)) {
          const b = el("button", "ww-row ww-opt");
          b.type = "button";
          b.disabled = st.busy;
          const t = el("span", "t");
          t.append(el("b", "", titleOf(c)));
          const where = ctx.where(c);
          if (where) t.append(el("span", "", where));
          b.append(I.icon("message"), t);
          b.onclick = () => change(panel, s, st, "POST", base(backend), { a: s.id, b: c.id }, redo);
          opts.append(b);
        }
        if (!found.length) opts.append(el("p", "ww-none", st.query ? "No chat matches." : "No other chats to add."));
      };
      input.oninput = () => { st.query = input.value; fill(); };
      fill();
      picker.append(q, opts);
      panel.append(picker);
    }

    async function change(panel, s, st, method, path, body, redo) {
      st.busy = true;
      st.err = "";
      redo();
      try {
        await api(method, path, body);
        pairs.set(s.backend_id, (await api("GET", base(s.backend_id))) || []);
        ctx.redraw();
        decorateAll();
      } catch (e) {
        st.err = "Could not save: " + e.message;
      }
      st.busy = false;
      if (layer && layer.contains(panel)) redo();
    }

    return { load, open, close, mark, menuItems, decorate, refusal, available, isPartner, isOpen: () => !!layer };
  }

  return { create, countText, hopText, chips, mentions, partnersOf, candidates };
})();

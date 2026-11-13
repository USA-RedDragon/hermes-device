"use strict";
// HubPinOrder lets the user reorder pinned chats: drag a row with the
// mouse, press and hold a row on a touch screen, or use Move up and Move
// down in the row's menu. The same drag pins a chat dragged from Recent
// into Pinned, at the place where it is dropped, and unpins a pinned chat
// dropped onto Recent; a drop anywhere else cancels. Pin and Unpin in the
// row's menu stay the keyboard way. It uses pointer events, not the HTML5
// drag API, so one code path serves mouse, pen and touch.
// ctx: api, el, toast(text), banner(text, bad), pinned() (the pinned
// chats in the order shown), find(key) (a chat by its row key), unpin(s),
// redraw(), refresh(), nav() (the scrolling list), onMoved(s, byKeyboard)
// (called after a redraw for a chat that moved).
// hubPinPlan says what a drop does. from is the group the row came from
// ("pinned" or "recent"), zone is the group it was dropped on
// ("pinned", "recent", or null outside both), index is the place among
// the pinned rows other than the dragged one, key is the dragged row and
// pinned lists the pinned keys in the order shown. It returns
// {kind: "none"}, {kind: "reorder" | "pin", order, index} with the new
// order of the pinned keys, or {kind: "unpin"}.
const hubPinPlan = (from, zone, index, key, pinned) => {
  const rest = pinned.filter((k) => k !== key);
  const at = Math.max(0, Math.min(index, rest.length));
  if (zone === "pinned") {
    const order = rest.slice();
    order.splice(at, 0, key);
    if (from === "pinned") {
      const same = order.length === pinned.length && order.every((k, i) => k === pinned[i]);
      return same ? { kind: "none" } : { kind: "reorder", order, index: at };
    }
    return { kind: "pin", order, index: at };
  }
  if (zone === "recent" && from === "pinned") return { kind: "unpin" };
  return { kind: "none" };
};

window.HubPinOrder = (ctx) => {
  const { api, el } = ctx;
  const HOLD_MS = 400; // a touch must rest this long before a drag starts
  const SLOP_MOUSE = 5; // px a mouse moves before a click becomes a drag
  const SLOP_TOUCH = 10; // px a finger may drift during the hold
  const EDGE = 48; // px from the list's edge where it scrolls
  const MAX_SCROLL = 14; // px per frame at the very edge

  let drag = null; // the gesture in progress, from press to release
  let held = false; // a drag just ended: swallow the click that follows
  let redrawLater = false;

  const keyOf = (s) => s.backend_id + "\n" + s.id;

  const active = () => !!(drag && drag.on);

  // save stores the new order. Positions are set locally first so the
  // list shows it at once; a failure reloads what the server has.
  // add, when given, is a chat that is pinned by the same request.
  async function save(list, add) {
    list.forEach((s, i) => { s.pin_pos = i; });
    const ref = (s) => ({ backend_id: s.backend_id, session_id: s.id });
    try {
      await api("PUT", "/api/pins/order", { order: list.map(ref), ...(add ? { pin: ref(add) } : {}) });
    } catch (e) {
      ctx.banner("Could not save the order: " + e.message, true);
    }
    ctx.refresh();
  }

  // move shifts chat s by delta places among the pinned chats.
  async function move(s, delta) {
    const list = ctx.pinned().slice();
    const i = list.findIndex((x) => keyOf(x) === keyOf(s));
    const j = i + delta;
    if (i < 0 || j < 0 || j >= list.length) return;
    list.splice(j, 0, list.splice(i, 1)[0]);
    ctx.toast("Moved " + (delta < 0 ? "up" : "down") + ": place " + (j + 1) + " of " + list.length);
    ctx.redraw();
    ctx.onMoved(s, true);
    await save(list);
  }

  // items are the Move up and Move down entries for a chat's menu: none
  // unless the chat is pinned.
  function items(s, close) {
    if (!s.pinned) return [];
    const list = ctx.pinned();
    const i = list.findIndex((x) => keyOf(x) === keyOf(s));
    if (i < 0) return [];
    const b = (text, delta, off) => {
      const x = el("button", "", text);
      x.type = "button";
      x.setAttribute("role", "menuitem");
      x.disabled = off;
      x.onclick = (e) => { e.stopPropagation(); close(); move(s, delta); };
      return x;
    };
    return [b("Move up", -1, i === 0), b("Move down", 1, i === list.length - 1)];
  }

  const rowsOf = (group) => Array.from(group.querySelectorAll(":scope > .item"));

  function lineAt(group, rows, index) {
    const g = group.getBoundingClientRect();
    const head = group.querySelector(":scope > .group-head");
    let y;
    if (!rows.length) y = head ? head.getBoundingClientRect().bottom - g.top : 0;
    else if (index >= rows.length) y = rows[rows.length - 1].getBoundingClientRect().bottom - g.top;
    else y = rows[index].getBoundingClientRect().top - g.top;
    drag.line.style.top = Math.round(y - 1) + "px";
  }

  // Where the dragged row would land: how many of the other rows lie
  // above the pointer.
  function targetIndex() {
    let n = 0;
    for (const r of drag.others) {
      const b = r.getBoundingClientRect();
      if (drag.y > b.top + b.height / 2) n++;
    }
    return n;
  }

  // zoneAt is the group the pointer is over: "pinned", "recent", or null.
  function zoneAt() {
    const nav = ctx.nav();
    const n = nav.getBoundingClientRect();
    if (drag.x < n.left || drag.x > n.right || drag.y < n.top || drag.y > n.bottom) return null;
    // The gap between two groups belongs half to each, so a drop just
    // below the last pinned row still counts as Pinned.
    const gs = Array.from(nav.querySelectorAll(":scope > .group")).map((g) => ({ g, r: g.getBoundingClientRect() }));
    for (let i = 0; i < gs.length; i++) {
      const { g, r } = gs[i];
      const top = i ? (gs[i - 1].r.bottom + r.top) / 2 : r.top;
      const bottom = i < gs.length - 1 ? (r.bottom + gs[i + 1].r.top) / 2 : r.bottom;
      if (drag.y < top || drag.y > bottom) continue;
      if (g.classList.contains("pinned")) return "pinned";
      if (g.classList.contains("recent")) return "recent";
    }
    return null;
  }

  function place() {
    const nav = ctx.nav();
    const n = nav.getBoundingClientRect();
    // The row follows the pointer, kept inside the list's content so the
    // scroll area does not grow.
    const top = drag.row.getBoundingClientRect().top - drag.dy;
    const h = drag.row.offsetHeight;
    const lo = n.top - nav.scrollTop - top;
    const hi = lo + nav.scrollHeight - h;
    drag.dy = Math.min(hi, Math.max(lo, drag.y - drag.grab - top));
    drag.row.style.transform = "translateY(" + drag.dy + "px)";
    drag.zone = zoneAt();
    drag.recent.forEach((g) => g.classList.toggle("drop-here", drag.zone === "recent" && drag.from === "pinned"));
    drag.line.hidden = drag.zone !== "pinned";
    if (drag.zone === "pinned") {
      drag.target = targetIndex();
      lineAt(drag.pins, drag.others, drag.target);
    }
  }

  function frame() {
    drag.raf = 0;
    if (!active()) return;
    const nav = ctx.nav();
    const b = nav.getBoundingClientRect();
    let v = 0;
    if (drag.y < b.top + EDGE) v = -MAX_SCROLL * Math.min(1, (b.top + EDGE - drag.y) / EDGE);
    else if (drag.y > b.bottom - EDGE) v = MAX_SCROLL * Math.min(1, (drag.y - (b.bottom - EDGE)) / EDGE);
    if (v) {
      const before = nav.scrollTop;
      nav.scrollTop += v;
      if (nav.scrollTop !== before) place();
      drag.raf = requestAnimationFrame(frame);
    }
  }

  function begin() {
    const d = drag;
    d.on = true;
    const nav = ctx.nav();
    // With no pins there is no Pinned group yet: show an empty one to drop on.
    d.made = null;
    d.pins = nav.querySelector(":scope > .group.pinned");
    if (!d.pins) {
      d.pins = el("div", "group pinned");
      const h = el("div", "group-head");
      h.append(el("span", "grow", "Pinned"));
      d.pins.append(h);
      nav.insertBefore(d.pins, nav.firstChild);
      d.made = d.pins;
    }
    d.recent = Array.from(nav.querySelectorAll(":scope > .group.recent"));
    d.others = rowsOf(d.pins).filter((r) => r !== d.row);
    d.line = el("div", "pin-drop");
    d.line.setAttribute("aria-hidden", "true");
    d.pins.append(d.line);
    d.row.classList.add("lifting");
    document.body.classList.add("pin-dragging");
    d.dy = 0;
    d.grab = d.y0 - d.row.getBoundingClientRect().top;
    if (navigator.vibrate) { try { navigator.vibrate(12); } catch (_) { /* not allowed here */ } }
    place();
    frame();
  }

  function stop(commit) {
    const d = drag;
    if (!d) return;
    drag = null;
    clearTimeout(d.timer);
    cancelAnimationFrame(d.raf);
    window.removeEventListener("pointermove", onMove, true);
    window.removeEventListener("pointerup", onUp, true);
    window.removeEventListener("pointercancel", onCancel, true);
    window.removeEventListener("keydown", onKey, true);
    if (!d.on) return;
    held = true;
    setTimeout(() => { held = false; }, 60);
    d.row.classList.remove("lifting");
    d.row.style.transform = "";
    d.line.remove();
    if (d.made) d.made.remove();
    d.recent.forEach((g) => g.classList.remove("drop-here"));
    document.body.classList.remove("pin-dragging");
    const list = ctx.pinned().slice();
    const p = commit ? hubPinPlan(d.from, d.zone, d.target, d.key, list.map(keyOf)) : { kind: "none" };
    const s = ctx.find(d.key);
    if (s && p.kind === "unpin") {
      ctx.unpin(s);
    } else if (s && (p.kind === "reorder" || p.kind === "pin")) {
      const byKey = new Map(list.map((x) => [keyOf(x), x]));
      byKey.set(d.key, s);
      const next = p.order.map((k) => byKey.get(k));
      if (p.kind === "pin") s.pinned = true;
      next.forEach((x, i) => { x.pin_pos = i; });
      ctx.redraw();
      ctx.onMoved(s, false);
      save(next, p.kind === "pin" ? s : null);
    } else if (redrawLater) {
      ctx.redraw();
    }
    redrawLater = false;
  }

  function onMove(e) {
    if (!drag || e.pointerId !== drag.id) return;
    drag.y = e.clientY;
    drag.x = e.clientX;
    if (!drag.on) {
      const moved = Math.hypot(e.clientX - drag.x0, e.clientY - drag.y0);
      if (drag.touch) {
        if (moved > SLOP_TOUCH) stop(false);
      } else if (moved > SLOP_MOUSE) {
        begin();
      }
      return;
    }
    e.preventDefault();
    place();
    if (!drag.raf) drag.raf = requestAnimationFrame(frame);
  }

  const onUp = (e) => { if (drag && e.pointerId === drag.id) stop(true); };
  const onCancel = (e) => { if (drag && e.pointerId === drag.id) stop(false); };
  const onKey = (e) => {
    if (e.key === "Escape" && active()) { e.preventDefault(); e.stopPropagation(); stop(false); }
  };

  function down(e, group) {
    if (drag || (e.pointerType === "mouse" && e.button !== 0) || e.isPrimary === false) return;
    const row = e.target.closest && e.target.closest(".item");
    if (!row || row.parentNode !== group || e.target.closest(".more")) return;
    const touch = e.pointerType === "touch";
    drag = { id: e.pointerId, group, row, key: row.dataset.key, touch, on: false, x0: e.clientX, y0: e.clientY, y: e.clientY, x: e.clientX,
      from: group.classList.contains("pinned") ? "pinned" : "recent", zone: null, target: 0, raf: 0, timer: 0, dy: 0, grab: 0 };
    if (touch) drag.timer = setTimeout(() => { if (drag && !drag.on) begin(); }, HOLD_MS);
    window.addEventListener("pointermove", onMove, true);
    window.addEventListener("pointerup", onUp, true);
    window.addEventListener("pointercancel", onCancel, true);
    window.addEventListener("keydown", onKey, true);
  }

  // The click that ends a drag must not open the chat.
  document.addEventListener("click", (e) => {
    if (held) { e.preventDefault(); e.stopPropagation(); }
  }, true);

  // enable makes the rows of the Pinned group, or of a Recent group,
  // draggable.
  function enable(group) {
    group.addEventListener("pointerdown", (e) => down(e, group));
    group.addEventListener("dragstart", (e) => e.preventDefault());
    group.addEventListener("contextmenu", (e) => { if (drag && (drag.touch || drag.on)) e.preventDefault(); });
    // A touch can only be kept from scrolling the list in touchmove, and
    // only a handler that is not passive may do it.
    group.addEventListener("touchmove", (e) => { if (active() && e.cancelable) e.preventDefault(); }, { passive: false });
  }

  // defer holds a redraw back while a drag runs; it returns true then.
  function defer() {
    if (!active()) return false;
    redrawLater = true;
    return true;
  }

  return { enable, items, active, defer };
};
window.HubPinOrder.plan = hubPinPlan;

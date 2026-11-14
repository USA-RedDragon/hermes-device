"use strict";
// HubSidebars lets the reader drag the chat list and the Activity panel to
// a width they like, on a desktop. Each width lives in localStorage, so each
// browser keeps its own. The chat column keeps at least chatMin pixels. A
// side that is an overlay (phones, narrow windows) has no handle and its saved
// width is ignored.
(function () {
  const CFG = {
    left: { min: 192, max: 448, def: 272, key: "hub.width.left" },
    right: { min: 288, max: 640, def: 352, key: "hub.width.right" },
  };
  const chatMin = 384;
  const small = 16;
  const big = 64;
  const other = (s) => (s === "left" ? "right" : "left");

  // create(env) returns the controller. env gives width() of the layout and
  // shown(side), whether the side takes room beside the chat now; storage is
  // localStorage or anything with getItem and setItem.
  function create(env) {
    const w = { left: CFG.left.def, right: CFG.right.def };
    for (const s of ["left", "right"]) {
      try {
        const v = Number(env.storage.getItem(CFG[s].key));
        if (Number.isFinite(v) && v >= CFG[s].min && v <= CFG[s].max) w[s] = Math.round(v);
      } catch (e) { /* storage may be blocked */ }
    }
    const room = (s, o) => (env.shown(other(s)) ? o : 0);
    // limits of side s when the other side is oth pixels wide.
    function bounds(s, oth) {
      const c = CFG[s];
      const hi = Math.min(c.max, env.width() - room(s, oth) - chatMin);
      return [c.min, Math.max(c.min, hi)];
    }
    const clamp = (s, v, oth) => {
      const [a, b] = bounds(s, oth);
      return Math.round(Math.max(a, Math.min(b, v)));
    };
    // eff is what is drawn: the saved width held inside the limits.
    function eff() {
      const r = clamp("right", w.right, w.left);
      return { left: clamp("left", w.left, r), right: r };
    }
    function limits(s) { return bounds(s, eff()[other(s)]); }
    function save(s) {
      try { env.storage.setItem(CFG[s].key, String(w[s])); } catch (e) { /* ignore */ }
    }
    function set(s, v) { w[s] = clamp(s, v, eff()[other(s)]); }
    function reset(s) { set(s, CFG[s].def); save(s); }
    // drag moves side s to v, the pointer's distance from that side's outer edge.
    function drag(s, v) { set(s, v); }
    // key handles a key press on side s's handle; true if it was one of ours.
    function key(s, k, shift) {
      const dir = s === "left" ? 1 : -1;
      const cur = eff()[s];
      const step = shift ? big : small;
      let v;
      if (k === "ArrowRight") v = cur + step * dir;
      else if (k === "ArrowLeft") v = cur - step * dir;
      else if (k === "Home") v = limits(s)[0];
      else if (k === "End") v = limits(s)[1];
      else if (k === "Enter") v = CFG[s].def;
      else return false;
      set(s, v);
      save(s);
      return true;
    }
    return { cfg: CFG, widths: w, eff, limits, set, save, reset, drag, key };
  }

  // attach wires the controller to the page: the grid variables, the two
  // handles and their pointer and key events.
  function attach(doc, win) {
    const app = doc.getElementById("app");
    const hs = { left: doc.getElementById("rs-left"), right: doc.getElementById("rs-right") };
    if (!app || !hs.left || !hs.right) return null;
    const mq = { left: win.matchMedia("(max-width: 760px)"), right: win.matchMedia("(max-width: 900px)") };
    const act = () => !app.classList.contains("act-closed");
    // The layout's width is read once and again after it changes: a read
    // right after the page wrote to the DOM forces a layout.
    let cw = -1;
    if (typeof win.ResizeObserver === "function") new win.ResizeObserver(() => { cw = -1; }).observe(app);
    else if (win.addEventListener) win.addEventListener("resize", () => { cw = -1; });
    const ctl = create({
      storage: win.localStorage,
      width: () => (cw >= 0 ? cw : (cw = app.clientWidth)),
      shown: (s) => (s === "left" ? !mq.left.matches : !mq.right.matches && act()),
    });
    function paint() {
      const e = ctl.eff();
      app.style.setProperty("--wl", e.left + "px");
      app.style.setProperty("--wr", e.right + "px");
      for (const s of ["left", "right"]) {
        const h = hs[s];
        const [a, b] = ctl.limits(s);
        h.setAttribute("aria-valuemin", a);
        h.setAttribute("aria-valuemax", b);
        h.setAttribute("aria-valuenow", e[s]);
        const tip = h.querySelector(".tip");
        if (tip) tip.textContent = e[s] + " px" + (e[s] <= a ? " \u00b7 min" : e[s] >= b ? " \u00b7 max" : "");
      }
    }
    for (const s of ["left", "right"]) {
      const h = hs[s];
      h.addEventListener("pointerdown", (ev) => {
        if (ev.button) return;
        h.setPointerCapture(ev.pointerId);
        h.classList.add("drag");
        app.classList.add("dragging");
        ev.preventDefault();
      });
      h.addEventListener("pointermove", (ev) => {
        if (!h.hasPointerCapture(ev.pointerId)) return;
        const r = app.getBoundingClientRect();
        ctl.drag(s, s === "left" ? ev.clientX - r.left : r.right - ev.clientX);
        paint();
      });
      const end = () => {
        if (!h.classList.contains("drag")) return;
        h.classList.remove("drag");
        app.classList.remove("dragging");
        ctl.save(s);
      };
      h.addEventListener("pointerup", end);
      h.addEventListener("pointercancel", end);
      h.addEventListener("dblclick", () => { ctl.reset(s); paint(); });
      h.addEventListener("keydown", (ev) => {
        if (ev.altKey || ev.ctrlKey || ev.metaKey) return;
        if (ctl.key(s, ev.key, ev.shiftKey)) { ev.preventDefault(); paint(); }
      });
    }
    win.addEventListener("resize", paint);
    for (const m of [mq.left, mq.right]) m.addEventListener("change", paint);
    new win.MutationObserver(paint).observe(app, { attributes: true, attributeFilter: ["class"] });
    paint();
    return ctl;
  }

  window.HubSidebars = { create, attach, CFG };
  if (typeof document !== "undefined" && window.matchMedia) {
    if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", () => attach(document, window));
    else attach(document, window);
  }
})();

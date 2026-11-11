"use strict";
// HubShortcuts runs the page's keyboard shortcuts and the "?" sheet that
// lists them. ctx: search(), newChat(), stop(), running(), closeMenus()
// (true when it closed one), exportChat(), activity(), hasChat().
window.HubShortcuts = (ctx) => {
  const $ = (id) => document.getElementById(id);
  const sheet = $("keys");
  $("keys-close").onclick = () => sheet.close();
  sheet.addEventListener("click", (e) => { if (e.target === sheet) sheet.close(); });

  const typing = () => {
    const a = document.activeElement;
    return !!a && (/^(TEXTAREA|INPUT|SELECT)$/.test(a.tagName) || a.isContentEditable);
  };
  const links = () => Array.from(document.querySelectorAll("#sessions .item a.go"));

  function go(step) {
    const l = links();
    if (!l.length) return;
    const i = l.findIndex((a) => a.closest(".item").classList.contains("active"));
    const j = i < 0 ? (step > 0 ? 0 : l.length - 1) : Math.max(0, Math.min(l.length - 1, i + step));
    l[j].click();
  }

  function nextNeeding() {
    const l = links();
    const i = l.findIndex((a) => a.closest(".item").classList.contains("active"));
    for (let k = 1; k <= l.length; k++) {
      const a = l[(i + k + l.length) % l.length];
      if (a.closest(".item").querySelector(".needs") && !a.closest(".item").classList.contains("active")) { a.click(); return; }
    }
  }

  document.addEventListener("keydown", (e) => {
    if (e.defaultPrevented) return;
    const mod = e.ctrlKey || e.metaKey;
    const key = e.key.length === 1 ? e.key.toLowerCase() : e.key;
    const modal = document.querySelector("dialog[open]");
    if (modal && modal !== sheet) return;
    if (e.key === "Escape") {
      if (sheet.open) return;
      if (ctx.closeMenus()) { e.preventDefault(); return; }
      if (ctx.running() && ctx.hasChat()) { e.preventDefault(); ctx.stop(); }
      return;
    }
    if (mod && !e.altKey && !e.shiftKey && key === "k") { e.preventDefault(); ctx.search(); return; }
    if (mod && e.shiftKey && !e.altKey && key === "o") { e.preventDefault(); ctx.newChat(); return; }
    if (mod && e.shiftKey && !e.altKey && key === "e") { if (ctx.hasChat()) { e.preventDefault(); ctx.exportChat(); } return; }
    if (mod && !e.shiftKey && !e.altKey && key === ".") { if (ctx.hasChat()) { e.preventDefault(); ctx.activity(); } return; }
    if (e.altKey && !mod && e.shiftKey && e.key === "ArrowDown") { e.preventDefault(); nextNeeding(); return; }
    if (e.altKey && !mod && !e.shiftKey && (e.key === "ArrowDown" || e.key === "ArrowUp")) {
      e.preventDefault();
      go(e.key === "ArrowDown" ? 1 : -1);
      return;
    }
    if (typing() || mod || e.altKey) return;
    if (e.key === "?") { e.preventDefault(); if (!sheet.open) sheet.showModal(); return; }
    if (e.key === "/" && ctx.hasChat() && !$("composer").hidden) { e.preventDefault(); $("input").focus(); }
  });

  return { open: () => sheet.showModal() };
};

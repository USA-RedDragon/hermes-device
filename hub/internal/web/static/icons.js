"use strict";
// HubIcons draws the page's line icons as inline SVG that strokes in the
// text color. icon(name) returns a new <svg>; button(name, label, cls)
// returns an icon button with its tooltip and aria-label set; logo() is
// the Talaria mark, filled in the text color.
window.HubIcons = (() => {
  const paths = {
    plus: "M12 5v14M5 12h14", menu: "M3 6h18M3 12h18M3 18h18", download: "M12 3v12M7 10l5 5 5-5M5 21h14",
    stop: "M6 6h12v12H6z", send: "M12 19V5M5 12l7-7 7 7",
    copy: "M9 9h11v11H9zM5 15H4V4h11v1", edit: "M12 20h9M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4z",
    retry: "M3 12a9 9 0 1 0 3-6.7L3 8M3 3v5h5",
    branch: "M6 3v12M18 9a3 3 0 1 0 0-.01M6 21a3 3 0 1 0 0-.01M18 9c0 6-12 3-12 9",
    more: "M5 12h.01M12 12h.01M19 12h.01", chev: "M6 9l6 6 6-6",
    box: "M21 8l-9-5-9 5 9 5 9-5zM3 8v8l9 5 9-5V8M12 13v8",
    clip: "M21.4 11.1l-9.2 9.2a6 6 0 0 1-8.5-8.5l9.2-9.2a4 4 0 0 1 5.7 5.7l-9.2 9.2a2 2 0 0 1-2.8-2.8l8.5-8.5",
    files: "M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z",
    activity: "M22 12h-4l-3 9L9 3l-3 9H2", monitor: "M3 4h18v12H3zM8 20h8M12 16v4",
    search: "M11 18a7 7 0 1 0 0-14 7 7 0 0 0 0 14zM20 20l-3.5-3.5", pin: "M12 17v5M9 10.8V4h6v6.8l3 3.2H6z",
    close: "M18 6 6 18M6 6l12 12", check: "M20 6 9 17l-5-5",
    side: "M3 4h18v16H3zM15 4v16", open: "M14 3h7v7M10 14 21 3M21 14v7H3V3h7", popup: "M3 4h18v4H3zM11 8h10v9H11z",
    tabs: "M3 7h18v13H3zM3 7l3-4h6l3 4",
    checkcircle: "M12 22a10 10 0 1 0 0-20 10 10 0 0 0 0 20zM8 12l3 3 5-6",
    alert: "M12 22a10 10 0 1 0 0-20 10 10 0 0 0 0 20zM12 7v6M12 16.5v.01",
    info: "M12 22a10 10 0 1 0 0-20 10 10 0 0 0 0 20zM12 11v6M12 7.5v.01",
    terminal: "M3 5h18v14H3zM7 10l3 2-3 2M12 15h5", message: "M4 4h16v12H9l-5 4z",
    file: "M14 3H6a1 1 0 0 0-1 1v16a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1V8zM14 3v5h5M9 13h6M9 17h6",
    filenew: "M14 3H6a1 1 0 0 0-1 1v16a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1V8zM14 3v5h5M12 12v6M9 15h6",
    globe: "M12 22a10 10 0 1 0 0-20 10 10 0 0 0 0 20zM2 12h20M12 2a15 15 0 0 1 0 20 15 15 0 0 1 0-20",
    clock: "M12 22a10 10 0 1 0 0-20 10 10 0 0 0 0 20zM12 6v6l4 2",
    list: "M8 6h13M8 12h13M8 18h13M3 6h.01M3 12h.01M3 18h.01",
    tool: "M14.7 6.3a4 4 0 0 0-5.4 5.4L3 18l3 3 6.3-6.3a4 4 0 0 0 5.4-5.4l-2.5 2.5-2.5-.5-.5-2.5z",
    bot: "M5 8h14v10H5zM12 8V4M9 13h.01M15 13h.01M3 13v2M21 13v2",
    compass: "M12 22a10 10 0 1 0 0-20 10 10 0 0 0 0 20zM16 8l-2 6-6 2 2-6z",
    mail: "M3 5h18v14H3zM3 6l9 7 9-7",
    database: "M12 8c4.4 0 8-1.3 8-3s-3.6-3-8-3-8 1.3-8 3 3.6 3 8 3zM4 5v14c0 1.7 3.6 3 8 3s8-1.3 8-3V5M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3",
    book: "M4 4a2 2 0 0 1 2-2h14v18H6a2 2 0 0 0-2 2zM4 20V4M8 7h8",
    cursor: "M5 3l14 7-6 2-2 6z",
    camera: "M3 7h4l2-3h6l2 3h4v13H3zM12 17a4 4 0 1 0 0-.01",
    archive: "M3 4h18v4H3zM5 8v12h14V8M10 12h4",
    eye: "M2 12s4-7 10-7 10 7 10 7-4 7-10 7S2 12 2 12zM12 15a3 3 0 1 0 0-.01",
  };
  const mark = "M3.00 45.00 Q30.50 36.00 38.00 9.00 Q38.40 18.00 40.72 25.72 C47.80 18.30 57.30 12.60 66.80 12.60 " +
    "C61.10 16.40 57.30 21.15 56.35 24.95 C59.20 23.05 62.05 24.47 64.42 24.95 C58.25 31.60 50.65 34.45 43.85 33.82 " +
    "Q47.06 40.49 52.00 46.00 Q27.50 40.00 3.00 45.00 Z";
  const ns = "http://www.w3.org/2000/svg";
  function icon(name) {
    const s = document.createElementNS(ns, "svg");
    s.setAttribute("viewBox", "0 0 24 24");
    s.setAttribute("class", "ico");
    s.setAttribute("aria-hidden", "true");
    const p = document.createElementNS(ns, "path");
    p.setAttribute("d", paths[name] || "");
    s.append(p);
    return s;
  }
  function set(node, name) {
    const old = node.querySelector(":scope > svg");
    if (old) old.replaceWith(icon(name));
    else node.prepend(icon(name));
    return node;
  }
  function button(name, label, cls) {
    const b = document.createElement("button");
    b.type = "button";
    b.className = cls || "ghost ib";
    b.title = label;
    b.setAttribute("aria-label", label.replace(/\s*\([^)]*\)$/, ""));
    b.append(icon(name));
    return b;
  }
  function logo() {
    const s = document.createElementNS(ns, "svg");
    s.setAttribute("viewBox", "-0.1 -7.5 70 70");
    s.setAttribute("class", "logo");
    s.setAttribute("aria-hidden", "true");
    const p = document.createElementNS(ns, "path");
    p.setAttribute("d", mark);
    for (const [k, v] of [["fill", "currentColor"], ["stroke", "currentColor"], ["stroke-width", "2.5"], ["stroke-linejoin", "round"]]) {
      p.setAttribute(k, v);
    }
    s.append(p);
    return s;
  }
  return { icon, set, button, logo, has: (n) => n in paths };
})();

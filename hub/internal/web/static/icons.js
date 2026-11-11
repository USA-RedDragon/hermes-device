"use strict";
// HubIcons draws the page's line icons as inline SVG that strokes in the
// text color. icon(name) returns a new <svg>; button(name, label, cls)
// returns an icon button with its tooltip and aria-label set.
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
  };
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
  return { icon, set, button, has: (n) => n in paths };
})();

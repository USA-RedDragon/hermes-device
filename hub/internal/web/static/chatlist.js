"use strict";
// HubChatList adds to the chat list what the hub keeps per user: pinned
// chats, unread replies and chats that wait on the user, with the marks and
// menu entries for them. It also runs the new chat menu: "+" starts a chat
// in the sandbox, the arrow next to it picks the sandbox or a computer.
// ctx: api, el, toast(text), banner(text, bad), refresh(), exportChat(b,
// s, title), computers(), newChat(), pickFolder(computer), me().
window.HubChatList = (ctx) => {
  const { api, el } = ctx;
  const $ = (id) => document.getElementById(id);
  const I = window.HubIcons;
  const base = (s) => "/api/sessions/" + encodeURIComponent(s.backend_id) + "/" + encodeURIComponent(s.id);
  const needsText = { approval: "Approval", question: "Question" };

  // mark is the dot before a chat's name: green while it runs, blue for a
  // reply the user has not seen.
  function mark(s) {
    const m = el("span", "mark" + (s.running ? " running" : s.unread ? " unread" : ""));
    if (s.running) m.title = "Working";
    else if (s.unread) m.title = "New reply";
    m.setAttribute("aria-hidden", "true");
    return m;
  }

  // tail is what follows a chat's name: a badge when it waits on the user,
  // and a pin when pinned.
  function tail(s) {
    const out = [];
    if (needsText[s.needs]) {
      const n = el("span", "needs", needsText[s.needs]);
      n.title = s.needs === "approval" ? "Talaria waits for your approval" : "Talaria asked you a question";
      out.push(n);
    }
    if (s.pinned) {
      const p = el("span", "pin");
      p.title = "Pinned";
      p.append(I.icon("pin"));
      out.push(p);
    }
    return out;
  }

  function label(s) {
    const bits = [];
    if (s.running) bits.push("working");
    else if (s.unread) bits.push("new reply");
    if (needsText[s.needs]) bits.push("waits for your " + (s.needs === "approval" ? "approval" : "answer"));
    if (s.pinned) bits.push("pinned");
    return bits.join(", ");
  }

  async function post(s, path, body, fail) {
    try {
      await api("POST", base(s) + path, body);
      ctx.refresh();
      return true;
    } catch (e) {
      ctx.banner(fail + e.message, true);
      return false;
    }
  }

  const pin = (s, on) => post(s, "/pin", { pinned: on }, on ? "Could not pin: " : "Could not unpin: ").then((ok) => {
    if (ok) { s.pinned = on; ctx.toast(on ? "Pinned" : "Unpinned"); }
  });

  // read marks a chat read (as of the update the page saw) or unread.
  function read(s, unread) {
    s.unread = !!unread;
    return api("POST", base(s) + "/read", { unread: !!unread, at: s.updated_at || new Date().toISOString() }).catch(() => {});
  }

  // menu returns the chat menu entries this module adds.
  function menu(s, close) {
    const b = (text, fn) => {
      const x = el("button", "", text);
      x.type = "button";
      x.setAttribute("role", "menuitem");
      x.onclick = (e) => { e.stopPropagation(); close(); fn(); };
      return x;
    };
    return {
      top: [
        b(s.pinned ? "Unpin" : "Pin to top", () => pin(s, !s.pinned)),
        b(s.unread ? "Mark as read" : "Mark as unread", () => { read(s, !s.unread).then(ctx.refresh); }),
      ],
      export: b("Export as Markdown", () => ctx.exportChat(s.backend_id, s.id, s.title)),
    };
  }

  // seen marks the open chat read when the list says it has a new reply.
  function seen(s) {
    if (s && s.unread && !document.hidden) read(s, false);
  }

  // The new chat menu.
  const menuBox = $("newmenu");
  const chev = $("newchev");
  function closeNew() {
    menuBox.hidden = true;
    chev.setAttribute("aria-expanded", "false");
  }
  function openNew() {
    menuBox.replaceChildren();
    const add = (ico, name, desc, on, fn) => {
      const x = el("button");
      x.type = "button";
      x.setAttribute("role", "menuitem");
      x.disabled = !on;
      const t = el("span", "t");
      t.append(el("span", "", name), el("small", "", desc));
      x.append(I.icon(ico), t);
      x.onclick = (e) => { e.stopPropagation(); closeNew(); fn(); };
      menuBox.append(x);
    };
    add("box", "Sandbox", "No access to your files", true, () => ctx.newChat());
    for (const c of ctx.computers()) {
      add("monitor", c.name, c.online ? "Pick a folder next" : "Offline", c.online, () => ctx.pickFolder(c));
    }
    menuBox.hidden = false;
    chev.setAttribute("aria-expanded", "true");
    const first = menuBox.querySelector("button:not(:disabled)");
    if (first) first.focus();
  }
  chev.onclick = (e) => { e.stopPropagation(); if (menuBox.hidden) openNew(); else closeNew(); };
  menuBox.addEventListener("keydown", (e) => {
    const items = Array.from(menuBox.querySelectorAll("button:not(:disabled)"));
    const i = items.indexOf(document.activeElement);
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      const n = items.length;
      if (n) items[(i + (e.key === "ArrowDown" ? 1 : n - 1) + n) % n].focus();
    } else if (e.key === "Escape") {
      e.preventDefault();
      e.stopPropagation();
      closeNew();
      chev.focus();
    }
  });
  document.addEventListener("click", (e) => { if (!menuBox.hidden && !menuBox.contains(e.target)) closeNew(); });

  // showChevron shows the arrow only when the user has computers to pick.
  function showChevron(on) {
    chev.hidden = !on;
    $("split").classList.toggle("one", !on);
    $("newchat").title = on ? "New chat in the Sandbox (Ctrl+Shift+O)" : "New chat (Ctrl+Shift+O)";
  }

  return { mark, tail, label, menu, seen, read, pin, showChevron, closeNew, newOpen: () => !menuBox.hidden };
};

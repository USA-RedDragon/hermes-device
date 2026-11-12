"use strict";

// The page chat: the toolbar popup and the sidebar frame the hub's page
// chat view (<hub>/pagechat), so the hub's own chat UI runs here and
// cannot drift from the hub. This page gives that view what only the
// extension knows, over a MessagePort: the tabs of this window, the
// selected text, this device's hub id, and, for each message, a grant
// that lets the chat read those tabs (docs/browser.md section 6.2).
//
// Opening the sidebar or the popup needs a click in an extension page
// (Firefox checks the page's own user input), so the view reports where
// those buttons are and this page lays its own transparent buttons over
// them.

const MAX_TABS = 50;
const MAX_TITLE = 200;
const MAX_URL = 2000;
const MAX_SELECTION = 4000;

const popup = browser.extension.getViews({ type: "popup" }).includes(window);
const frame = document.getElementById("frame");
const statusLine = document.getElementById("status");
const keysBox = document.getElementById("keys");
let hubOrigin = "";
let port = null;
let windowId = null;
let state = null;

document.body.classList.toggle("popup", popup);

function say(text) {
  statusLine.textContent = text;
  statusLine.hidden = !text;
  frame.hidden = Boolean(text);
}

function hostOf(url) {
  try {
    return new URL(url).host.toLowerCase();
  } catch (e) {
    return "";
  }
}

function pageTab(tab) {
  return /^https?:/.test(tab.url || "") && hostOf(tab.url) !== hostOf(hubOrigin);
}

function tabInfo(tab) {
  return { tab_id: tab.id, title: String(tab.title || "").slice(0, MAX_TITLE), url: String(tab.url).slice(0, MAX_URL),
    active: Boolean(tab.active) };
}

async function windowTabs() {
  if (windowId === null) {
    windowId = (await browser.windows.getCurrent()).id;
  }
  return (await browser.tabs.query({ windowId })).filter(pageTab).slice(0, MAX_TABS);
}

async function selection(tabs) {
  const tab = tabs.find((t) => t.active);
  if (!tab) {
    return null;
  }
  try {
    const [text] = await browser.tabs.executeScript(tab.id, { code: "String(window.getSelection() || '')" });
    const t = String(text || "").trim();
    return t ? { tab_id: tab.id, text: t.slice(0, MAX_SELECTION) } : null;
  } catch (e) {
    return null;
  }
}

async function context(scope, session) {
  let tabs = await windowTabs();
  const sel = await selection(tabs);
  if (scope === "tab") {
    tabs = tabs.filter((t) => t.active);
  }
  if (session) {
    const { tabs: ok } = await browser.runtime.sendMessage({ type: "page_grant", session, tabs: tabs.map((t) => t.id) });
    tabs = tabs.filter((t) => ok.includes(t.id));
  }
  return { window_id: windowId, tabs: tabs.map(tabInfo), selection: sel };
}

let watchTimer = 0;
function watch() {
  clearTimeout(watchTimer);
  watchTimer = setTimeout(async () => {
    if (!port) {
      return;
    }
    const tabs = await windowTabs();
    const sel = await selection(tabs);
    port.postMessage({ type: "tabs", count: tabs.length, active: tabs.some((t) => t.active), selection: Boolean(sel),
      selection_text: sel ? sel.text.slice(0, 200) : "" });
  }, 150);
}

for (const ev of [browser.tabs.onCreated, browser.tabs.onRemoved, browser.tabs.onActivated, browser.tabs.onAttached,
  browser.tabs.onDetached]) {
  ev.addListener(watch);
}
browser.tabs.onUpdated.addListener((_id, change) => {
  if (change.url || change.title || change.status === "complete") {
    watch();
  }
});

// The buttons only an extension page may run.
const keyActions = {
  sidebar() {
    browser.sidebarAction.open().finally(() => window.close());
  },
  popup() {
    browser.browserAction.openPopup().then(() => browser.sidebarAction.close(), () => browser.sidebarAction.close());
  },
  close() {
    browser.sidebarAction.close();
  },
};

function placeKeys(keys) {
  keysBox.replaceChildren();
  for (const k of keys || []) {
    if (!keyActions[k.name]) {
      continue;
    }
    const b = document.createElement("button");
    b.type = "button";
    b.title = String(k.label || "");
    b.setAttribute("aria-label", String(k.label || ""));
    Object.assign(b.style, { left: k.x + "px", top: k.y + "px", width: k.w + "px", height: k.h + "px" });
    b.addEventListener("click", keyActions[k.name]);
    for (const [ev, on] of [["mouseenter", true], ["mouseleave", false]]) {
      b.addEventListener(ev, () => port && port.postMessage({ type: "hover", name: k.name, on }));
    }
    keysBox.append(b);
  }
}

async function onPort(msg) {
  if (!msg || typeof msg !== "object") {
    return;
  }
  switch (msg.type) {
    case "context":
      try {
        port.postMessage({ type: "context", id: msg.id, ...(await context(msg.scope, msg.session)) });
      } catch (e) {
        port.postMessage({ type: "context", id: msg.id, error: String(e && e.message ? e.message : e) });
      }
      break;
    case "watch":
      watch();
      break;
    case "keys":
      placeKeys(msg.keys);
      break;
    case "open":
      if (typeof msg.url === "string" && new URL(msg.url, hubOrigin).origin === hubOrigin) {
        await browser.tabs.create({ url: new URL(msg.url, hubOrigin).href, windowId: windowId === null ? undefined : windowId });
        if (popup) {
          window.close();
        }
      }
      break;
    case "close":
      if (popup) {
        window.close();
      }
      break;
    default:
      break;
  }
}

frame.addEventListener("load", () => {
  if (!hubOrigin) {
    return;
  }
  const ch = new MessageChannel();
  port = ch.port1;
  port.onmessage = (e) => onPort(e.data);
  frame.contentWindow.postMessage({ type: "talaria-page:init", mode: popup ? "popup" : "sidebar",
    device_id: state.device_id, version: state.version }, hubOrigin, [ch.port2]);
});

async function start() {
  try {
    state = await browser.runtime.sendMessage({ type: "page_open" });
  } catch (e) {
    state = null;
  }
  if (!state || !state.hub_url) {
    say(state && state.connected ? "Connecting to talaria..." :
      "Talaria is not connected. Start talaria on this computer to use the page chat.");
    setTimeout(start, 2000);
    return;
  }
  hubOrigin = state.hub_url;
  windowId = (await browser.windows.getCurrent()).id;
  say("");
  frame.src = hubOrigin + "/pagechat";
}

start();

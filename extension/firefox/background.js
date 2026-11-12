"use strict";

// Talaria browser control. The ways in are the native messaging port to
// the talaria host and messages from this extension's own page chat
// (pagechat.html): there are no content scripts that listen to the page,
// no external messaging and no web accessible resources.

// The native messaging host. Before the rename to talaria it was
// hermes_device; the extension tries that name too, so it works with a
// computer that has only the old host manifest.
const HOSTS = ["talaria", "hermes_device"];
let hostIndex = 0;
const GROUP_TITLE = "Talaria";
const RETRY_MS = 5000;
const LOAD_TIMEOUT_MS = 30000;
const MAX_TEXT = 100000;
const MAX_EVAL_RESULT = 1000000;
const MAX_WAIT_MS = 30000;
const MAX_SHOT_HEIGHT = 16384;
const MAX_SHOT_PIXELS = 16 * 1024 * 1024;
const LOGIN_FILL_MS = 15000;
const MAX_LOGIN_FILL_MS = 60000;

// Salesforce hosts are "protected sites" (docs/browser.md section 7.1):
// every click and every key press that can activate a control needs the
// user's OK each time, and scripts may never change data there.
const PROTECTED_SUFFIXES = ["force.com", "salesforce.com", "visualforce.com", "cloudforce.com", "salesforce-setup.com",
  "salesforce-sites.com", "documentforce.com", "force-user-content.com"];

// Keys browser_press accepts, with their legacy key codes.
const KEY_CODES = {
  Enter: 13, Tab: 9, Escape: 27, Backspace: 8, Delete: 46, " ": 32, ArrowLeft: 37, ArrowUp: 38, ArrowRight: 39,
  ArrowDown: 40, Home: 36, End: 35, PageUp: 33, PageDown: 34,
};
const KEY_ALIASES = {
  esc: "Escape", escape: "Escape", enter: "Enter", return: "Enter", tab: "Tab", space: " ", spacebar: " ",
  backspace: "Backspace", delete: "Delete", del: "Delete", up: "ArrowUp", down: "ArrowDown", left: "ArrowLeft",
  right: "ArrowRight", arrowup: "ArrowUp", arrowdown: "ArrowDown", arrowleft: "ArrowLeft", arrowright: "ArrowRight",
  home: "Home", end: "End", pageup: "PageUp", pagedown: "PageDown",
};
const MODIFIERS = {
  control: "ctrlKey", ctrl: "ctrlKey", shift: "shiftKey", alt: "altKey", option: "altKey", meta: "metaKey", cmd: "metaKey",
  command: "metaKey",
};

let port = null;
// replaced is set when the daemon took a newer connection (another Firefox
// with this extension). This one then waits for a click on its button.
let replaced = false;
let groupId = null;
let inflight = 0;
const agentTabs = new Set();
// Hosts on loopback or private networks that the device allows, from the
// daemon's config message. Everything else private is blocked in agent tabs.
let allowHosts = new Set();
// Sites from the device config where, as on Salesforce, every click asks.
let confirmHosts = [];
// Exact hosts from the device config where browser_login may submit the
// login form that Firefox filled from its saved logins (docs/browser.md
// section 7.4). The plugin cannot change this list.
let autologinHosts = new Set();
// loginGuards holds, per tab, the hosts a login submit may post to while
// browser_login waits for the result (docs/browser.md section 7.4).
const loginGuards = new Map();
let loginBusy = false;

// The daemon's hub.url, from its config message. Agent tabs may not open
// the hub.
let hubHost = "";
let hubURL = "";
// The hub's id of this device, from the config message once the daemon's
// exec leg connected. The page chat names it, so its chat uses this
// Firefox.
let deviceID = "";

// pageGrants holds, per hub chat (session id), the user's tabs that the
// page chat let that chat read: read_text and snapshot of these tabs work
// for requests of that session, as for agent tabs (docs/browser.md
// section 6.2). Each message from the page chat replaces its chat's grant.
const pageGrants = new Map();
const PAGE_GRANT_MS = 30 * 60 * 1000;
const MAX_PAGE_TABS = 50;
const SESSION_RE = /^[A-Za-z0-9_.:-]{1,200}$/;

// Refused marks an error the extension chose: the device reports it with
// code "refused" instead of "failed".
class Refused extends Error {}

// The badge says "on" while the extension is connected to the talaria
// daemon, in a dark green that reads on light and dark toolbars. The title
// carries the same state in words, so the color is not the only signal.
const BADGE_ON = "#167a42";
const BADGE_BUSY = "#2f6fde";

function setStatus(text, badge) {
  browser.browserAction.setTitle({ title: "Talaria: " + text });
  browser.browserAction.setBadgeText({ text: badge });
  browser.browserAction.setBadgeBackgroundColor({ color: badge === "on" ? BADGE_ON : BADGE_BUSY });
  browser.browserAction.setBadgeTextColor({ color: "#ffffff" });
}

function showStatus() {
  if (inflight > 0) {
    return;
  }
  if (port) {
    setStatus("connected", "on");
  } else if (replaced) {
    setStatus("another Firefox took over; open this button to use this one", "");
  } else {
    setStatus("not connected", "");
  }
}

async function connect() {
  const p = browser.runtime.connectNative(HOSTS[hostIndex]);
  let answered = false;
  port = p;
  p.onMessage.addListener((msg) => {
    answered = true;
    return onMessage(msg);
  });
  p.onDisconnect.addListener(() => {
    if (!answered) {
      hostIndex = (hostIndex + 1) % HOSTS.length;
    }
    port = null;
    showStatus();
    if (!replaced) {
      setTimeout(connect, RETRY_MS);
    }
  });
  try {
    const info = await browser.runtime.getBrowserInfo();
    p.postMessage({
      type: "hello",
      version: browser.runtime.getManifest().version,
      browser: info.name + " " + info.version,
    });
    showStatus();
  } catch (e) {
    // The port went away before the hello; onDisconnect retries.
  }
}

async function onMessage(msg) {
  if (msg && msg.type === "config") {
    allowHosts = new Set((msg.allow_hosts || []).map((h) => h.toLowerCase()));
    confirmHosts = (msg.confirm_hosts || []).map((h) => String(h).toLowerCase().replace(/\.$/, "")).filter(Boolean);
    autologinHosts = new Set((msg.autologin_hosts || []).map((h) => String(h).toLowerCase().replace(/\.$/, "")).filter(Boolean));
    hubHost = hostPortOf(msg.hub_url);
    hubURL = hubHost ? new URL(msg.hub_url).origin : "";
    deviceID = typeof msg.device_id === "string" ? msg.device_id : "";
    return;
  }
  if (msg && msg.type === "replaced") {
    replaced = true;
    showStatus();
    return;
  }
  if (!msg || msg.type !== "request") {
    return;
  }
  const handler = OPS[msg.op];
  let reply;
  inflight++;
  setStatus("working: " + msg.op, "...");
  try {
    if (!handler) {
      throw new Refused("unknown op " + msg.op);
    }
    reply = { type: "response", id: msg.id, ok: true, result: await handler(msg.args || {}) };
  } catch (e) {
    reply = {
      type: "response", id: msg.id, ok: false, code: e instanceof Refused ? "refused" : "failed",
      error: String(e && e.message ? e.message : e),
    };
  }
  if (--inflight === 0) {
    showStatus();
  }
  if (port) {
    port.postMessage(reply);
  }
}


// Script state per tab (docs/browser.md section 7.2). taints holds the
// network limits of a tab that ran browser_eval: the sites it may still
// reach and whether it may only read. scripted marks tabs whose content
// script world ran agent code: the action helpers there cannot be trusted
// until the agent navigates the tab again.
const taints = new Map();
const scripted = new Set();
// everScripted holds tabs that ran a script at any time. They may not go
// back, so a page that ran agent code is never restored from the history
// cache without its limits.
const everScripted = new Set();
// frameNums maps a tab's Firefox frame ids to the short numbers in refs
// (f1, f2, ...), which stay the same for a frame while the tab lives.
const frameNums = new Map();

browser.tabs.onRemoved.addListener((tabId) => {
  agentTabs.delete(tabId);
  taints.delete(tabId);
  scripted.delete(tabId);
  everScripted.delete(tabId);
  frameNums.delete(tabId);
  loginGuards.delete(tabId);
});

// A tab that a scripted tab opens inherits its limits.
browser.tabs.onCreated.addListener((tab) => {
  inheritTaint(tab);
});

function inheritTaint(tab) {
  const from = tab.openerTabId !== undefined ? taints.get(tab.openerTabId) : undefined;
  if (from && !taints.has(tab.id)) {
    taints.set(tab.id, { sites: new Map(from.sites) });
    scripted.add(tab.id);
    everScripted.add(tab.id);
  }
  return taints.get(tab.id);
}

// taintBlocks reports whether a tab with taint t may not send request d:
// it goes to another site than the ones the script ran on, or it is not a
// GET or HEAD (or it opens a WebSocket) to a site where scripts may only
// read. t.sites maps each site to whether writes are approved there.
function taintBlocks(t, d) {
  let host;
  try {
    host = new URL(d.url).hostname;
  } catch (e) {
    return true;
  }
  const site = siteOf(host);
  if (!t.sites.has(site)) {
    return true;
  }
  return !t.sites.get(site) && (d.type === "websocket" || (d.method !== "GET" && d.method !== "HEAD"));
}

function protectedHost(host) {
  const h = String(host || "").toLowerCase().replace(/\.$/, "");
  return PROTECTED_SUFFIXES.concat(confirmHosts).some((s) => h === s || h.endsWith("." + s));
}

// autologinHost reports whether host is one of the device's
// browser.autologin-hosts. Exact host names, no subdomains.
function autologinHost(host) {
  return autologinHosts.has(String(host || "").toLowerCase().replace(/\.$/, ""));
}

function hostOf(url) {
  try {
    return new URL(url).hostname.toLowerCase();
  } catch (e) {
    return "";
  }
}

function frameNum(tabId, frameId) {
  if (!frameId) {
    return 0;
  }
  let m = frameNums.get(tabId);
  if (!m) {
    m = { byNum: new Map(), byId: new Map(), next: 1 };
    frameNums.set(tabId, m);
  }
  let n = m.byId.get(frameId);
  if (n === undefined) {
    n = m.next++;
    m.byId.set(frameId, n);
    m.byNum.set(n, frameId);
  }
  return n;
}

// frameIdOf turns a frame name from a snapshot ("f2", or "" for the top
// frame) into Firefox's frame id.
function frameIdOf(tabId, name) {
  if (!name) {
    return 0;
  }
  const m = /^f(\d{1,6})$/.exec(String(name));
  const ids = frameNums.get(tabId);
  const id = m && ids ? ids.byNum.get(Number(m[1])) : undefined;
  if (id === undefined) {
    throw new Refused("unknown frame " + String(name).slice(0, 20) + "; take a new snapshot");
  }
  return id;
}

// parseRef splits a ref such as "e12" or "f2e12" into its frame and the
// frame's own ref.
function parseRef(tabId, ref) {
  const m = /^@?(f\d{1,6})?(e\d{1,6})$/.exec(String(ref || ""));
  if (!m) {
    throw new Refused("bad ref " + String(ref).slice(0, 20) + "; use a ref from the last snapshot, such as e12 or f1e3");
  }
  return { frameId: frameIdOf(tabId, m[1]), ref: m[2] };
}

// parseKey reads a key such as "Enter", "Shift+Tab" or "Control+a".
function parseKey(text) {
  const raw = String(text || "");
  const plus = raw.endsWith("++") || raw === "+";
  const parts = (plus ? raw.slice(0, -1) : raw).split("+");
  let name = plus ? "+" : parts.pop();
  if (plus && parts.length && parts[parts.length - 1] === "") {
    parts.pop();
  }
  const key = { key: "", ctrlKey: false, shiftKey: false, altKey: false, metaKey: false };
  for (const p of parts) {
    const m = MODIFIERS[p.trim().toLowerCase()];
    if (!m) {
      throw new Refused("unknown modifier " + p.slice(0, 20) + "; use Control, Shift, Alt or Meta");
    }
    key[m] = true;
  }
  name = name || "";
  if (name.length === 1) {
    key.key = name;
  } else {
    key.key = KEY_ALIASES[name.trim().toLowerCase()] || "";
    if (!key.key) {
      throw new Refused("unknown key " + name.slice(0, 20) + "; use Enter, Tab, Escape, Space, Backspace, Delete, an " +
        "arrow, Home, End, PageUp, PageDown or one character");
    }
  }
  key.code = KEY_CODES[key.key] || key.key.toUpperCase().charCodeAt(0);
  key.activates = key.key === "Enter" || key.key === " ";
  return key;
}


async function addToGroup(tabId) {
  agentTabs.add(tabId);
  if (!browser.tabs.group) {
    return;
  }
  try {
    if (groupId !== null) {
      await browser.tabs.group({ tabIds: [tabId], groupId });
      return;
    }
  } catch (e) {
    groupId = null;
  }
  groupId = await browser.tabs.group({ tabIds: [tabId] });
  await browser.tabGroups.update(groupId, { title: GROUP_TITLE, color: "purple" });
}

// inGroup reports whether the agent may act on tab: it must be in the
// Talaria tab group, so a tab the user drags out of the group is no longer
// the agent's. Without the tab groups API, the tabs the agent opened.
function inGroup(tab) {
  return groupId !== null ? tab.groupId === groupId : agentTabs.has(tab.id);
}

// agentTab returns the tab if the agent may act on it. Unless anyURL is
// set, the tab must also show an http or https page, so a tab that went
// back or redirected to file: or about: cannot be read. With sessions (the
// session and its ancestors), a tab the page chat granted to one of them
// also counts; only reads pass sessions.
async function agentTab(tabId, anyURL, sessions) {
  const tab = await browser.tabs.get(tabId);
  if (!inGroup(tab) && !(Array.isArray(sessions) && sessions.some((s) => typeof s === "string" && granted(s, tab)))) {
    throw new Refused("tab " + tabId + " is not in the Talaria tab group");
  }
  if (!anyURL && !/^https?:/.test(tab.url || "")) {
    throw new Refused("tab " + tabId + " shows " + String(tab.url).split(":")[0] + ": content, which the agent may not use");
  }
  return tab;
}

// pageTab reports whether the page chat may offer tab to the agent: an
// http or https page that is not the hub.
function pageTab(tab) {
  return /^https?:/.test(tab.url || "") && !hermesHost(hostPortOf(tab.url));
}

function granted(session, tab) {
  const g = pageGrants.get(session);
  if (!g || g.until < Date.now()) {
    pageGrants.delete(session);
    return false;
  }
  return g.tabs.has(tab.id) && pageTab(tab);
}

// grantPage lets session read tabIds, the tabs the user's page chat
// message covers, and returns those it may read.
async function grantPage(session, tabIds) {
  if (typeof session !== "string" || !SESSION_RE.test(session) || !Array.isArray(tabIds)) {
    throw new Error("bad grant");
  }
  const tabs = new Set();
  for (const id of tabIds.slice(0, MAX_PAGE_TABS)) {
    try {
      const tab = await browser.tabs.get(id);
      if (pageTab(tab)) {
        tabs.add(tab.id);
      }
    } catch (e) {
      // The tab closed.
    }
  }
  for (const [k, g] of pageGrants) {
    if (g.until < Date.now()) {
      pageGrants.delete(k);
    }
  }
  pageGrants.set(session, { tabs, until: Date.now() + PAGE_GRANT_MS });
  return Array.from(tabs);
}

function pageState() {
  return { connected: Boolean(port), replaced, hub_url: hubURL, device_id: deviceID,
    version: browser.runtime.getManifest().version };
}

// The page chat (pagechat.html, the toolbar popup and the sidebar) asks
// for the hub and this device, and grants its chat the tabs a message
// covers. Only this extension's own pages may.
browser.runtime.onMessage.addListener((msg, sender) => {
  if (sender.id !== browser.runtime.id || !String(sender.url || "").startsWith(browser.runtime.getURL("pagechat.html"))) {
    return undefined;
  }
  if (msg && msg.type === "page_open") {
    if (!port) {
      replaced = false;
      connect();
    }
    return Promise.resolve(pageState());
  }
  if (msg && msg.type === "page_grant") {
    return grantPage(msg.session, msg.tabs).then((tabs) => ({ tabs }));
  }
  return undefined;
});

// siteOf is the host name without a leading "www.", the unit the user
// approves (docs/browser.md section 7). The plugin uses the same rule.
function siteOf(host) {
  const h = String(host || "").toLowerCase().replace(/\.$/, "");
  return h.startsWith("www.") ? h.slice(4) : h;
}

// checkVisit refuses a navigation to another site than from (a host, or
// "" for a new tab) unless the plugin approved that site in guard.
function checkVisit(target, from, guard) {
  const to = siteOf(new URL(target).hostname);
  const approved = ((guard && guard.visit_hosts) || []).map(siteOf);
  if (to !== siteOf(from) && !approved.includes(to)) {
    throw new Refused("opening " + to + " needs the user's approval");
  }
}

// hostPortOf is url's host with its port, lower case, or "".
function hostPortOf(url) {
  try {
    return url ? new URL(url).host.toLowerCase() : "";
  } catch (e) {
    return "";
  }
}

// hermesHost reports whether host (with its port) is the daemon's hub.
// Agent tabs may not open it, so the agent cannot use the user's hub
// session, for example to pair another computer.
function hermesHost(host) {
  return hubHost !== "" && String(host || "").toLowerCase().replace(/\.$/, "") === hubHost;
}

function checkURL(url) {
  const u = new URL(url);
  if (u.protocol !== "https:" && u.protocol !== "http:") {
    throw new Refused("only http and https URLs are allowed, got " + u.protocol);
  }
  if (hermesHost(u.host)) {
    throw new Refused("the agent may not open the Talaria hub in its tabs");
  }
  if (privateHost(u.hostname) && !allowHosts.has(u.hostname.toLowerCase())) {
    throw new Refused(u.hostname + " is on a loopback or private network; allow it in the device config");
  }
  return u.href;
}

function ipv4Private(a) {
  const [x, y] = a;
  return x === 0 || x === 10 || x === 127 || (x === 169 && y === 254) || (x === 172 && y >= 16 && y < 32) ||
    (x === 192 && y === 168) || (x === 100 && y >= 64 && y < 128);
}

function parseIPv4(h) {
  const m = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(h);
  return m ? m.slice(1).map(Number) : null;
}

// privateAddress reports whether an IP literal is loopback, private,
// link-local, CGNAT (Tailscale) or unspecified.
function privateAddress(ip) {
  const h = ip.replace(/^\[|\]$/g, "").toLowerCase();
  const v4 = parseIPv4(h);
  if (v4) {
    return ipv4Private(v4);
  }
  if (!h.includes(":")) {
    return false;
  }
  const mapped = /::ffff:(\d+\.\d+\.\d+\.\d+)$/.exec(h);
  if (mapped) {
    return ipv4Private(parseIPv4(mapped[1]) || [0]);
  }
  // WHATWG URLs write an IPv4-mapped address in hex: [::ffff:7f00:1].
  // NAT64 (64:ff9b::/96) embeds an IPv4 address the same way.
  const hex = /^(?:::ffff|64:ff9b:):([0-9a-f]{1,4}):([0-9a-f]{1,4})$/.exec(h);
  if (hex) {
    const hi = parseInt(hex[1], 16);
    const lo = parseInt(hex[2], 16);
    return ipv4Private([hi >> 8, hi & 255, lo >> 8, lo & 255]);
  }
  return h === "::" || h === "::1" || /^f[cd]/.test(h) || /^fe[89ab]/.test(h);
}

function privateHost(host) {
  const h = host.toLowerCase().replace(/\.$/, "");
  return h === "localhost" || h.endsWith(".localhost") || h.endsWith(".local") || h.endsWith(".internal") ||
    h.endsWith(".home.arpa") || privateAddress(h);
}

// Every request from an agent tab, including redirects, links, forms and
// subresources, is checked: a page or an injected instruction must not
// reach loopback or LAN services through the user's browser. Host names
// are resolved with the browser's own resolver, so a public name that
// points at a private address is caught too. A tab that ran a script is
// also held to the limits of its taint, even after it left the group.
browser.webRequest.onBeforeRequest.addListener(
  async (details) => {
    if (details.tabId < 0) {
      // Requests a content script makes with the extension's own fetch or
      // XMLHttpRequest have no tab and no origin (measured on Firefox
      // 157). While a tab holds a script's limits, those are refused, so
      // agent code cannot reach other sites around the tab's limits.
      return taints.size && !details.originUrl && !details.documentUrl ? { cancel: true } : {};
    }
    const lg = loginGuards.get(details.tabId);
    if (lg && lg.submitting && details.method !== "GET" && details.method !== "HEAD" &&
      (!/^https:/.test(details.url) || !lg.hosts.has(hostOf(details.url)))) {
      return { cancel: true };
    }
    const own = taints.get(details.tabId);
    if (own && taintBlocks(own, details)) {
      return { cancel: true };
    }
    let tab;
    try {
      tab = await browser.tabs.get(details.tabId);
    } catch (e) {
      return {};
    }
    if (!own) {
      const inherited = inheritTaint(tab);
      if (inherited && taintBlocks(inherited, details)) {
        return { cancel: true };
      }
    }
    if (!inGroup(tab)) {
      return {};
    }
    const target = new URL(details.url);
    if (hermesHost(target.host)) {
      return { cancel: true };
    }
    const host = target.hostname.toLowerCase();
    if (allowHosts.has(host)) {
      return {};
    }
    if (privateHost(host)) {
      return { cancel: true };
    }
    if (parseIPv4(host) || host.startsWith("[")) {
      return {};
    }
    try {
      const rec = await browser.dns.resolve(host);
      return rec.addresses.some(privateAddress) ? { cancel: true } : {};
    } catch (e) {
      // A name the browser cannot resolve here is not loaded either; failing
      // closed keeps a resolver error from skipping the check.
      return { cancel: true };
    }
  },
  { urls: ["http://*/*", "https://*/*", "ws://*/*", "wss://*/*"] },
  ["blocking"],
);

// While browser_login waits for its submit, a POST that a 307 or 308
// redirect would send again to a host outside browser.autologin-hosts is
// cancelled, so the login form's body never goes to another host. The
// first other answer from an autologin host ends the submitting phase, so
// later posts (a SAML response to the application) go through.
browser.webRequest.onHeadersReceived.addListener(
  (details) => {
    const lg = loginGuards.get(details.tabId);
    if (!lg || details.method === "GET" || details.method === "HEAD") {
      return {};
    }
    if (details.statusCode === 307 || details.statusCode === 308) {
      const loc = (details.responseHeaders || []).find((h) => h.name.toLowerCase() === "location");
      let to = "";
      try {
        const u = new URL(loc ? loc.value : "", details.url);
        to = u.protocol === "https:" ? u.hostname.toLowerCase() : "";
      } catch (e) {
        to = "";
      }
      if (!lg.hosts.has(to)) {
        return { cancel: true };
      }
      return {};
    }
    if (details.type === "main_frame" && lg.hosts.has(hostOf(details.url))) {
      lg.submitting = false;
    }
    return {};
  },
  { urls: ["https://*/*"] },
  ["blocking", "responseHeaders"],
);

// waitComplete resolves when the tab finishes loading an http(s) page.
// With fresh set it ignores the tab's current state and waits for the next
// load; register it before starting the navigation.
function waitComplete(tabId, fresh) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      done();
      reject(new Error("page load timed out"));
    }, LOAD_TIMEOUT_MS);
    const listener = (id, change, tab) => {
      if (id === tabId && change.status === "complete" && tab.url !== "about:blank") {
        done();
        resolve(tab);
      }
    };
    const done = () => {
      clearTimeout(timer);
      browser.tabs.onUpdated.removeListener(listener);
    };
    browser.tabs.onUpdated.addListener(listener);
    if (fresh) {
      return;
    }
    browser.tabs.get(tabId).then(
      (tab) => {
        if (tab.status === "complete" && tab.url !== "about:blank") {
          done();
          resolve(tab);
        }
      },
      (e) => {
        done();
        reject(e);
      },
    );
  });
}

// settle waits for a navigation that an action may have started.
async function settle(tabId) {
  await new Promise((r) => setTimeout(r, 300));
  const tab = await browser.tabs.get(tabId);
  return tab.status === "loading" ? waitComplete(tabId) : tab;
}

// loadIn navigates an agent tab for the agent. A tab that ran a script
// first goes to about:blank, which needs no request, so the scripted page
// is gone before its limits end; then the new page loads without them.
async function loadIn(tabId, url) {
  if (taints.has(tabId) || scripted.has(tabId)) {
    await browser.tabs.update(tabId, { url: "about:blank" });
    const deadline = Date.now() + 10000;
    for (;;) {
      const tab = await browser.tabs.get(tabId);
      if (tab.url === "about:blank" && tab.status === "complete") {
        break;
      }
      if (Date.now() > deadline) {
        throw new Error("the page that ran a script did not unload");
      }
      await new Promise((r) => setTimeout(r, 100));
    }
    taints.delete(tabId);
    scripted.delete(tabId);
  }
  const loaded = waitComplete(tabId, true);
  await browser.tabs.update(tabId, { url });
  return loaded;
}

async function inFrame(tabId, frameId, func, args, withHelpers) {
  const target = { tabId, frameIds: [frameId] };
  if (withHelpers) {
    await browser.scripting.executeScript({ target, func: installHelpers });
  }
  const [res] = await browser.scripting.executeScript({ target, func, args: args || [] });
  if (!res) {
    throw new Error("the frame is gone; take a new snapshot");
  }
  if (res.error) {
    throw new Error(String(res.error));
  }
  return res.result;
}

// inAllFrames runs func in every frame of the tab and returns
// [{frameId, result}] for the frames that answered.
async function inAllFrames(tabId, func, args, withHelpers) {
  const target = { tabId, allFrames: true };
  if (withHelpers) {
    await browser.scripting.executeScript({ target, func: installHelpers });
  }
  const res = await browser.scripting.executeScript({ target, func, args: args || [] });
  return res.filter((r) => r && !r.error && r.result !== undefined && r.result !== null);
}

async function inPage(tabId, func, args, withHelpers) {
  return inFrame(tabId, 0, func, args, withHelpers);
}

function tabInfo(tab) {
  return { tab_id: tab.id, url: tab.url, title: tab.title, active: tab.active, group_id: tab.groupId, agent: inGroup(tab) };
}

// tabSensitive lists the kinds of secret fields in any frame of the tab.
async function tabSensitive(tabId) {
  const all = await inAllFrames(tabId, pageInspect, ["", false], true);
  return [...new Set(all.flatMap((r) => r.result.sensitive || []))].sort();
}

// prepare checks an action on an agent tab before it runs and returns the
// frame and the frame's own ref. kind is click, type, submit (type with
// submit), key (a key press), activate (Enter or Space) or hover.
async function prepare(tabId, ref, kind, guard) {
  const tab = await agentTab(tabId);
  const g = guard || {};
  if (kind !== "hover" && scripted.has(tabId)) {
    throw new Refused("this tab ran a script, so clicks, typing and key presses are refused in it until the agent " +
      "loads a page in it again with browser_navigate");
  }
  const { frameId, ref: local } = ref ? parseRef(tabId, ref) : { frameId: 0, ref: "" };
  if (kind === "hover") {
    return { frameId, local };
  }
  const frameHost = String(await inFrame(tabId, frameId, () => location.hostname) || "").toLowerCase();
  if ((kind === "click" || kind === "submit" || kind === "activate") &&
    (protectedHost(hostOf(tab.url)) || protectedHost(frameHost)) && !g.confirmed) {
    throw new Refused("on Salesforce (and the sites in browser.confirm-hosts) every click and every Enter or Space key " +
      "press needs the user's approval");
  }
  const sensitive = await tabSensitive(tabId);
  if (sensitive.includes("password") && (autologinHost(hostOf(tab.url)) || autologinHost(frameHost))) {
    throw new Refused("this is a login page on a host in browser.autologin-hosts; clicks, typing and key presses are " +
      "refused here, so the saved password cannot be shown or changed. Use browser_login");
  }
  if (sensitive.length && !g.sensitive) {
    throw new Refused("this page has " + sensitive.join(", ") + " fields; the user must confirm each action here");
  }
  return { frameId, local };
}

// pageGuard is the guard the page functions check right before they act,
// with the device's autologin hosts, which the plugin cannot set.
function pageGuard(guard) {
  return Object.assign({}, guard || {}, { autologin_hosts: [...autologinHosts] });
}

// onLoginPage reports whether the tab shows a host in
// browser.autologin-hosts, at the top or in a frame, and has a password
// field. Screenshots and selector waits are refused there.
async function onLoginPage(tab) {
  if (!autologinHosts.size) {
    return false;
  }
  const hosts = await inAllFrames(tab.id, () => (/^https?:$/.test(location.protocol) ? location.hostname.toLowerCase() : null));
  if (!autologinHost(hostOf(tab.url)) && !hosts.some((r) => autologinHost(r.result))) {
    return false;
  }
  return (await tabSensitive(tab.id)).includes("password");
}

// activate shows tab in its window, and the window when it is minimized,
// and returns what restore needs to put things back.
async function activate(tab) {
  const before = { tabId: tab.id, windowId: tab.windowId, prev: null, minimized: false };
  if (!tab.active) {
    const [prev] = await browser.tabs.query({ active: true, windowId: tab.windowId });
    before.prev = prev ? prev.id : null;
    await browser.tabs.update(tab.id, { active: true });
  }
  const win = await browser.windows.get(tab.windowId);
  if (win.state === "minimized") {
    before.minimized = true;
    await browser.windows.update(tab.windowId, { state: "normal" });
  }
  return before;
}

// restore makes the previously active tab active again, and minimizes the
// window again, unless the user changed the active tab meanwhile.
async function restore(before) {
  let restored = false;
  try {
    const [now] = await browser.tabs.query({ active: true, windowId: before.windowId });
    if (before.prev !== null && now && now.id === before.tabId) {
      await browser.tabs.update(before.prev, { active: true });
      restored = true;
    }
    if (before.minimized) {
      await browser.windows.update(before.windowId, { state: "minimized" });
    }
  } catch (e) {
    // A tab or window the user closed meanwhile needs nothing restored.
  }
  return restored;
}

// pageVisibility reads what Firefox's login autofill waits for.
function pageVisibility() {
  return { visibility: document.visibilityState, ready: document.readyState, focus: document.hasFocus() };
}

// pageLogin finds the one visible password field of the top frame and its
// username field, the way Firefox's login autofill does for a simple form,
// and reports only whether each is filled. With submit it also submits
// that form, after checking that the page and the form's target are on an
// autologin host, that it posts, and that it stays in this tab. It never
// returns a field value. It is self-contained, so nothing a page or an
// earlier script left in this world is used.
function pageLogin(hosts, tabHost, submit) {
  const allowed = new Set(hosts);
  if (location.protocol !== "https:") {
    return { error: "the page is not https" };
  }
  const here = location.hostname.toLowerCase();
  if (here !== tabHost || !allowed.has(here)) {
    return { error: "the page's host " + here + " is not the tab's autologin host" };
  }
  const shown = (el) => {
    const r = el.getBoundingClientRect();
    if (r.width === 0 && r.height === 0) {
      return false;
    }
    const s = getComputedStyle(el);
    return s.visibility !== "hidden" && s.display !== "none";
  };
  const passwords = [...document.querySelectorAll("input[type=password]")].filter(shown);
  const out = { visibility: document.visibilityState, ready: document.readyState, passwords: passwords.length };
  if (passwords.length !== 1) {
    return out;
  }
  const pw = passwords[0];
  const form = pw.form;
  const fields = form ? [...form.elements] : [...document.querySelectorAll("input")];
  const texty = (f) => f.tagName === "INPUT" && ["text", "email", "tel", "search", ""].includes(f.type) && shown(f);
  let user = fields.find((f) => texty(f) && /\b(username|email)\b/.test((f.getAttribute("autocomplete") || "").toLowerCase()));
  if (!user) {
    for (let i = fields.indexOf(pw) - 1; i >= 0; i--) {
      if (texty(fields[i])) {
        user = fields[i];
        break;
      }
    }
  }
  let autofilled = false;
  try {
    autofilled = pw.matches(":autofill");
  } catch (e) {
    autofilled = false;
  }
  Object.assign(out, {
    form: Boolean(form), password_filled: pw.value !== "", username_present: Boolean(user),
    username_filled: Boolean(user) && user.value !== "", autofilled,
    new_password: /new-password/.test((pw.getAttribute("autocomplete") || "").toLowerCase()),
  });
  if (!submit) {
    return out;
  }
  if (!form) {
    return Object.assign(out, { error: "the password field is not in a form" });
  }
  if (!out.password_filled || !out.username_filled) {
    return Object.assign(out, { error: "the login form is no longer filled" });
  }
  // Read the form through its prototype, so fields named "action" or
  // "method" cannot stand in for the form's own values.
  const get = (name) => Reflect.apply(Object.getOwnPropertyDescriptor(HTMLFormElement.prototype, name).get, form, []);
  const isSubmit = (e) => !e.disabled && ((e.tagName === "BUTTON" && e.type === "submit") ||
    (e.tagName === "INPUT" && (e.type === "submit" || e.type === "image")));
  const controls = [...get("elements")];
  const after = controls.slice(controls.indexOf(pw) + 1);
  const submitter = after.find(isSubmit) || controls.find(isSubmit);
  const has = (a) => submitter && submitter.hasAttribute(a);
  const method = String(has("formmethod") ? submitter.formMethod : get("method")).toLowerCase();
  const action = String(has("formaction") ? submitter.formAction : get("action"));
  const tgt = String(has("formtarget") ? submitter.formTarget : get("target")).toLowerCase();
  let to;
  try {
    to = new URL(action || location.href, location.href);
  } catch (e) {
    return Object.assign(out, { error: "the login form has no usable action" });
  }
  if (to.protocol !== "https:" || !allowed.has(to.hostname.toLowerCase())) {
    return Object.assign(out, { error: "the login form submits to " + to.hostname + ", which is not in browser.autologin-hosts" });
  }
  if (method !== "post") {
    return Object.assign(out, { error: "the login form does not use POST, so the password would go into the address" });
  }
  if (tgt && tgt !== "_self") {
    return Object.assign(out, { error: "the login form submits into another tab or frame" });
  }
  if (submitter) {
    form.requestSubmit(submitter);
  } else {
    form.requestSubmit();
  }
  return Object.assign(out, { submitted: true });
}

const NOT_FILLED = "Firefox did not fill the login form. Likely causes: no login is saved for this host in this " +
  "Firefox profile; more than one login is saved for it (Firefox then fills nothing until the user picks one); the " +
  "primary password is locked; or autofill is off (signon.autofillForms). The user must sign in once by hand.";

// composeSnapshot joins the snapshots of the tab's frames: each frame's
// outline goes under its iframe line, and its refs carry the frame's
// number (f2e5). With a selector or a ref it outlines only the matching
// elements (and the frames inside them), and keeps the refs that earlier
// snapshots gave out.
async function composeSnapshot(tabId, interactiveOnly, selector, ref) {
  const tab = await browser.tabs.get(tabId);
  let scope = null;
  if (ref) {
    const r = parseRef(tabId, ref);
    scope = { frame: r.frameId, ref: r.ref };
  } else if (selector) {
    scope = { selector: String(selector).slice(0, 500) };
  }
  const res = await inAllFrames(tabId, pageSnapshot, [Boolean(interactiveOnly), autologinHost(hostOf(tab.url)),
    [...autologinHosts], scope], true);
  const byFrame = new Map(res.map((r) => [r.frameId, r.result]));
  const top = byFrame.get(0);
  if (!top) {
    throw new Error("could not read the page");
  }
  const bad = res.find((r) => r.result.scope_error);
  if (bad) {
    throw new Refused(bad.result.scope_error);
  }
  const lines = [];
  const seen = new Set();
  let count = 0;
  const emit = (frameId, base) => {
    seen.add(frameId);
    const prefix = frameId ? "f" + frameNum(tabId, frameId) : "";
    for (const e of byFrame.get(frameId).entries) {
      const indent = "  ".repeat(base + e.d);
      if (e.r === undefined) {
        if (e.frame && byFrame.has(e.frame) && !seen.has(e.frame)) {
          lines.push(indent + "- " + e.t + " [frame=f" + frameNum(tabId, e.frame) + "]");
          emit(e.frame, base + e.d + 1);
        } else {
          lines.push(indent + "- " + e.t + " [frame not readable]");
        }
        continue;
      }
      count++;
      lines.push(indent + "- " + e.t + " [ref=" + prefix + e.r + "]");
    }
  };
  if (!scope) {
    emit(0, 0);
    return { title: top.title, url: top.url, snapshot: lines.join("\n"), element_count: count };
  }
  // A scoped frame outlines only its matches; the others outline everything,
  // for when a match holds their iframe.
  const roots = res.filter((r) => r.result.matched > 0).map((r) => r.frameId).sort((a, b) => (a ? 1 : 0) - (b ? 1 : 0));
  if (scope.ref !== undefined && !roots.includes(scope.frame)) {
    throw new Refused("unknown or stale ref " + ref + "; take a new snapshot without ref or selector");
  }
  for (const frameId of roots) {
    if (seen.has(frameId)) {
      continue;
    }
    if (frameId) {
      lines.push("- frame f" + frameNum(tabId, frameId) + " " + JSON.stringify(hostOf(byFrame.get(frameId).url)));
      emit(frameId, 1);
    } else {
      emit(0, 0);
    }
  }
  return { title: top.title, url: top.url, snapshot: lines.join("\n"), element_count: count,
    scope: scope.ref !== undefined ? "ref" : "selector", matched: res.reduce((n, r) => n + (r.result.matched || 0), 0) };
}

// composeText joins the text of the tab's frames, each frame's text after
// its parent's under a "[frame f2: host]" line.
async function composeText(tabId) {
  const res = await inAllFrames(tabId, pageText, [], true);
  const byFrame = new Map(res.map((r) => [r.frameId, r.result]));
  const top = byFrame.get(0);
  if (!top) {
    throw new Error("could not read the page");
  }
  const seen = new Set();
  const text = (frameId) => {
    seen.add(frameId);
    const f = byFrame.get(frameId);
    let out = f.text;
    for (const child of f.frames) {
      if (byFrame.has(child) && !seen.has(child)) {
        out += "\n\n[frame f" + frameNum(tabId, child) + ": " + hostOf(byFrame.get(child).url) + "]\n" + text(child);
      }
    }
    return out;
  };
  return { title: top.title, url: top.url, text: text(0) };
}

// evalCode wraps the agent's script, the body of an async function, so
// its result comes back as JSON. fetch and XMLHttpRequest are the page's
// own, so requests go out as the page's, with its cookies and origin.
function evalCode(script, site, blocked) {
  // The extension API (native messaging, tabs) is
  // taken out of this world before agent code runs; the helpers keep only
  // getFrameId, saved by installHelpers. The script runs only in a document
  // on the approved site that is not an autologin host and has no filled
  // password field: the frame can load another document between the checks
  // and the injection, and a password Firefox filled must never be read.
  return "(async () => {\n" +
    "{\n" +
    "  const h = location.hostname.toLowerCase().replace(/\\.$/, '');\n" +
    "  const s = h.startsWith('www.') ? h.slice(4) : h;\n" +
    "  if (!/^https?:$/.test(location.protocol) || s !== " + JSON.stringify(site) + " || " + JSON.stringify(blocked) + ".includes(h)) {\n" +
    "    return { ok: false, refused: true, error: 'the frame shows another page than the one approved; take a new snapshot' };\n" +
    "  }\n" +
    "  if ([...document.querySelectorAll('input[type=password]')].some((f) => f.value !== '')) {\n" +
    "    return { ok: false, refused: true, error: 'scripts are refused on a page with a filled password field' };\n" +
    "  }\n" +
    "}\n" +
    "for (const k of ['browser', 'chrome', 'messenger']) {\n" +
    "  try { delete globalThis[k]; } catch (e) {}\n" +
    "  try { if (globalThis[k] !== undefined) { Object.defineProperty(globalThis, k, {value: undefined}); } } catch (e) {}\n" +
    "  if (globalThis[k] !== undefined) { return { ok: false, error: 'the extension API could not be hidden from the script' }; }\n" +
    "}\n" +
    "const fetch = (...a) => content.fetch(...a);\n" +
    "const XMLHttpRequest = content.XMLHttpRequest;\n" +
    "try {\n" +
    "const __hermesValue = await (async () => {\n" + script + "\n})();\n" +
    "return { ok: true, json: JSON.stringify(__hermesValue === undefined ? null : __hermesValue), undef: __hermesValue === undefined };\n" +
    "} catch (e) {\n" +
    "return { ok: false, error: String((e && e.message) || e) };\n" +
    "}\n" +
    "})();\n";
}

// Page functions run in the extension's isolated world in the page, so the
// page cannot see or change the element refs. Each function is sent alone,
// so helpers it needs come from installHelpers.

function pageText() {
  if (!/^https?:$/.test(location.protocol)) {
    return null;
  }
  const frames = [];
  for (const f of document.querySelectorAll("iframe, frame")) {
    const r = f.getBoundingClientRect();
    if (r.width > 0 && r.height > 0) {
      try {
        frames.push(globalThis.__hermesFrameId(f));
      } catch (e) {
        // A frame Firefox cannot name is left out.
      }
    }
  }
  return { title: document.title, url: location.href, text: globalThis.__hermesHelpers.textOf(), frames };
}

function pageSnapshot(interactiveOnly, hideValues, hideHosts, scope) {
  if (!/^https?:$/.test(location.protocol)) {
    return null;
  }
  // On an autologin host no field value is shown at all, so a field whose
  // type a page changed cannot show a filled password.
  const noValues = Boolean(hideValues) || (hideHosts || []).includes(location.hostname.toLowerCase());
  // A scoped snapshot keeps the frame's refs and adds to them, so refs from
  // the last full snapshot still work.
  const kept = scope && globalThis.__hermes;
  const state = kept || (globalThis.__hermes = { refs: new Map(), next: 1 });
  const known = new Map();
  if (kept) {
    for (const [r, w] of state.refs) {
      const el = w.deref();
      if (el) {
        known.set(el, r);
      }
    }
  }
  const implicit = {
    A: "link", BUTTON: "button", SELECT: "combobox", TEXTAREA: "textbox", H1: "heading", H2: "heading",
    H3: "heading", H4: "heading", H5: "heading", H6: "heading", IMG: "img", SUMMARY: "button", P: "paragraph",
    LI: "listitem", NAV: "navigation", MAIN: "main", FORM: "form", TABLE: "table", DIALOG: "dialog",
  };
  const inputRoles = { checkbox: "checkbox", radio: "radio", submit: "button", button: "button", reset: "button", range: "slider", search: "searchbox" };
  const roles = new Set(["link", "button", "textbox", "searchbox", "combobox", "checkbox", "radio", "slider", "tab",
    "menuitem", "option", "switch", "heading", "img", "paragraph", "listitem", "navigation", "main", "form", "table",
    "dialog", "listbox", "menu", "tablist", "tabpanel", "alert", "banner", "contentinfo", "region", "search"]);
  const role = (el) => {
    const r = (el.getAttribute("role") || "").trim().toLowerCase();
    if (r) {
      return roles.has(r) ? r : null;
    }
    if (el.tagName === "INPUT") {
      return el.type === "hidden" ? null : inputRoles[el.type] || "textbox";
    }
    if (el.tagName === "A" && !el.hasAttribute("href")) {
      return null;
    }
    if (el.isContentEditable && el.parentElement && !el.parentElement.isContentEditable) {
      return "textbox";
    }
    return implicit[el.tagName] || null;
  };
  // Names come from the page, so they are quoted as JSON strings and lose
  // brackets: page text cannot forge snapshot lines or refs.
  const clean = (s) => (s || "").replace(/\s+/g, " ").replace(/[[\]]/g, "").trim().slice(0, 120);
  const name = (el) => {
    const by = el.getAttribute("aria-labelledby");
    if (by) {
      const root = el.getRootNode();
      const byId = (id) => (root.getElementById ? root.getElementById(id) : document.getElementById(id));
      const t = by.split(/\s+/).map(byId).filter(Boolean).map((e) => e.textContent).join(" ");
      if (clean(t)) {
        return clean(t);
      }
    }
    const direct = el.getAttribute("aria-label") || el.getAttribute("alt") || el.getAttribute("title");
    if (clean(direct)) {
      return clean(direct);
    }
    if (el.labels && el.labels.length) {
      return clean(el.labels[0].textContent);
    }
    if (el.tagName === "INPUT" || el.tagName === "TEXTAREA") {
      return clean(el.getAttribute("placeholder"));
    }
    return clean(el.innerText || el.textContent);
  };
  const visible = (el) => {
    const r = el.getBoundingClientRect();
    if (r.width === 0 && r.height === 0) {
      return false;
    }
    const s = getComputedStyle(el);
    return s.visibility !== "hidden" && s.display !== "none";
  };
  // The outline follows the rendered tree: a shadow host's shadow tree in
  // place of its children, and a slot's assigned elements.
  const kids = (el) => {
    if (el.tagName === "SLOT") {
      const assigned = el.assignedElements({ flatten: true });
      return assigned.length ? assigned : [...el.children];
    }
    const sr = el.openOrClosedShadowRoot || el.shadowRoot;
    return sr ? [...sr.children] : [...el.children];
  };
  const interactive = new Set(["link", "button", "textbox", "searchbox", "combobox", "checkbox", "radio", "slider", "tab", "menuitem", "option", "switch"]);
  const entries = [];
  const walk = (list, depth) => {
    for (const el of list) {
      if (el.tagName === "IFRAME" || el.tagName === "FRAME") {
        if (visible(el)) {
          let frame = null;
          try {
            frame = globalThis.__hermesFrameId(el);
          } catch (e) {
            frame = null;
          }
          const n = clean(el.getAttribute("title") || el.getAttribute("aria-label") || el.getAttribute("name") || "");
          entries.push({ d: depth, t: "iframe" + (n ? " " + JSON.stringify(n) : ""), frame });
        }
        continue;
      }
      const r = role(el);
      let next = depth;
      if (r && visible(el) && (!interactiveOnly || interactive.has(r))) {
        let ref = known.get(el);
        if (!ref) {
          ref = "e" + state.next++;
          state.refs.set(ref, new WeakRef(el));
        }
        let line = r;
        const n = name(el);
        if (n) {
          line += " " + JSON.stringify(n);
        }
        if (r === "heading") {
          const level = /^H[1-6]$/.test(el.tagName) ? el.tagName[1] : parseInt(el.getAttribute("aria-level"), 10) || 2;
          line += " [level=" + level + "]";
        }
        const secret = el.type === "password" ||
          /\bcc-|one-time-code|password/.test((el.getAttribute("autocomplete") || "").toLowerCase()) ||
          /(card.?num|cc.?num|cvc|cvv|csc|card.?code|security.?code|otp|one.?time|totp|2fa|mfa|verification.?code|auth.?code)/
            .test(((el.name || "") + " " + (el.id || "")).toLowerCase());
        if ((el.tagName === "INPUT" || el.tagName === "TEXTAREA") && !secret && !noValues && el.value) {
          line += " value=" + JSON.stringify(clean(el.value));
        }
        if (el.type === "checkbox" || el.type === "radio") {
          line += el.checked ? " [checked]" : "";
        }
        entries.push({ d: depth, t: line, r: ref });
        next = depth + 1;
      }
      walk(kids(el), next);
    }
  };
  const out = { title: document.title, url: location.href, entries };
  if (!scope) {
    if (document.body) {
      walk(kids(document.body), 0);
    }
    return out;
  }
  let matches = [];
  let self = null;
  try {
    self = globalThis.__hermesFrameId(window);
  } catch (e) {
    self = null;
  }
  if (scope.ref !== undefined) {
    if (self === scope.frame) {
      const w = state.refs.get(scope.ref);
      const el = w && w.deref();
      if (el && el.isConnected) {
        matches = [el];
      }
    }
  } else if (document.body) {
    try {
      document.body.matches(scope.selector);
    } catch (e) {
      return Object.assign(out, { scope_error: "selector is not a valid CSS selector: " + String(scope.selector).slice(0, 100) });
    }
    const all = globalThis.__hermesHelpers.allElements(document.body).filter((el) => el.matches(scope.selector));
    matches = all.filter((el) => !all.some((o) => o !== el && o.contains(el))).slice(0, 50);
  }
  if (matches.length) {
    walk(matches, 0);
  } else if (document.body) {
    walk(kids(document.body), 0);
  }
  out.matched = matches.length;
  return out;
}

// installHelpers defines the classification helpers in the page's isolated
// world, where the other page functions find them. executeScript sends one
// function's source, so the helpers travel inside this one.
function installHelpers() {
  if (!globalThis.__hermesFrameId && typeof browser !== "undefined") {
    const runtime = browser.runtime;
    globalThis.__hermesFrameId = (target) => runtime.getFrameId(target);
  }
  const shadowOf = (el) => el.openOrClosedShadowRoot || el.shadowRoot || null;

  // allElements lists every element under root, also inside shadow roots.
  function allElements(root) {
    const out = [];
    const visit = (r) => {
      for (const el of r.querySelectorAll("*")) {
        out.push(el);
        const sr = shadowOf(el);
        if (sr) {
          visit(sr);
        }
      }
    };
    visit(root);
    return out;
  }

  // textOf is the page's visible text, with the text of shadow trees that
  // innerText leaves out added at the end.
  function textOf() {
    const body = document.body;
    const text = body ? body.innerText : "";
    if (!body) {
      return text;
    }
    const skip = new Set(["STYLE", "SCRIPT", "TEMPLATE", "NOSCRIPT"]);
    const extra = [];
    for (const el of allElements(body)) {
      const sr = shadowOf(el);
      if (!sr) {
        continue;
      }
      const t = [...sr.children].filter((c) => !skip.has(c.tagName)).map((c) => c.innerText || "").join("\n").trim();
      if (t && !text.includes(t)) {
        extra.push(t);
      }
    }
    return extra.length ? text + "\n" + extra.join("\n") : text;
  }

  function deepActive() {
    let el = document.activeElement;
    while (el && shadowOf(el) && shadowOf(el).activeElement) {
      el = shadowOf(el).activeElement;
    }
    return el && el !== document.body ? el : null;
  }

  // elementFor returns the element of a ref from the last snapshot, or
  // with focus and no ref the focused element.
  function elementFor(ref, focus) {
    if (!ref) {
      return focus ? deepActive() : null;
    }
    const state = globalThis.__hermes;
    const entry = state && state.refs.get(String(ref).replace(/^@/, ""));
    const el = entry && entry.deref();
    if (!el || !el.isConnected) {
      throw new Error("unknown or stale ref " + ref + "; take a new snapshot");
    }
    return el;
  }

  // pageClassify describes the frame and, with a ref (or with focus, the
  // focused element), that element, so the plugin can decide what to ask
  // the user (docs/browser.md section 7). sensitive lists the kinds of
  // secret fields in the frame: password, payment and one_time_code.
  function pageClassify(ref, focus) {
    const host = (u) => {
      try {
        return new URL(u, location.href).hostname.toLowerCase();
      } catch (e) {
        return "";
      }
    };
    const payHosts = /(^|\.)(stripe\.com|stripe\.network|braintreegateway\.com|braintree-api\.com|adyen\.com|paypal\.com|checkout\.com|squareup\.com|klarna\.com|recurly\.com)$/;
    const sensitive = new Set();
    const elements = allElements(document);
    for (const el of elements) {
      if (el.tagName === "IFRAME" && el.src && payHosts.test(host(el.src))) {
        sensitive.add("payment");
      }
      if (el.tagName !== "INPUT" && el.tagName !== "TEXTAREA" && el.tagName !== "SELECT") {
        continue;
      }
      const ac = (el.getAttribute("autocomplete") || "").toLowerCase();
      const id = ((el.name || "") + " " + (el.id || "")).toLowerCase();
      if (el.type === "password" || /current-password|new-password/.test(ac)) {
        sensitive.add("password");
      }
      if (/\bcc-(number|csc|exp|exp-month|exp-year|name|type)\b/.test(ac) || /(card.?num|cc.?num|cvc|cvv|csc|card.?code|security.?code|expir)/.test(id)) {
        sensitive.add("payment");
      }
      if (/one-time-code/.test(ac) || /(otp|one.?time|totp|2fa|mfa|verification.?code|auth.?code)/.test(id)) {
        sensitive.add("one_time_code");
      }
    }
    const out = { url: location.href, host: location.hostname.toLowerCase(), sensitive: [...sensitive].sort() };
    let el;
    try {
      el = elementFor(ref, focus);
    } catch (e) {
      return Object.assign(out, { error: e.message });
    }
    if (!el) {
      return out;
    }
    // A click activates the nearest button or link around the element, or
    // the control of a label around it.
    const label = el.closest("label");
    const actor = (label && label.control) || el;
    const button = actor.closest("button, input[type=submit], input[type=image]");
    const link = actor.closest("a[href]");
    const form = (button && button.form) || actor.form || actor.closest("form");
    const submitter = button && button.form && (button.type === "submit" || button.type === "image");
    let target = { kind: "other" };
    if (submitter) {
      target = { kind: "submit", host: host(button.formAction || button.form.action || location.href) };
    } else if (link && !/^(javascript|mailto|tel):/i.test(link.getAttribute("href"))) {
      target = { kind: "link", host: host(link.href), url: link.href };
    } else if (el.tagName === "INPUT" || el.tagName === "TEXTAREA" || el.isContentEditable) {
      target = { kind: "field", host: host(form ? form.action || location.href : location.href), form: Boolean(form) };
      if (el.type === "password" || /current-password|new-password/.test((el.getAttribute("autocomplete") || "").toLowerCase())) {
        target.field = "password";
      }
    }
    // A field's value is never a label: it can be a password.
    const buttonValue = actor.tagName === "INPUT" && ["submit", "button", "reset"].includes(actor.type) ? actor.value : "";
    const name = (actor.getAttribute("aria-label") || actor.innerText || buttonValue || actor.getAttribute("title") || "")
      .replace(/\s+/g, " ").trim().slice(0, 80);
    if (name) {
      target.label = name;
    }
    return Object.assign(out, { target });
  }

  // guardCheck refuses an action the plugin did not get approved: a submit,
  // a link to another host, or any action on a page with secret fields. It
  // runs in the page right before the action, so a page that changed since
  // the plugin asked cannot slip an action past the user.
  function guardCheck(info, action, submit, guard) {
    const g = guard || {};
    if ((g.autologin_hosts || []).includes(info.host) && info.sensitive.includes("password")) {
      return "this is a login page on a host in browser.autologin-hosts; clicks, typing and key presses are refused " +
        "here. Use browser_login";
    }
    if (info.sensitive.length && !g.sensitive) {
      return "this page has " + info.sensitive.join(", ") + " fields; the user must confirm each action here";
    }
    const t = info.target || { kind: "other" };
    const submits = (action === "click" && t.kind === "submit") || (action === "type" && submit);
    if (submits && !(g.submit_hosts || []).includes(t.host || info.host)) {
      return "submitting a form to " + (t.host || info.host) + " needs the user's approval";
    }
    const site = (h) => {
      const x = String(h || "").toLowerCase();
      return x.startsWith("www.") ? x.slice(4) : x;
    };
    if (action === "click" && t.kind === "link" && t.host && site(t.host) !== site(info.host) &&
      !(g.visit_hosts || []).map(site).includes(site(t.host))) {
      return "following a link to " + t.host + " needs the user's approval";
    }
    return "";
  }

  // findText returns the element whose text contains needle, searching
  // text nodes in the document and in shadow trees.
  function findText(needle) {
    const want = String(needle);
    const lower = want.toLowerCase();
    let loose = null;
    const roots = [document.body || document.documentElement];
    for (const el of allElements(document)) {
      const sr = shadowOf(el);
      if (sr) {
        roots.push(sr);
      }
    }
    for (const root of roots) {
      const w = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
      for (let n = w.nextNode(); n; n = w.nextNode()) {
        const t = n.nodeValue.replace(/\s+/g, " ");
        if (t.includes(want)) {
          return n.parentElement;
        }
        if (!loose && t.toLowerCase().includes(lower)) {
          loose = n.parentElement;
        }
      }
    }
    return loose;
  }

  globalThis.__hermesHelpers = { pageClassify, guardCheck, elementFor, textOf, findText, deepActive, allElements };
}

function pageInspect(ref, focus) {
  if (!/^https?:$/.test(location.protocol)) {
    return null;
  }
  return globalThis.__hermesHelpers.pageClassify(ref, focus);
}

function pageAct(action, ref, text, submit, guard) {
  const { pageClassify, guardCheck, elementFor } = globalThis.__hermesHelpers;
  const info = pageClassify(ref);
  if (info.error) {
    throw new Error(info.error);
  }
  const why = guardCheck(info, action, submit, guard);
  if (why) {
    return { refused: why };
  }
  const el = elementFor(ref);
  el.scrollIntoView({ block: "center", inline: "center" });
  const r = el.getBoundingClientRect();
  const opts = { bubbles: true, cancelable: true, composed: true, clientX: r.x + r.width / 2, clientY: r.y + r.height / 2, view: window };
  if (action === "click") {
    el.dispatchEvent(new PointerEvent("pointerdown", opts));
    el.dispatchEvent(new MouseEvent("mousedown", opts));
    if (el.focus) {
      el.focus();
    }
    el.dispatchEvent(new PointerEvent("pointerup", opts));
    el.dispatchEvent(new MouseEvent("mouseup", opts));
    // Page handlers of the events above can rewrite links and forms, so
    // check again right before the activation.
    const again = guardCheck(pageClassify(ref), action, submit, guard);
    if (again) {
      return { refused: again };
    }
    el.click();
    return { clicked: ref };
  }
  if (action === "type") {
    el.focus();
    if (el.isContentEditable) {
      document.execCommand("selectAll", false, null);
      document.execCommand("insertText", false, text);
    } else {
      el.value = text;
      el.dispatchEvent(new InputEvent("input", { bubbles: true, inputType: "insertText", data: text }));
      el.dispatchEvent(new Event("change", { bubbles: true }));
    }
    if (submit) {
      const enter = { key: "Enter", code: "Enter", keyCode: 13, bubbles: true, cancelable: true };
      const go = el.dispatchEvent(new KeyboardEvent("keydown", enter));
      el.dispatchEvent(new KeyboardEvent("keyup", enter));
      const again = guardCheck(pageClassify(ref), action, submit, guard);
      if (again) {
        return { refused: again };
      }
      if (go && el.form) {
        el.form.requestSubmit();
      }
    }
    return { typed: ref, submitted: Boolean(submit) };
  }
  throw new Error("unknown action " + action);
}

// pageKey presses one key on the ref's element (focused first) or on the
// focused element. Synthetic key events have no default action in
// Firefox, so it does the usual one itself when the page did not cancel
// the keydown: Enter and Space activate buttons and links and Enter
// submits a field's form (both checked against the guard first), Tab moves
// the focus, the arrow and page keys scroll, and a character is typed.
function pageKey(ref, key, guard) {
  const { pageClassify, guardCheck, elementFor, deepActive } = globalThis.__hermesHelpers;
  const info = pageClassify(ref, true);
  if (info.error) {
    throw new Error(info.error);
  }
  let el = elementFor(ref, true);
  if (el && ref && el.focus) {
    el.focus();
  }
  el = el || document.body || document.documentElement;
  const textField = (e) => e && (e.tagName === "TEXTAREA" || e.isContentEditable ||
    (e.tagName === "INPUT" && !["button", "submit", "reset", "checkbox", "radio", "image", "file", "range", "color"].includes(e.type)));
  const t = info.target || { kind: "other" };
  let action = "";
  if (key.key === "Enter" && t.kind === "field" && el.tagName === "INPUT" && el.form) {
    action = "submit";
  } else if (key.key === "Enter" && (t.kind === "submit" || t.kind === "link" || el.tagName === "BUTTON" ||
    (el.getAttribute && el.getAttribute("role") === "button"))) {
    action = "click";
  } else if (key.key === " " && !textField(el) && (t.kind === "submit" || el.tagName === "BUTTON" ||
    (el.tagName === "INPUT" && ["checkbox", "radio", "button", "submit", "reset"].includes(el.type)) ||
    (el.getAttribute && ["button", "checkbox", "switch", "radio", "tab", "menuitem", "option"].includes(el.getAttribute("role"))))) {
    action = "click";
  }
  const check = () => (action ? guardCheck(pageClassify(ref, true), action === "submit" ? "type" : "click", action === "submit", guard) : "");
  const why = check();
  if (why) {
    return { refused: why };
  }
  const init = {
    key: key.key, code: key.key.length === 1 ? (/[a-z]/i.test(key.key) ? "Key" + key.key.toUpperCase() : "") : key.key,
    keyCode: key.code, which: key.code, ctrlKey: key.ctrlKey, shiftKey: key.shiftKey, altKey: key.altKey,
    metaKey: key.metaKey, bubbles: true, cancelable: true, composed: true,
  };
  if (key.key === " ") {
    init.code = "Space";
  }
  const go = el.dispatchEvent(new KeyboardEvent("keydown", init));
  const printable = key.key.length === 1 && !key.ctrlKey && !key.metaKey && !key.altKey;
  let pressGo = go;
  if (go && (printable || key.key === "Enter")) {
    pressGo = el.dispatchEvent(new KeyboardEvent("keypress", Object.assign({}, init, { charCode: key.key === "Enter" ? 13 : key.key.charCodeAt(0) })));
  }
  let done = "";
  if (go && pressGo) {
    if (action) {
      const again = check();
      if (again) {
        el.dispatchEvent(new KeyboardEvent("keyup", init));
        return { refused: again };
      }
      if (action === "submit") {
        el.form.requestSubmit();
        done = "submitted";
      } else {
        el.click();
        done = "clicked";
      }
    } else if (key.key === "Tab") {
      const sel = "a[href], button, input:not([type=hidden]), select, textarea, [tabindex], [contenteditable=true], summary";
      const all = [...document.querySelectorAll(sel)].filter((e) => !e.disabled && e.tabIndex >= 0 && e.getClientRects().length);
      const cur = deepActive();
      let i = all.indexOf(cur);
      i = key.shiftKey ? (i <= 0 ? all.length - 1 : i - 1) : (i + 1) % Math.max(all.length, 1);
      if (all[i]) {
        all[i].focus();
        done = "focus moved";
      }
    } else if (printable && textField(el)) {
      if (el.isContentEditable) {
        document.execCommand("insertText", false, key.key);
      } else {
        el.setRangeText(key.key, el.selectionStart ?? el.value.length, el.selectionEnd ?? el.value.length, "end");
        el.dispatchEvent(new InputEvent("input", { bubbles: true, inputType: "insertText", data: key.key }));
      }
      done = "typed";
    } else if (key.key === "Backspace" && textField(el) && !el.isContentEditable) {
      const s = el.selectionStart ?? el.value.length;
      const e = el.selectionEnd ?? s;
      el.setRangeText("", s === e ? Math.max(s - 1, 0) : s, e, "end");
      el.dispatchEvent(new InputEvent("input", { bubbles: true, inputType: "deleteContentBackward" }));
      done = "deleted";
    } else if ((key.ctrlKey || key.metaKey) && key.key.toLowerCase() === "a" && textField(el) && el.select) {
      el.select();
      done = "selected all";
    } else if (!textField(el) && ["ArrowDown", "ArrowUp", "PageDown", "PageUp", "Home", "End", " "].includes(key.key)) {
      const page = window.innerHeight * 0.85;
      const by = { ArrowDown: 40, ArrowUp: -40, PageDown: page, PageUp: -page, " ": key.shiftKey ? -page : page }[key.key];
      if (key.key === "Home") {
        window.scrollTo({ top: 0, behavior: "instant" });
      } else if (key.key === "End") {
        window.scrollTo({ top: document.documentElement.scrollHeight, behavior: "instant" });
      } else {
        window.scrollBy({ top: by, behavior: "instant" });
      }
      done = "scrolled";
    }
  }
  el.dispatchEvent(new KeyboardEvent("keyup", init));
  const now = deepActive();
  return {
    pressed: key.key === " " ? "Space" : key.key, default_action: done || (go ? "none" : "cancelled by the page"),
    focused: now ? (now.tagName.toLowerCase() + (now.getAttribute("aria-label") ? " " + JSON.stringify(now.getAttribute("aria-label").slice(0, 80)) : "")) : "",
  };
}

// pageHover moves the pointer onto the ref's element, as far as page
// scripts can tell.
function pageHover(ref) {
  const el = globalThis.__hermesHelpers.elementFor(ref);
  el.scrollIntoView({ block: "center", inline: "center" });
  const r = el.getBoundingClientRect();
  const opts = { bubbles: true, cancelable: true, composed: true, clientX: r.x + r.width / 2, clientY: r.y + r.height / 2, view: window };
  el.dispatchEvent(new PointerEvent("pointerover", opts));
  el.dispatchEvent(new PointerEvent("pointerenter", Object.assign({}, opts, { bubbles: false })));
  el.dispatchEvent(new MouseEvent("mouseover", opts));
  el.dispatchEvent(new MouseEvent("mouseenter", Object.assign({}, opts, { bubbles: false })));
  el.dispatchEvent(new PointerEvent("pointermove", opts));
  el.dispatchEvent(new MouseEvent("mousemove", opts));
  return { hovered: ref };
}

function pageFind(text) {
  if (!/^https?:$/.test(location.protocol)) {
    return null;
  }
  return { found: Boolean(globalThis.__hermesHelpers.findText(text)) };
}

// pageScroll scrolls the frame, the scrollable area around the ref's
// element, or brings an element (by ref or by text) into view.
function pageScroll(ref, direction, pages, toText) {
  const { elementFor, findText } = globalThis.__hermesHelpers;
  const doc = document.scrollingElement || document.documentElement;
  let el = ref ? elementFor(ref) : null;
  if (toText) {
    el = findText(toText);
    if (!el) {
      return { found: false };
    }
  }
  let box = doc;
  if (el && direction) {
    for (let p = el; p && p !== document.body && p !== document.documentElement;) {
      const s = getComputedStyle(p);
      if (/(auto|scroll|overlay)/.test(s.overflowY) && p.scrollHeight > p.clientHeight + 1) {
        box = p;
        break;
      }
      p = p.parentElement || (p.getRootNode && p.getRootNode().host) || null;
    }
  } else if (el) {
    el.scrollIntoView({ block: "center", inline: "nearest", behavior: "instant" });
  }
  const view = box === doc ? window.innerHeight : box.clientHeight;
  if (direction) {
    const by = view * 0.85 * (pages || 1);
    const top = { down: box.scrollTop + by, up: box.scrollTop - by, top: 0, bottom: box.scrollHeight }[direction];
    box.scrollTo({ top, behavior: "instant" });
  }
  const out = {
    scrolled: box === doc ? "page" : "area", scroll_y: Math.round(box.scrollTop), scroll_height: box.scrollHeight,
    view_height: view, at_top: box.scrollTop <= 0, at_bottom: box.scrollTop + view >= box.scrollHeight - 2,
  };
  if (toText) {
    out.found = true;
  }
  return out;
}

function pageHas(text, selector, blocked) {
  if (!/^https?:$/.test(location.protocol)) {
    return null;
  }
  if (selector && (blocked || []).includes(location.hostname.toLowerCase()) && document.querySelector("input[type=password]")) {
    return { error: "CSS selector waits are refused on a login page of a host in browser.autologin-hosts; wait for a text" };
  }
  if (selector) {
    try {
      if (document.querySelector(selector)) {
        return { found: true };
      }
    } catch (e) {
      return { error: "bad CSS selector: " + e.message };
    }
  }
  if (text && globalThis.__hermesHelpers.textOf().includes(text)) {
    return { found: true };
  }
  return { found: false };
}

const OPS = {
  async tabs_list() {
    const tabs = await browser.tabs.query({});
    return { tabs: tabs.filter(inGroup).map(tabInfo) };
  },
  async tab_open({ url, guard }) {
    const target = checkURL(url || "https://example.com/");
    checkVisit(target, "", guard);
    // Group the blank tab first, so the private network block covers the
    // first request and its redirects.
    const tab = await browser.tabs.create({ url: "about:blank", active: false });
    await addToGroup(tab.id);
    const loaded = waitComplete(tab.id, true);
    await browser.tabs.update(tab.id, { url: target });
    return tabInfo(await loaded);
  },
  async tab_close({ tab_id }) {
    await agentTab(tab_id, true);
    await browser.tabs.remove(tab_id);
    return { closed: tab_id };
  },
  async navigate({ tab_id, url, guard }) {
    const tab = await agentTab(tab_id, true);
    const target = checkURL(url);
    let from = "";
    try {
      from = /^https?:/.test(tab.url || "") ? new URL(tab.url).hostname : "";
    } catch (e) {
      from = "";
    }
    checkVisit(target, from, guard);
    return tabInfo(await loadIn(tab_id, target));
  },
  async back({ tab_id }) {
    await agentTab(tab_id);
    if (everScripted.has(tab_id)) {
      throw new Refused("this tab ran a script, so it cannot go back; open the page with browser_navigate");
    }
    const loaded = waitComplete(tab_id, true);
    await browser.tabs.goBack(tab_id);
    return tabInfo(await loaded);
  },
  async read_text({ tab_id, max_chars, offset, sessions }) {
    await agentTab(tab_id, false, sessions);
    const page = await composeText(tab_id);
    const max = Math.max(1, Math.min(Number(max_chars) || MAX_TEXT, MAX_TEXT));
    const start = Math.max(0, Math.min(Number(offset) || 0, page.text.length));
    const end = Math.min(start + max, page.text.length);
    const out = { title: page.title, url: page.url, text: page.text.slice(start, end), truncated: end < page.text.length,
      total_chars: page.text.length };
    if (end < page.text.length) {
      out.next_offset = end;
    }
    return out;
  },
  async snapshot({ tab_id, interactive_only, selector, ref, sessions }) {
    await agentTab(tab_id, false, sessions);
    return composeSnapshot(tab_id, interactive_only, selector, ref);
  },
  async inspect({ tab_id, ref, url, frame, focus }) {
    const out = {};
    if (url) {
      const u = new URL(checkURL(url));
      out.url_host = u.hostname.toLowerCase();
      out.url_autologin = u.protocol === "https:" && autologinHost(out.url_host);
    }
    if (tab_id === undefined || tab_id === null) {
      return out;
    }
    const tab = await agentTab(tab_id);
    const { frameId, ref: local } = ref ? parseRef(tab_id, ref) : { frameId: frameIdOf(tab_id, frame), ref: "" };
    const info = await inFrame(tab_id, frameId, pageInspect, [local, Boolean(focus)], true);
    if (!info) {
      throw new Refused("the frame does not show an http or https page");
    }
    const topHost = hostOf(tab.url);
    return Object.assign(info, out, {
      sensitive: await tabSensitive(tab_id), top_host: topHost, scripted: scripted.has(tab_id),
      protected: protectedHost(topHost) || protectedHost(info.host),
      autologin: autologinHost(topHost) || autologinHost(info.host),
      top_autologin: /^https:/.test(tab.url || "") && autologinHost(topHost),
    });
  },
  async click({ tab_id, ref, guard }) {
    const { frameId, local } = await prepare(tab_id, String(ref), "click", guard);
    const res = await inFrame(tab_id, frameId, pageAct, ["click", local, "", false, pageGuard(guard)], true);
    if (res.refused) {
      throw new Refused(res.refused);
    }
    return Object.assign(res, tabInfo(await settle(tab_id)));
  },
  async type({ tab_id, ref, text, submit, guard }) {
    const { frameId, local } = await prepare(tab_id, String(ref), submit ? "submit" : "type", guard);
    const res = await inFrame(tab_id, frameId, pageAct, ["type", local, String(text), Boolean(submit), pageGuard(guard)], true);
    if (res.refused) {
      throw new Refused(res.refused);
    }
    return Object.assign(res, tabInfo(await settle(tab_id)));
  },
  async press({ tab_id, ref, key, guard }) {
    const k = parseKey(key);
    const { frameId, local } = await prepare(tab_id, ref ? String(ref) : "", k.activates ? "activate" : "key", guard);
    const res = await inFrame(tab_id, frameId, pageKey, [local, k, pageGuard(guard)], true);
    if (res.refused) {
      throw new Refused(res.refused);
    }
    return Object.assign(res, tabInfo(await settle(tab_id)));
  },
  async hover({ tab_id, ref }) {
    const { frameId, local } = await prepare(tab_id, String(ref), "hover", {});
    const res = await inFrame(tab_id, frameId, pageHover, [local], true);
    await new Promise((r) => setTimeout(r, 300));
    return Object.assign(res, tabInfo(await browser.tabs.get(tab_id)));
  },
  async scroll({ tab_id, ref, frame, direction, pages, to_text }) {
    const tab = await agentTab(tab_id);
    if (direction && !["down", "up", "top", "bottom"].includes(direction)) {
      throw new Refused("direction must be down, up, top or bottom");
    }
    if (!direction && !ref && !to_text) {
      throw new Refused("give a direction, a ref to scroll into view, or to_text");
    }
    let frameId = frameIdOf(tab_id, frame);
    let local = "";
    if (ref) {
      ({ frameId, ref: local } = parseRef(tab_id, ref));
    } else if (to_text) {
      const found = (await inAllFrames(tab_id, pageFind, [String(to_text)], true)).filter((r) => r.result.found);
      if (!found.length) {
        return { found: false, tab_id, url: tab.url, title: tab.title };
      }
      frameId = (found.find((r) => r.frameId === 0) || found[0]).frameId;
    }
    const n = Math.max(0.1, Math.min(Number(pages) || 1, 20));
    const res = await inFrame(tab_id, frameId, pageScroll, [local, direction || "", n, to_text ? String(to_text) : ""], true);
    if (frameId) {
      res.frame = "f" + frameNum(tab_id, frameId);
    }
    return Object.assign(res, { tab_id, url: tab.url, title: tab.title });
  },
  async wait({ tab_id, text, selector, timeout_ms }) {
    const tab = await agentTab(tab_id);
    if (!text && !selector) {
      throw new Refused("give the text or the CSS selector to wait for");
    }
    if (selector && await onLoginPage(tab)) {
      throw new Refused("CSS selector waits are refused on a login page of a host in browser.autologin-hosts; wait for a text");
    }
    const limit = Math.max(0, Math.min(Number(timeout_ms) || 10000, MAX_WAIT_MS));
    const start = Date.now();
    for (;;) {
      const res = await inAllFrames(tab_id, pageHas, [text ? String(text) : "", selector ? String(selector) : "",
        [...autologinHosts]], true);
      const bad = res.find((r) => r.result.error);
      if (bad) {
        throw new Refused(bad.result.error);
      }
      const hit = res.find((r) => r.result.found);
      if (hit) {
        const out = { found: true, waited_ms: Date.now() - start };
        if (hit.frameId) {
          out.frame = "f" + frameNum(tab_id, hit.frameId);
        }
        return out;
      }
      if (Date.now() - start >= limit) {
        return { found: false, waited_ms: Date.now() - start };
      }
      await new Promise((r) => setTimeout(r, 300));
    }
  },
  async eval({ tab_id, frame, script, allow_writes, guard, max_chars }) {
    const tab = await agentTab(tab_id);
    if (typeof script !== "string" || !script.trim()) {
      throw new Refused("script is empty");
    }
    if (tab.openerTabId !== undefined) {
      throw new Refused("this tab was opened from another tab, which a script could reach; open the page in a new " +
        "agent tab with browser_navigate");
    }
    const frameId = frameIdOf(tab_id, frame);
    if (frameId && scripted.has(tab_id)) {
      throw new Refused("a script already ran in this tab, so scripts in its frames wait until browser_navigate loads " +
        "a page in it again");
    }
    // The top frame's address comes from the browser, not from the page's
    // world, which an earlier script could have changed.
    const where = frameId ? await inFrame(tab_id, frameId, () => (/^https?:$/.test(location.protocol) ? location.href : ""))
      : (/^https?:/.test(tab.url || "") ? tab.url : "");
    if (!where) {
      throw new Refused("the frame does not show an http or https page");
    }
    const topHost = hostOf(tab.url);
    const host = hostOf(where);
    const site = siteOf(host);
    if (autologinHost(topHost) || autologinHost(host)) {
      throw new Refused("scripts are never allowed on a host in browser.autologin-hosts (" + (autologinHost(topHost) ? topHost : host) +
        "), where Firefox fills a saved password");
    }
    if (allow_writes && (protectedHost(topHost) || protectedHost(host))) {
      throw new Refused("scripts that change data are never allowed on Salesforce (or the sites in browser.confirm-hosts); " +
        "run it without allow_writes");
    }
    const g = guard || {};
    const writes = (g.write_hosts || []).map(siteOf);
    if (!writes.includes(site) && !(g.script_hosts || []).map(siteOf).includes(site)) {
      throw new Refused("running scripts on " + site + " needs the user's approval");
    }
    if (allow_writes && !writes.includes(site)) {
      throw new Refused("scripts that change data on " + site + " need the user's separate approval");
    }
    const sensitive = await tabSensitive(tab_id);
    if (sensitive.length && allow_writes) {
      throw new Refused("scripts that change data are not allowed on a page with " + sensitive.join(", ") + " fields");
    }
    if (sensitive.length && !g.sensitive) {
      throw new Refused("this page has " + sensitive.join(", ") + " fields; the user must confirm each script here");
    }
    // The limits apply before the script runs and stay until the agent
    // loads another page in the tab (docs/browser.md section 7.2).
    // Writes are allowed only to the site the user approved them for.
    const old = taints.get(tab_id);
    const sites = new Map(old ? old.sites : []);
    if (!sites.has(siteOf(topHost))) {
      sites.set(siteOf(topHost), false);
    }
    sites.set(site, Boolean(allow_writes));
    taints.set(tab_id, { sites });
    scripted.add(tab_id);
    everScripted.add(tab_id);
    const [res] = await browser.tabs.executeScript(tab_id, { code: evalCode(script, site, [...autologinHosts]), frameId });
    if (!res || typeof res !== "object") {
      throw new Error("the script did not finish");
    }
    if (res.refused) {
      throw new Refused(String(res.error));
    }
    if (!res.ok) {
      throw new Error("script error: " + String(res.error).slice(0, 2000));
    }
    const max = Math.max(1, Math.min(Number(max_chars) || MAX_TEXT, MAX_EVAL_RESULT));
    const out = { tab_id, url: tab.url, title: tab.title, read_only: !allow_writes };
    if (frameId) {
      out.frame = "f" + frameNum(tab_id, frameId);
    }
    const json = typeof res.json === "string" ? res.json : "null";
    if (json.length > max) {
      out.result_text = json.slice(0, max);
      out.truncated = true;
      out.result_chars = json.length;
    } else {
      out.result = JSON.parse(json);
    }
    if (res.undef) {
      out.hint = "the script returned nothing; end it with return <value>";
    }
    return out;
  },
  async login({ tab_id, url, guard, timeout_ms }) {
    const approved = new Set(((guard && guard.login_hosts) || []).map((h) => String(h).toLowerCase()));
    if (loginBusy) {
      throw new Refused("another browser_login is running");
    }
    loginBusy = true;
    let before = null;
    let tabId = tab_id;
    try {
      if (url) {
        const target = new URL(checkURL(url));
        if (target.protocol !== "https:" || !autologinHost(target.hostname) || !approved.has(target.hostname.toLowerCase())) {
          throw new Refused("browser_login opens only https pages on an approved host in browser.autologin-hosts; " +
            target.hostname + " is not one");
        }
        tabId = (await OPS.tab_open({ url: target.href, guard: { visit_hosts: [target.hostname] } })).tab_id;
      }
      let tab = await agentTab(tabId);
      if (tab.status === "loading") {
        tab = await waitComplete(tabId);
      }
      const host = hostOf(tab.url);
      if (!/^https:/.test(tab.url || "") || !autologinHost(host)) {
        throw new Refused("browser_login works only on an https page whose host is in this device's " +
          "browser.autologin-hosts; tab " + tabId + " shows " + (host || "no page"));
      }
      if (!approved.has(host)) {
        throw new Refused("browser_login on " + host + " needs the user's approval");
      }
      if (taints.has(tabId) || scripted.has(tabId)) {
        throw new Refused("this tab ran a script; open the login page in a new tab with browser_navigate");
      }
      const hosts = [...autologinHosts];
      const state = await inPage(tabId, pageLogin, [hosts, host, false]);
      if (state.error) {
        throw new Refused(state.error);
      }
      if (state.passwords !== 1 || state.new_password || !state.username_present) {
        const why = !state.passwords ? "no password field" : state.passwords > 1 || state.new_password ?
          "more than one password field, or a new password field" : "no username field next to its password field";
        return { tab_id: tabId, filled: false, submitted: false, landed_host: host,
          reason: "the page has " + why + ", so it is not a simple login form" };
      }
      before = await activate(tab);
      const limit = Math.max(1000, Math.min(Number(timeout_ms) || LOGIN_FILL_MS, MAX_LOGIN_FILL_MS));
      const start = Date.now();
      let now = state;
      for (;;) {
        now = await inPage(tabId, pageLogin, [hosts, host, false]);
        if (now.error) {
          throw new Refused(now.error);
        }
        if (now.passwords === 1 && now.password_filled && now.username_filled) {
          break;
        }
        if (now.passwords !== 1 || Date.now() - start >= limit) {
          const out = { tab_id: tabId, filled: false, submitted: false, landed_host: hostOf((await browser.tabs.get(tabId)).url) };
          out.reason = now.passwords !== 1 ? "the login form changed while waiting for Firefox to fill it"
            : now.visibility !== "visible" ? "the tab did not become visible (document.visibilityState is " +
              now.visibility + "), and Firefox fills saved logins only in a visible tab. Is the window hidden or the " +
              "screen off?" : NOT_FILLED;
          if (now.password_filled && now.username_present && !now.username_filled) {
            out.reason = "Firefox filled the password but not the username field. " + NOT_FILLED;
          }
          return out;
        }
        await new Promise((r) => setTimeout(r, 250));
      }
      // The page's host is checked again in the page right before the
      // submit; the guard keeps the posted form on the autologin hosts.
      loginGuards.set(tabId, { hosts: new Set(hosts), submitting: true });
      const loaded = waitComplete(tabId, true).catch(() => null);
      const sent = await inPage(tabId, pageLogin, [hosts, host, true]);
      if (sent.error || !sent.submitted) {
        loginGuards.delete(tabId);
        return { tab_id: tabId, filled: true, submitted: false, landed_host: host, reason: sent.error || "not submitted" };
      }
      await Promise.race([loaded, new Promise((r) => setTimeout(r, LOAD_TIMEOUT_MS))]);
      const after = await settle(tabId).catch(() => browser.tabs.get(tabId));
      return { tab_id: tabId, filled: true, submitted: true, landed_host: hostOf(after.url) };
    } finally {
      // The redirect rule stays a while for a post that is still pending.
      const g = loginGuards.get(tabId);
      if (g) {
        setTimeout(() => {
          if (loginGuards.get(tabId) === g) {
            loginGuards.delete(tabId);
          }
        }, 2 * LOAD_TIMEOUT_MS);
      }
      if (before) {
        await restore(before);
      }
      loginBusy = false;
    }
  },
  async check({ tab_id }) {
    let tab = null;
    if (tab_id !== undefined && tab_id !== null) {
      tab = await agentTab(tab_id);
    } else {
      tab = (await browser.tabs.query({})).find((t) => inGroup(t) && /^https?:/.test(t.url || "")) || null;
    }
    const win = await browser.windows.getLastFocused();
    const out = { window_state: win.state, window_focused: win.focused, autologin_hosts: [...autologinHosts].sort() };
    if (!tab) {
      out.note = "no agent tab with a web page to check; open one with browser_navigate";
      return out;
    }
    out.tab_id = tab.id;
    out.tab_active_before = tab.active;
    out.before = await inPage(tab.id, pageVisibility);
    if (loginBusy) {
      out.note = "browser_login is running, so the tab was not activated";
      return out;
    }
    if (!autologinHost(hostOf(tab.url)) && (await tabSensitive(tab.id)).includes("password")) {
      out.note = "the tab has a password field and is not on a host in browser.autologin-hosts, so it was not " +
        "activated (Firefox would fill a saved login there)";
      return out;
    }
    loginBusy = true;
    const before = await activate(tab);
    try {
      const start = Date.now();
      let seen = await inPage(tab.id, pageVisibility);
      while (seen.visibility !== "visible" && Date.now() - start < 3000) {
        await new Promise((r) => setTimeout(r, 200));
        seen = await inPage(tab.id, pageVisibility);
      }
      out.after_activate = seen;
      out.waited_ms = Date.now() - start;
      const w = await browser.windows.get(tab.windowId);
      out.tab_window_state = w.state;
      out.tab_window_focused = w.focused;
    } finally {
      out.restored = await restore(before);
      loginBusy = false;
    }
    out.would_fill = out.after_activate.visibility === "visible";
    return out;
  },
  async screenshot({ tab_id, format, quality, full_page }) {
    const tab = await agentTab(tab_id);
    if (await onLoginPage(tab)) {
      throw new Refused("screenshots are refused on a login page of a host in browser.autologin-hosts, since they " +
        "would show how long a filled password is");
    }
    const opts = format === "jpeg" ? { format: "jpeg", quality: Math.max(10, Math.min(Number(quality) || 80, 100)) } : { format: "png" };
    const out = { tab_id, url: tab.url, title: tab.title };
    if (full_page) {
      const d = await inPage(tab_id, () => {
        const e = document.scrollingElement || document.documentElement;
        return { w: Math.max(e.scrollWidth, window.innerWidth), h: Math.max(e.scrollHeight, window.innerHeight) };
      });
      const w = Math.min(d.w, 4096);
      const h = Math.min(d.h, MAX_SHOT_HEIGHT);
      opts.rect = { x: 0, y: 0, width: w, height: h };
      opts.scale = Math.min(1, Math.sqrt(MAX_SHOT_PIXELS / (w * h)));
      Object.assign(out, { page_height: d.h, clipped: d.h > h, scale: Math.round(opts.scale * 1000) / 1000 });
    }
    out.data_url = await browser.tabs.captureTab(tab_id, opts);
    if (await onLoginPage(await browser.tabs.get(tab_id))) {
      throw new Refused("the tab loaded a login page of a host in browser.autologin-hosts during the screenshot, so it " +
        "was dropped");
    }
    return out;
  },
};

connect();

"use strict";

// Hermes browser control. The only way in is the native messaging port to
// the hermes-device host: there are no content scripts that listen to the
// page, no external messaging and no web accessible resources. The
// extension's own pages (the popup and the options page) talk to this page
// with runtime messages to sign in and out.

const HOST = "hermes_device";
const GROUP_TITLE = "Hermes";
const RETRY_MS = 5000;
const LOAD_TIMEOUT_MS = 30000;
const MAX_TEXT = 100000;

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

// The daemon's config: its hub.url (a hint) and which device it is.
let daemon = {};
// What the daemon and the hub say about the stored credential.
let daemonAuth = { state: "offline" };
// stored is browser.storage.local: hub_url (the user's choice), instance_id
// (this browser's random id) and auth (token, hub, user, device_id,
// device_name, signed_in_at).
let stored = {};
let signingIn = false;
let lastError = "";

const NOT_SIGNED_IN = "Firefox is not signed in to Hermes";

// Refused marks an error the extension chose: the device reports it with
// code "refused" instead of "failed".
class Refused extends Error {}

function setStatus(text, badge) {
  browser.browserAction.setTitle({ title: text.startsWith("Sign in") ? text : "Hermes: " + text });
  browser.browserAction.setBadgeText({ text: badge });
}

function signedIn() {
  return Boolean(port && stored.auth && stored.auth.token && daemonAuth.state === "signed_in");
}

// showStatus sets the toolbar button from the connection and the sign-in.
function showStatus() {
  if (inflight > 0) {
    return;
  }
  if (!port) {
    setStatus(replaced ? "another Firefox took over; open this menu to use this one" : "not connected", "");
  } else if (!stored.auth || !stored.auth.token) {
    setStatus("Sign in to Hermes", "!");
  } else if (daemonAuth.state === "signed_in") {
    setStatus("connected as " + daemonAuth.user, "on");
  } else if (daemonAuth.state === "refused") {
    setStatus(daemonAuth.reason || NOT_SIGNED_IN, "!");
  } else if (daemonAuth.state === "offline") {
    setStatus(daemonAuth.reason || "hermes-device is not connected to the hub", "");
  } else {
    setStatus("checking the sign-in", "");
  }
}

function hubURL() {
  return stored.hub_url || DEFAULT_HUB_URL || "";
}

async function save(changes) {
  Object.assign(stored, changes);
  await browser.storage.local.set(changes);
}

function sendAuth() {
  if (port) {
    port.postMessage({ type: "auth", token: (stored.auth && stored.auth.token) || "" });
  }
}

function b64url(bytes) {
  let s = "";
  for (const b of new Uint8Array(bytes)) {
    s += String.fromCharCode(b);
  }
  return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function randomB64(n) {
  return b64url(crypto.getRandomValues(new Uint8Array(n)));
}

async function hubFetch(hub, path, init) {
  const ctl = new AbortController();
  const timer = setTimeout(() => ctl.abort(), 15000);
  try {
    return await fetch(hub + path, Object.assign({ credentials: "omit", redirect: "error", cache: "no-store", signal: ctl.signal }, init));
  } finally {
    clearTimeout(timer);
  }
}

// signIn runs the hub's sign-in flow: browser.identity opens the hub's
// page, where the user signs in and approves this browser for this
// computer, and the hub redirects to the extension's identity URL with a
// one-time code. The code and the PKCE verifier buy the credential.
async function signIn() {
  const hub = hubURL();
  if (!hub) {
    throw new Error("Set the address of your Hermes hub first.");
  }
  if (!port) {
    throw new Error("hermes-device is not running on this computer, so Firefox cannot sign in for it.");
  }
  if (!daemon.device_id) {
    throw new Error("hermes-device has not connected to its hub yet. Check that hermes-device run is running.");
  }
  let instance = stored.instance_id;
  if (!instance) {
    instance = randomB64(18);
    await save({ instance_id: instance });
  }
  const deviceID = daemon.device_id;
  const verifier = randomB64(32);
  const challenge = b64url(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(verifier)));
  const state = randomB64(16);
  const redirect = browser.identity.getRedirectURL();
  const info = await browser.runtime.getBrowserInfo();
  const params = new URLSearchParams({
    response_type: "code", client_id: browser.runtime.id, redirect_uri: redirect, state,
    code_challenge: challenge, code_challenge_method: "S256", device_id: deviceID, instance,
    browser: info.name + " " + info.version,
  });
  let back;
  try {
    back = new URL(await browser.identity.launchWebAuthFlow({ url: hub + "/browser/authorize?" + params, interactive: true }));
  } catch (e) {
    throw new Error("Sign-in did not finish: " + (e && e.message ? e.message : e));
  }
  if (back.origin + back.pathname !== redirect || back.searchParams.get("state") !== state) {
    throw new Error("The hub's answer did not match this sign-in. Try again.");
  }
  if (back.searchParams.get("error")) {
    throw new Error("Firefox was not allowed to use Hermes.");
  }
  const resp = await hubFetch(hub, "/api/browser/token", {
    method: "POST", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ grant_type: "authorization_code", code: back.searchParams.get("code") || "", code_verifier: verifier,
      redirect_uri: redirect, device_id: deviceID, instance }),
  });
  const body = await resp.json().catch(() => ({}));
  if (!resp.ok || typeof body.token !== "string" || !body.token.startsWith("hbt_")) {
    throw new Error(body.message || "The hub did not accept the sign-in (HTTP " + resp.status + ").");
  }
  const old = stored.auth;
  await save({ auth: { token: body.token, hub, user: body.user, device_id: body.device_id, device_name: body.device_name,
    signed_in_at: Date.now() } });
  if (old && old.token && old.token !== body.token) {
    revoke(old).catch(() => {});
  }
  daemonAuth = { state: "checking" };
  sendAuth();
  showStatus();
}

async function revoke(auth) {
  const resp = await hubFetch(auth.hub, "/api/browser/revoke", { method: "POST", headers: { Authorization: "Bearer " + auth.token } });
  if (!resp.ok) {
    throw new Error("HTTP " + resp.status);
  }
}

// signOut forgets the credential here and revokes it at the hub that
// issued it.
async function signOut() {
  const old = stored.auth;
  await save({ auth: {} });
  sendAuth();
  showStatus();
  if (old && old.token) {
    try {
      await revoke(old);
    } catch (e) {
      throw new Error("Signed out here, but " + old.hub + " could not be reached to end the sign-in there (" + e.message +
        "). Remove this browser under Settings on the hub.");
    }
  }
}

async function setHubURL(text) {
  const url = text ? normalizeHubURL(text) : "";
  if (url === (stored.hub_url || "")) {
    return;
  }
  const auth = stored.auth;
  await save({ hub_url: url });
  if (auth && auth.token && auth.hub !== hubURL()) {
    await signOut();
  }
}

function publicState() {
  const a = stored.auth || {};
  return {
    connected: Boolean(port), replaced, signed_in: Boolean(a.token), signing_in: signingIn, error: lastError,
    hub_url: hubURL(), default_hub_url: DEFAULT_HUB_URL, daemon_hub_url: daemon.hub_url || "",
    device_id: daemon.device_id || "", device_name: daemon.device_name || "",
    user: a.user || "", token_hub: a.hub || "", token_device: a.device_name || "", signed_in_at: a.signed_in_at || 0,
    auth_state: daemonAuth.state, auth_user: daemonAuth.user || "", auth_reason: daemonAuth.reason || "",
  };
}

async function runCommand(cmd, arg) {
  switch (cmd) {
    case "state":
      return publicState();
    case "connect":
      if (!port) {
        replaced = false;
        connect();
      }
      return publicState();
    case "signIn":
      if (signingIn) {
        throw new Error("A sign-in is already open.");
      }
      signingIn = true;
      try {
        await signIn();
      } finally {
        signingIn = false;
      }
      return publicState();
    case "signOut":
      await signOut();
      return publicState();
    case "setHubURL":
      await setHubURL(arg);
      return publicState();
  }
  throw new Error("unknown command");
}

// Only the extension's own pages may sign in or out; page code injected
// into tabs has the tab's URL as its sender.
browser.runtime.onMessage.addListener((msg, sender) => {
  if (sender.id !== browser.runtime.id || !String(sender.url || "").startsWith(browser.runtime.getURL("")) || !msg) {
    return undefined;
  }
  return runCommand(msg.cmd, msg.arg).then(
    (state) => {
      if (msg.cmd !== "state") {
        lastError = "";
      }
      return { ok: true, state: Object.assign(state, { error: lastError }) };
    },
    (e) => {
      lastError = String(e && e.message ? e.message : e);
      return { ok: false, error: lastError, state: publicState() };
    },
  );
});

async function connect() {
  const p = browser.runtime.connectNative(HOST);
  port = p;
  p.onMessage.addListener(onMessage);
  p.onDisconnect.addListener(() => {
    port = null;
    daemonAuth = { state: "offline" };
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
    sendAuth();
    showStatus();
  } catch (e) {
    // The port went away before the hello; onDisconnect retries.
  }
}

async function onMessage(msg) {
  if (msg && msg.type === "config") {
    allowHosts = new Set((msg.allow_hosts || []).map((h) => h.toLowerCase()));
    daemon = { hub_url: msg.hub_url || "", device_id: msg.device_id || "", device_name: msg.device_name || "" };
    return;
  }
  if (msg && msg.type === "auth_status") {
    daemonAuth = { state: msg.state || "offline", user: msg.user || "", reason: msg.reason || "" };
    showStatus();
    return;
  }
  if (msg && (msg.type === "sign_in" || msg.type === "sign_out")) {
    runCommand(msg.type === "sign_in" ? "signIn" : "signOut").then(
      () => {
        lastError = "";
      },
      (e) => {
        lastError = String(e && e.message ? e.message : e);
      },
    );
    return;
  }
  if (msg && msg.type === "replaced") {
    replaced = true;
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
    if (!signedIn()) {
      throw new Refused(NOT_SIGNED_IN);
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


browser.tabs.onRemoved.addListener((tabId) => agentTabs.delete(tabId));

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
// Hermes tab group, so a tab the user drags out of the group is no longer
// the agent's. Without the tab groups API, the tabs the agent opened.
function inGroup(tab) {
  return groupId !== null ? tab.groupId === groupId : agentTabs.has(tab.id);
}

// agentTab returns the tab if the agent may act on it. Unless anyURL is
// set, the tab must also show an http or https page, so a tab that went
// back or redirected to file: or about: cannot be read.
async function agentTab(tabId, anyURL) {
  const tab = await browser.tabs.get(tabId);
  if (!inGroup(tab)) {
    throw new Refused("tab " + tabId + " is not in the Hermes tab group");
  }
  if (!anyURL && !/^https?:/.test(tab.url || "")) {
    throw new Refused("tab " + tabId + " shows " + String(tab.url).split(":")[0] + ": content, which the agent may not use");
  }
  return tab;
}

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

// hermesHost reports whether host (with its port) is the hub this browser
// signs in to or Firefox's sign-in redirect host. Agent tabs may not open
// them, so the agent cannot approve a sign-in for itself.
function hermesHost(host) {
  const h = String(host || "").toLowerCase().replace(/\.$/, "");
  if (h.endsWith(".extensions.allizom.org")) {
    return true;
  }
  for (const hub of [hubURL(), stored.auth && stored.auth.hub]) {
    try {
      if (hub && new URL(hub).host.toLowerCase() === h) {
        return true;
      }
    } catch (e) {
      // An unusable address names no host.
    }
  }
  return false;
}

function checkURL(url) {
  const u = new URL(url);
  if (u.protocol !== "https:" && u.protocol !== "http:") {
    throw new Refused("only http and https URLs are allowed, got " + u.protocol);
  }
  if (hermesHost(u.host)) {
    throw new Refused("the agent may not open the Hermes hub or its sign-in pages in its tabs");
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
// points at a private address is caught too.
browser.webRequest.onBeforeRequest.addListener(
  async (details) => {
    if (details.tabId < 0) {
      return {};
    }
    let tab;
    try {
      tab = await browser.tabs.get(details.tabId);
    } catch (e) {
      return {};
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

async function inPage(tabId, func, args, withHelpers) {
  if (withHelpers) {
    await browser.scripting.executeScript({ target: { tabId }, func: installHelpers });
  }
  const [res] = await browser.scripting.executeScript({ target: { tabId }, func, args: args || [] });
  if (res.error) {
    throw new Error(String(res.error));
  }
  return res.result;
}

function tabInfo(tab) {
  return { tab_id: tab.id, url: tab.url, title: tab.title, active: tab.active, group_id: tab.groupId, agent: inGroup(tab) };
}

// Page functions run in the extension's isolated world in the page, so the
// page cannot see or change the element refs.

function pageText(max) {
  const text = document.body ? document.body.innerText : "";
  return { title: document.title, url: location.href, text: text.slice(0, max), truncated: text.length > max };
}

function pageSnapshot(interactiveOnly) {
  const state = (globalThis.__hermes = { refs: new Map(), next: 1 });
  const implicit = {
    A: "link", BUTTON: "button", SELECT: "combobox", TEXTAREA: "textbox", H1: "heading", H2: "heading",
    H3: "heading", H4: "heading", H5: "heading", H6: "heading", IMG: "img", SUMMARY: "button", P: "paragraph",
    LI: "listitem", NAV: "navigation", MAIN: "main", FORM: "form", TABLE: "table", DIALOG: "dialog",
  };
  const inputRoles = { checkbox: "checkbox", radio: "radio", submit: "button", button: "button", reset: "button", range: "slider", search: "searchbox" };
  const known = new Set(["link", "button", "textbox", "searchbox", "combobox", "checkbox", "radio", "slider", "tab",
    "menuitem", "option", "switch", "heading", "img", "paragraph", "listitem", "navigation", "main", "form", "table",
    "dialog", "listbox", "menu", "tablist", "tabpanel", "alert", "banner", "contentinfo", "region", "search"]);
  const role = (el) => {
    const r = (el.getAttribute("role") || "").trim().toLowerCase();
    if (r) {
      return known.has(r) ? r : null;
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
      const t = by.split(/\s+/).map((id) => document.getElementById(id)).filter(Boolean).map((e) => e.textContent).join(" ");
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
  const interactive = new Set(["link", "button", "textbox", "searchbox", "combobox", "checkbox", "radio", "slider", "tab", "menuitem", "option", "switch"]);
  const lines = [];
  const walk = (node, depth) => {
    for (const el of node.children) {
      const r = role(el);
      let next = depth;
      if (r && visible(el) && (!interactiveOnly || interactive.has(r))) {
        const ref = "e" + state.next++;
        state.refs.set(ref, new WeakRef(el));
        let line = "  ".repeat(depth) + "- " + r;
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
        if ((el.tagName === "INPUT" || el.tagName === "TEXTAREA") && !secret && el.value) {
          line += " value=" + JSON.stringify(clean(el.value));
        }
        if (el.type === "checkbox" || el.type === "radio") {
          line += el.checked ? " [checked]" : "";
        }
        lines.push(line + " [ref=" + ref + "]");
        next = depth + 1;
      }
      walk(el, next);
    }
  };
  if (document.body) {
    walk(document.body, 0);
  }
  return { title: document.title, url: location.href, snapshot: lines.join("\n"), element_count: state.next - 1 };
}

// installHelpers defines the classification helpers in the page's isolated
// world, where pageInspect and pageAct find them. executeScript sends one
// function's source, so the helpers travel inside this one.
function installHelpers() {
  // pageClassify describes the page and, with a ref, its element, so the
  // plugin can decide what to ask the user (docs/browser.md section 7).
  // sensitive lists the kinds of secret fields on the page: password,
  // payment and one_time_code.
  function pageClassify(ref) {
    const host = (u) => {
      try {
        return new URL(u, location.href).hostname.toLowerCase();
      } catch (e) {
        return "";
      }
    };
    const payHosts = /(^|\.)(stripe\.com|stripe\.network|braintreegateway\.com|braintree-api\.com|adyen\.com|paypal\.com|checkout\.com|squareup\.com|klarna\.com|recurly\.com)$/;
    const sensitive = new Set();
    for (const el of document.querySelectorAll("input, textarea, select")) {
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
    for (const f of document.querySelectorAll("iframe[src]")) {
      if (payHosts.test(host(f.src))) {
        sensitive.add("payment");
      }
    }
    const out = { url: location.href, host: location.hostname.toLowerCase(), sensitive: [...sensitive].sort() };
    if (!ref) {
      return out;
    }
    const state = globalThis.__hermes;
    const entry = state && state.refs.get(String(ref).replace(/^@/, ""));
    const el = entry && entry.deref();
    if (!el || !el.isConnected) {
      return Object.assign(out, { error: "unknown or stale ref " + ref + "; take a new snapshot" });
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
      target = { kind: "field", host: host(form ? form.action || location.href : location.href) };
      if (el.type === "password") {
        target.field = "password";
      }
    }
    return Object.assign(out, { target });
  }

  // guardCheck refuses an action the plugin did not get approved: a submit,
  // a link to another host, or any action on a page with secret fields. It
  // runs in the page right before the action, so a page that changed since
  // the plugin asked cannot slip an action past the user.
  function guardCheck(info, action, submit, guard) {
    const g = guard || {};
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
  globalThis.__hermesHelpers = { pageClassify, guardCheck };
}

function pageInspect(ref) {
  return globalThis.__hermesHelpers.pageClassify(ref);
}

function pageAct(action, ref, text, submit, guard) {
  const { pageClassify, guardCheck } = globalThis.__hermesHelpers;
  const info = pageClassify(ref);
  if (info.error) {
    throw new Error(info.error);
  }
  const why = guardCheck(info, action, submit, guard);
  if (why) {
    return { refused: why };
  }
  const state = globalThis.__hermes;
  const el = state.refs.get(ref.replace(/^@/, "")).deref();
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
    const loaded = waitComplete(tab_id, true);
    await browser.tabs.update(tab_id, { url: target });
    return tabInfo(await loaded);
  },
  async back({ tab_id }) {
    await agentTab(tab_id);
    const loaded = waitComplete(tab_id, true);
    await browser.tabs.goBack(tab_id);
    return tabInfo(await loaded);
  },
  async read_text({ tab_id, max_chars }) {
    await agentTab(tab_id);
    return inPage(tab_id, pageText, [Math.min(max_chars || MAX_TEXT, MAX_TEXT)]);
  },
  async snapshot({ tab_id, interactive_only }) {
    await agentTab(tab_id);
    return inPage(tab_id, pageSnapshot, [Boolean(interactive_only)]);
  },
  async inspect({ tab_id, ref, url }) {
    const out = {};
    if (url) {
      out.url_host = new URL(checkURL(url)).hostname.toLowerCase();
    }
    if (tab_id === undefined || tab_id === null) {
      return out;
    }
    await agentTab(tab_id);
    return Object.assign(await inPage(tab_id, pageInspect, [ref ? String(ref) : ""], true), out);
  },
  async click({ tab_id, ref, guard }) {
    await agentTab(tab_id);
    const res = await inPage(tab_id, pageAct, ["click", String(ref), "", false, guard || {}], true);
    if (res.refused) {
      throw new Refused(res.refused);
    }
    return Object.assign(res, tabInfo(await settle(tab_id)));
  },
  async type({ tab_id, ref, text, submit, guard }) {
    await agentTab(tab_id);
    const res = await inPage(tab_id, pageAct, ["type", String(ref), String(text), Boolean(submit), guard || {}], true);
    if (res.refused) {
      throw new Refused(res.refused);
    }
    return Object.assign(res, tabInfo(await settle(tab_id)));
  },
  async screenshot({ tab_id, format, quality }) {
    const tab = await agentTab(tab_id);
    const opts = format === "jpeg" ? { format: "jpeg", quality: Math.max(10, Math.min(Number(quality) || 80, 100)) } : { format: "png" };
    const data_url = await browser.tabs.captureTab(tab_id, opts);
    return { tab_id, url: tab.url, title: tab.title, data_url };
  },
};

browser.storage.local.get(null).then(
  (data) => {
    stored = data || {};
    connect();
  },
  () => connect(),
);

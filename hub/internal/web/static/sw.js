"use strict";
// The hub's service worker. It keeps one page for when the phone is
// offline and shows Web Push notifications. It never caches the hub's
// pages, API answers or streams: those carry the person's chats and need
// their login.
const CACHE = "hub-offline-3";
const OFFLINE = "/static/offline.html";
const ICON = "/static/icon-192.png";
const BADGE = "/static/badge-96.png";

self.addEventListener("install", (event) => {
  event.waitUntil(caches.open(CACHE).then((c) => c.addAll([OFFLINE, "/static/offline.css", ICON])).then(() => self.skipWaiting()));
});

self.addEventListener("activate", (event) => {
  event.waitUntil(caches.keys()
    .then((keys) => Promise.all(keys.filter((k) => k !== CACHE && !k.startsWith("hub-static-")).map((k) => caches.delete(k))))
    .then(() => self.clients.claim()));
});

// A page that found a new release asks for its files before it reloads
// (update.js). The worker fetches the page and the static files it names
// into a cache of their own, and serves them from there for the next
// minute only, so the reload needs no network and nothing stays stale.
let warm = null;

async function prefetch() {
  const name = "hub-static-" + Date.now();
  const c = await caches.open(name);
  const page = await fetch("/", { cache: "no-store", credentials: "same-origin" });
  if (!page.ok) return;
  const html = await page.clone().text();
  await c.put("/", page);
  const urls = [...new Set([...html.matchAll(/(?:src|href)="(\/static\/[^"?#]+)(?:\?v=[0-9a-f]+)?"/g)].map((m) => m[1]))];
  await Promise.all(urls.map((u) => fetch(u, { cache: "no-store", credentials: "same-origin" }).then((r) => (r.ok ? c.put(u, r) : null))));
  for (const k of await caches.keys()) if (k.startsWith("hub-static-") && k !== name) await caches.delete(k);
  warm = { name, until: Date.now() + 60000 };
}

self.addEventListener("message", (event) => {
  if (!event.data || event.data.type !== "prefetch") return;
  const port = event.ports && event.ports[0];
  event.waitUntil(prefetch().catch(() => {}).then(() => { if (port) port.postMessage("ready"); }));
});

function fromWarm(req) {
  if (!warm || Date.now() > warm.until || req.method !== "GET") return null;
  const url = new URL(req.url);
  if (url.origin !== self.location.origin || (url.search && !/^\?v=[0-9a-f]+$/.test(url.search)) || (url.pathname !== "/" && !url.pathname.startsWith("/static/"))) return null;
  const name = warm.name;
  return caches.open(name).then((c) => c.match(url.pathname)).then((r) => r || fetch(req));
}

self.addEventListener("fetch", (event) => {
  const req = event.request;
  const cached = fromWarm(req);
  if (cached) {
    event.respondWith(cached.catch(() => fetch(req)));
    return;
  }
  if (req.mode !== "navigate" || req.method !== "GET") return;
  event.respondWith(fetch(req).catch(async () => (await caches.match(OFFLINE)) || Response.error()));
});

// sameOrigin keeps a notification's link inside the hub.
function sameOrigin(raw) {
  try {
    const u = new URL(raw || "/", self.location.origin);
    return u.origin === self.location.origin ? u.pathname + u.search + u.hash : "/";
  } catch (_) {
    return "/";
  }
}

self.addEventListener("push", (event) => {
  let p = {};
  try { p = event.data ? event.data.json() : {}; } catch (_) { p = {}; }
  const title = typeof p.title === "string" && p.title ? p.title.slice(0, 80) : "Talaria";
  const body = typeof p.body === "string" ? p.body.slice(0, 200) : "";
  event.waitUntil(self.registration.showNotification(title, {
    body, icon: ICON, badge: BADGE, tag: typeof p.tag === "string" ? p.tag.slice(0, 64) : undefined,
    renotify: p.kind === "approval" || p.kind === "question",
    data: { url: sameOrigin(p.url) },
  }));
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const url = sameOrigin(event.notification.data && event.notification.data.url);
  event.waitUntil((async () => {
    const wins = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
    for (const w of wins) {
      if (new URL(w.url).origin !== self.location.origin || w.frameType === "nested") continue;
      await w.focus();
      w.postMessage({ type: "hub.open", url });
      return;
    }
    await self.clients.openWindow(url);
  })());
});

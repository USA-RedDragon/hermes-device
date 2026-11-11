"use strict";
// The hub's service worker. It keeps one page for when the phone is
// offline and shows Web Push notifications. It never caches the hub's
// pages, API answers or streams: those carry the person's chats and need
// their login.
const CACHE = "hub-offline-2";
const OFFLINE = "/static/offline.html";
const ICON = "/static/icon-192.png";
const BADGE = "/static/badge-96.png";

self.addEventListener("install", (event) => {
  event.waitUntil(caches.open(CACHE).then((c) => c.addAll([OFFLINE, "/static/offline.css", ICON])).then(() => self.skipWaiting()));
});

self.addEventListener("activate", (event) => {
  event.waitUntil(caches.keys()
    .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
    .then(() => self.clients.claim()));
});

self.addEventListener("fetch", (event) => {
  const req = event.request;
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
      if (new URL(w.url).origin !== self.location.origin) continue;
      await w.focus();
      w.postMessage({ type: "hub.open", url });
      return;
    }
    await self.clients.openWindow(url);
  })());
});

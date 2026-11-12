// tmag-client's service worker: show an alert, and open the session it names.
// It caches nothing. It keeps notification tags, routes and times of recent
// alerts, and the route a tap names, in IndexedDB for minutes, where a page
// can open one it missed.
"use strict";

// A new worker takes over at once: nothing is cached, and iOS rarely closes
// every window the old worker controls.
self.addEventListener("install", event => {
  event.waitUntil(self.skipWaiting());
});

self.addEventListener("activate", event => {
  event.waitUntil(clients.claim());
});

self.addEventListener("push", event => {
  let data = {};
  try { data = event.data ? event.data.json() : {}; } catch { /* a payload without JSON */ }
  const title = data.title || "tmag";
  const options = {
    body: data.body || "",
    tag: data.tag || data.session || "tmag",
    data: { url: data.url || "/" },
    icon: "/icon.png",
    badge: "/icon.png",
    renotify: true,
  };
  const route = alertRoute(options.data);
  event.waitUntil((async () => {
    try { await rememberAlert({ tag: options.tag, route, at: Date.now() }); } catch { /* still show it */ }
    await self.registration.showNotification(title, options);
  })());
});

self.addEventListener("notificationclick", event => {
  event.notification.close();
  const route = alertRoute(event.notification.data);
  event.waitUntil((async () => {
    try { await rememberRoute(route); } catch { /* the route still reaches the page */ }
    const open = await clients.matchAll({ type: "window", includeUncontrolled: true });
    for (const client of open) {
      if (new URL(client.url).origin !== self.location.origin) continue;
      client.postMessage({ type: "tmag-route", route });
      try { await client.focus(); } catch { /* the route is already posted */ }
      return;
    }
    await clients.openWindow("/?open=" + encodeURIComponent(route));
  })());
});

// A route is the inbox, one session, or one request; anything else opens the
// inbox.
const ROUTE = /^#\/(?:s\/[^/?#]+(?:\/r\/[^/?#]+)?)?$/;

// alertRoute is the page route a notification names, in the form the page
// knows.
function alertRoute(data) {
  let route = (data && data.url) || "/";
  try {
    const url = new URL(route, self.location.origin);
    route = url.origin === self.location.origin ? (url.hash || "#/") : "#/";
  } catch {
    route = "#/";
  }
  if (!route.startsWith("#")) route = "#" + route;
  return ROUTE.test(route) ? route : "#/";
}

const ROUTE_DB = "tmag-route";
const ROUTE_STORE = "route";
const ROUTE_KEY = "tapped";
const ALERTS_STORE = "alerts";
const ALERT_LIMIT = 20;
const ALERT_TTL = 10 * 60 * 1000;
const DB_VERSION = 2;

// routeDB opens the database that holds the tapped route and recent alerts.
function routeDB() {
  return new Promise((resolve, reject) => {
    const request = indexedDB.open(ROUTE_DB, DB_VERSION);
    request.onupgradeneeded = () => {
      const db = request.result;
      if (!db.objectStoreNames.contains(ROUTE_STORE)) db.createObjectStore(ROUTE_STORE);
      if (!db.objectStoreNames.contains(ALERTS_STORE)) db.createObjectStore(ALERTS_STORE, { keyPath: "tag" });
    };
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error);
  });
}

// rememberRoute keeps the tapped route for the page, which may reload before
// it can receive the message.
function rememberRoute(route) {
  return routeDB().then(db => new Promise((resolve, reject) => {
    const tx = db.transaction(ROUTE_STORE, "readwrite");
    tx.objectStore(ROUTE_STORE).put({ route, at: Date.now() }, ROUTE_KEY);
    tx.oncomplete = () => { db.close(); resolve(); };
    tx.onerror = () => { db.close(); reject(tx.error); };
    tx.onabort = () => { db.close(); reject(tx.error); };
  }));
}

// rememberAlert keeps a notification's tag, route and time, for the page to
// open if iOS never fires a click. It keeps the newest 20 recent alerts.
function rememberAlert(alert) {
  return routeDB().then(db => new Promise((resolve, reject) => {
    const tx = db.transaction(ALERTS_STORE, "readwrite");
    const store = tx.objectStore(ALERTS_STORE);
    store.put(alert);
    const all = store.getAll();
    all.onsuccess = () => {
      const now = Date.now();
      const recent = (all.result || [])
        .filter(a => a && typeof a.tag === "string" && typeof a.at === "number" && now - a.at < ALERT_TTL)
        .sort((a, b) => b.at - a.at);
      const keep = new Set(recent.slice(0, ALERT_LIMIT).map(a => a.tag));
      for (const record of all.result || []) {
        if (!record || typeof record.tag !== "string" || !keep.has(record.tag)) store.delete(record.tag);
      }
    };
    tx.oncomplete = () => { db.close(); resolve(); };
    tx.onerror = () => { db.close(); reject(tx.error); };
    tx.onabort = () => { db.close(); reject(tx.error); };
  }));
}

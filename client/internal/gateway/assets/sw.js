// tmag-client's service worker: show an alert, and open the session it names.
// It caches nothing. It keeps only the route a tap names, for a moment, in
// IndexedDB, where a page can find it after a reload.
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
  event.waitUntil(self.registration.showNotification(title, options));
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

// routeDB opens the one database and store that hold the tapped route.
function routeDB() {
  return new Promise((resolve, reject) => {
    const request = indexedDB.open(ROUTE_DB, 1);
    request.onupgradeneeded = () => request.result.createObjectStore(ROUTE_STORE);
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

// tmag-client's service worker shows the alerts the gateway pushes and opens
// the session one names when it is tapped. It caches nothing and keeps
// nothing.
"use strict";

self.addEventListener("install", event => event.waitUntil(self.skipWaiting()));
self.addEventListener("activate", event => event.waitUntil(self.clients.claim()));

self.addEventListener("push", event => {
  let n = {};
  try { n = event.data ? event.data.json() : {}; } catch { /* not JSON: a bare notification */ }
  event.waitUntil(self.registration.showNotification(n.title || "tmag", {
    body: n.body || "",
    tag: n.tag || "tmag",
    renotify: true,
    icon: "/icon-180.png",
    badge: "/icon-180.png",
    data: { url: route(n.url), key: typeof n.key === "string" ? n.key : "" },
  }));
});

// A tap opens the session: in a window of this page if one is open, which
// gets the route as a message because WebKit does not reliably navigate a
// client, or in a new one. It never answers anything.
self.addEventListener("notificationclick", event => {
  event.notification.close();
  const url = route(event.notification.data && event.notification.data.url);
  event.waitUntil((async () => {
    const open = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
    const page = open.find(c => new URL(c.url).origin === self.location.origin);
    if (page) {
      page.postMessage({ type: "open", url });
      try { await page.focus(); } catch { /* the message still routes it */ }
      return;
    }
    await self.clients.openWindow(url);
  })());
});

// route is a URL of this page's own routes, or the inbox.
function route(url) {
  if (typeof url !== "string" || !/^\/#\/(s\/[^#?]+)?$/.test(url)) return "/#/";
  return url;
}

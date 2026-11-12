// tmag-client's service worker: show an alert, and open the session it names.
// It caches nothing.
"use strict";

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
    const open = await clients.matchAll({ type: "window", includeUncontrolled: true });
    for (const client of open) {
      if (new URL(client.url).origin !== self.location.origin) continue;
      await client.focus();
      client.postMessage({ type: "tmag-route", route });
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

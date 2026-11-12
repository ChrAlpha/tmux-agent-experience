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
  const route = new URL((event.notification.data && event.notification.data.url) || "/", self.location.origin).href;
  event.waitUntil((async () => {
    const open = await clients.matchAll({ type: "window", includeUncontrolled: true });
    for (const client of open) {
      if (new URL(client.url).origin !== self.location.origin) continue;
      await client.focus();
      if ("navigate" in client) await client.navigate(route);
      return;
    }
    await clients.openWindow(route);
  })());
});

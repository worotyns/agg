// agg service worker: shows alert notifications sent with Web Push.
self.addEventListener('push', (event) => {
  let msg = { title: 'agg', body: '' };
  try { msg = event.data.json(); } catch (e) { if (event.data) msg.body = event.data.text(); }
  event.waitUntil(self.registration.showNotification(msg.title, {
    body: msg.body,
    tag: msg.tag,
    data: { url: msg.url || '/' },
    icon: '/ui/icon.svg',
    badge: '/ui/icon.svg',
  }));
});

self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const url = (event.notification.data && event.notification.data.url) || '/';
  event.waitUntil(self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then((list) => {
    for (const c of list) {
      if (new URL(c.url).origin === self.location.origin && 'focus' in c) { c.navigate(url); return c.focus(); }
    }
    return self.clients.openWindow(url);
  }));
});

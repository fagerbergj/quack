// Web push only: no fetch handler and no cache, so a deploy is never held back by this worker.
self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', (e) => e.waitUntil(self.clients.claim()));

self.addEventListener('push', (e) => {
  let note = {};
  try {
    note = e.data ? e.data.json() : {};
  } catch {
    // A payload that is not JSON still gets a notification, just without words.
  }
  e.waitUntil(self.registration.showNotification(note.title || 'quack', { body: note.body || '', icon: 'icon-192.png', data: { url: note.url || '' } }));
});

// The payload's url is a hash relative to the app, so it works under any path prefix.
self.addEventListener('notificationclick', (e) => {
  e.notification.close();
  const target = new URL(e.notification.data?.url || '', self.registration.scope).href;
  e.waitUntil((async () => {
    const [open] = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
    try {
      if (!open) throw new Error('no window');
      await open.focus();
      await open.navigate(target);
    } catch {
      await self.clients.openWindow(target);
    }
  })());
});

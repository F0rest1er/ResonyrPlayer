const shellCache = 'resonyr-shell-v27';
const shellFiles = ['/', '/style.css', '/offline.js', '/app.js', '/manifest.webmanifest', '/icon.svg', '/icon-32.png', '/icon-180.png', '/icon-192.png', '/icon-512.png', '/icon-maskable-192.png', '/icon-maskable-512.png'];

self.addEventListener('install', (event) => event.waitUntil(Promise.all([caches.open(shellCache).then((cache) => cache.addAll(shellFiles)), self.skipWaiting()])));
self.addEventListener('activate', (event) => event.waitUntil(Promise.all([self.clients.claim(), caches.keys().then((keys) => Promise.all(keys.filter((key) => key.startsWith('resonyr-shell-') && key !== shellCache).map((key) => caches.delete(key))))])));
self.addEventListener('fetch', (event) => {
  if (event.request.method !== 'GET') return;
  const url = new URL(event.request.url);
  if (url.origin !== self.location.origin) return;
  if (/^\/api\/tracks\/\d+\/(stream|cover)$/.test(url.pathname) && url.searchParams.has('offlineUser')) {
    event.respondWith(audioResponse(event.request));
    return;
  }
  if (shellFiles.includes(url.pathname)) {
    event.respondWith(caches.open(shellCache).then(async (cache) => {
      const key = event.request.mode === 'navigate' ? '/' : url.pathname;
      try {
        const response = await fetch(event.request);
        if (response.ok) { await cache.put(key, response.clone()); return response; }
        return (await cache.match(key)) || response;
      } catch (error) {
        const saved = await cache.match(key);
        if (saved) return saved;
        throw error;
      }
    }));
    return;
  }
});
self.addEventListener('push', (event) => {
  const data = event.data?.json() || { title: 'Resonyr', body: 'Новое уведомление', url: '/' };
  event.waitUntil(self.registration.showNotification(data.title, { body: data.body, icon: '/icon.svg', badge: '/icon.svg', data: { url: data.url || '/' } }));
});
self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const targetUrl = event.notification.data?.url || '/';
  event.waitUntil(
    clients.matchAll({ type: 'window', includeUncontrolled: true }).then((windowClients) => {
      for (const client of windowClients) {
        if ('focus' in client) return client.focus();
      }
      return clients.openWindow(targetUrl);
    })
  );
});

async function audioResponse(request) {
  const cache = await caches.open('resonyr-media-v1');
  const cached = await cache.match(new Request(request.url));
  if (!cached) return fetch(request);
  const range = request.headers.get('range');
  if (!range) return cached;
  const data = await cached.arrayBuffer();
  const match = /^bytes=(\d*)-(\d*)$/.exec(range);
  if (!match) return cached;
  const start = match[1] ? Number(match[1]) : Math.max(0, data.byteLength - Number(match[2]));
  const end = match[1] && match[2] ? Math.min(Number(match[2]), data.byteLength - 1) : data.byteLength - 1;
  if ((!match[1] && !match[2]) || start > end || start >= data.byteLength || !data.byteLength) {
    return new Response(null, { status: 416, headers: { 'Content-Range': `bytes */${data.byteLength}` } });
  }
  return new Response(data.slice(start, end + 1), {
    status: 206,
    headers: {
      'Accept-Ranges': 'bytes',
      'Content-Length': String(end - start + 1),
      'Content-Range': `bytes ${start}-${end}/${data.byteLength}`,
      'Content-Type': cached.headers.get('Content-Type') || 'audio/mpeg',
    },
  });
}

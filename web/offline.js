const offline = {
  userId: null,
  disconnected: false,
  syncing: false,
  downloading: false,
  supported: window.isSecureContext && 'caches' in window && 'serviceWorker' in navigator,
  mediaCache: 'resonyr-media-v1',
  dataCache: 'resonyr-data-v1',

  key(name) { return `resonyr:${this.userId}:${name}`; },
  mediaURL(id, kind = 'stream') { return `/api/tracks/${id}/${kind}?offlineUser=${this.userId}`; },
  dataURL(path) {
    const url = new URL(path, location.origin);
    url.searchParams.set('offlineUser', this.userId);
    return url.href;
  },
  pending() { return JSON.parse(localStorage.getItem(this.key('pending')) || '[]'); },
  notify(message) {
    const status = document.getElementById('offlineStatus');
    if (status) { status.hidden = false; status.textContent = message; }
  },
  setDisconnected(value) {
    const changed = this.disconnected !== value;
    this.disconnected = value;
    if (changed) window.dispatchEvent(new Event('resonyr-connection'));
    if (value) this.notify('Нет связи с сервером. Доступна скачанная музыка; изменения сохраняются на устройстве.');
  },
  canQueue(path, method) {
    return (method === 'PUT' && (/^\/api\/tracks\/\d+\/favorite$/.test(path) || path === '/api/playback' || /^\/api\/playlists\/\d+$/.test(path)))
      || (method === 'POST' && /^\/api\/playlists\/\d+\/tracks$/.test(path))
      || (method === 'DELETE' && /^\/api\/playlists\/\d+\/tracks\/\d+$/.test(path));
  },
  canCache(path) {
    return /^\/api\/(library|search|playlists|tracks|history|themes|playback)(\?|\/\d+(?:\?)?|$)/.test(path) && !/\/(cover|stream)/.test(path);
  },
  async network(path, options = {}) {
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), path.includes('/artists') ? 45000 : 8000);
    try {
      const response = await fetch(path, { ...options, signal: controller.signal, cache: 'no-store' });
      if (response.status >= 500 && path === '/api/me') throw new Error('Сервер недоступен');
      return response;
    } catch (cause) {
      if (path !== '/api/me') {
        try {
          const probe = await fetch('/api/me', { cache: 'no-store', signal: AbortSignal.timeout(5000) });
          if (probe.status < 500) return Response.json({ error: 'Сервис не ответил вовремя. Повторите запрос.' }, { status: 504 });
        } catch {}
      }
      const error = new Error('Нет связи с сервером', { cause });
      error.network = true;
      throw error;
    } finally { clearTimeout(timeout); }
  },
  async queue(path, options) {
    const pending = this.pending();
    const operation = { id: crypto.randomUUID(), path, method: options.method, body: options.body };
    if (options.method === 'PUT') {
      const index = pending.findIndex((item) => item.path === path && item.method === 'PUT');
      if (index >= 0) pending.splice(index, 1);
    }
    pending.push(operation);
    localStorage.setItem(this.key('pending'), JSON.stringify(pending));
    await this.applyLocal(operation);
    return new Response(null, { status: 204 });
  },
  async applyLocal(operation) {
    const cache = await caches.open(this.dataCache);
    const payload = operation.body ? JSON.parse(operation.body) : {};
    const trackMatch = operation.path.match(/^\/api\/tracks\/(\d+)\/favorite$/);
    if (trackMatch) {
      const tracks = JSON.parse(localStorage.getItem(`playerOfflineTracks:${this.userId}`) || '[]');
      tracks.forEach((track) => { if (track.id === Number(trackMatch[1])) track.favorite = payload.favorite; });
      localStorage.setItem(`playerOfflineTracks:${this.userId}`, JSON.stringify(tracks));
    }
    for (const request of await cache.keys()) {
      const url = new URL(request.url);
      if (url.searchParams.get('offlineUser') !== String(this.userId)) continue;
      const response = await cache.match(request);
      const data = await response.json();
      if (trackMatch) {
        const tracks = Array.isArray(data) ? data : data.tracks || [];
        tracks.forEach((track) => { if (track.id === Number(trackMatch[1])) track.favorite = payload.favorite; });
      }
      const playlistMatch = operation.path.match(/^\/api\/playlists\/(\d+)(?:\/tracks(?:\/(\d+))?)?$/);
      if (playlistMatch) {
        const playlistId = Number(playlistMatch[1]);
        if (url.pathname === '/api/playlists') {
          const playlist = data.find((item) => item.id === playlistId);
          if (playlist && operation.method === 'PUT') Object.assign(playlist, payload);
          if (playlist && operation.method !== 'PUT') {
            const ids = new Set(playlist.trackIds || []);
            if (operation.method === 'POST') ids.add(payload.trackId);
            else ids.delete(Number(playlistMatch[2]));
            playlist.trackIds = [...ids];
            playlist.trackCount = ids.size;
          }
        }
        if (url.pathname === `/api/playlists/${playlistId}` && Array.isArray(data)) {
          if (operation.method === 'DELETE') {
            const index = data.findIndex((item) => item.id === Number(playlistMatch[2]));
            if (index >= 0) data.splice(index, 1);
          }
          if (operation.method === 'POST' && !data.some((item) => item.id === payload.trackId)) {
            const tracks = JSON.parse(localStorage.getItem(`playerOfflineTracks:${this.userId}`) || '[]');
            const track = tracks.find((item) => item.id === payload.trackId);
            if (track) data.push(track);
          }
        }
      }
      if (operation.path === '/api/playback' && url.pathname === '/api/playback') Object.assign(data, payload);
      await cache.put(request, Response.json(data));
    }
  },
  async request(path, options = {}) {
    const method = options.method || 'GET';
    const queueable = this.supported && this.userId && this.canQueue(path, method);
    if (queueable && (this.disconnected || this.pending().length)) return this.queue(path, options);
    try {
      if (this.disconnected && !['/api/me', '/api/login', '/api/logout'].includes(path)) {
        const error = new Error('Нет связи с сервером');
        error.network = true;
        throw error;
      }
      const response = method === 'GET' || queueable ? await this.network(path, options) : await fetch(path, options);
      if (response.ok && method === 'GET' && this.supported && this.userId && this.canCache(path)) {
        const cache = await caches.open(this.dataCache);
        await cache.put(this.dataURL(path), response.clone()).catch(() => {});
      }
      return response;
    } catch (error) {
      if (!error.network && !(error instanceof TypeError)) throw error;
      error.network = true;
      this.setDisconnected(true);
      if (queueable) return this.queue(path, options);
      if (method === 'GET' && this.supported && this.userId && this.canCache(path)) {
        const cache = await caches.open(this.dataCache);
        const saved = await cache.match(this.dataURL(path));
        if (saved) return saved;
        if (path.startsWith('/api/search')) {
          const query = new URL(path, location.origin).searchParams.get('q') || '';
          const tracks = JSON.parse(localStorage.getItem(`playerOfflineTracks:${this.userId}`) || '[]');
          return Response.json(tracks.filter((item) => query === '%' || `${item.title} ${item.artist} ${item.album}`.toLowerCase().includes(query.toLowerCase())));
        }
      }
      throw error;
    }
  },
  async sync() {
    if (!this.userId || this.syncing) return;
    this.syncing = true;
    const userId = this.userId;
    try {
      const response = await this.network('/api/me');
      if (!response.ok) {
        this.notify('Для синхронизации войдите в свой аккаунт. Изменения сохранены на устройстве.');
        if (response.status === 401) window.dispatchEvent(new Event('resonyr-auth'));
        return;
      }
      const user = await response.json();
      if (user.id !== userId || this.userId !== userId) return;
      this.restoredUser = user;
      while (this.userId === userId && this.pending().length) {
        const operation = this.pending()[0];
        const result = await this.network(operation.path, {
          method: operation.method, body: operation.body,
          headers: { 'Content-Type': 'application/json' },
        });
        if (this.userId !== userId) return;
        if (!result.ok) {
          this.notify(`Не удалось синхронизировать изменение (${result.status}). Оно сохранено на устройстве.`);
          return;
        }
        localStorage.setItem(this.key('pending'), JSON.stringify(this.pending().filter((item) => item.id !== operation.id)));
      }
      if (this.userId !== userId) return;
      if (this.disconnected) this.notify('Связь восстановлена. Изменения синхронизированы.');
      this.setDisconnected(false);
    } catch (error) {
      if (error.network) this.setDisconnected(true);
    } finally { this.syncing = false; }
  },
  async download(item) {
    if (this.downloading) throw new Error('Дождитесь завершения текущего скачивания');
    this.downloading = true;
    const cache = await caches.open(this.mediaCache);
    const streamURL = this.mediaURL(item.id);
    try {
      if (!navigator.serviceWorker?.controller) throw new Error('Офлайн ещё не готов. Обновите страницу после первого открытия. Требуется HTTPS или localhost.');
      const persistent = await (navigator.storage?.persist?.() || Promise.resolve(false)).catch(() => false);
      const response = await fetch(streamURL);
      if (!response.ok || response.status === 206) throw new Error('Не удалось скачать трек целиком');
      const total = Number(response.headers.get('Content-Length'));
      const estimate = await navigator.storage?.estimate?.() || {};
      const available = estimate.quota ? Math.max(0, estimate.quota - (estimate.usage || 0) - 5 * 1024 * 1024) : Infinity;
      if (total > available) { await response.body?.cancel(); throw new Error('Недостаточно места. Удалите ненужные скачанные треки.'); }
      let loaded = 0;
      const reader = response.body.getReader();
      const progress = new ReadableStream({
        pull: async (controller) => {
          try {
            const part = await reader.read();
            if (part.done) { controller.close(); return; }
            loaded += part.value.byteLength;
            if (loaded > available) throw new Error('Недостаточно места для трека');
            this.notify(`Скачивание «${item.title}»: ${total ? Math.min(100, Math.round(loaded / total * 100)) + '%' : (loaded / 1048576).toFixed(1) + ' МБ'}`);
            controller.enqueue(part.value);
          } catch (error) { await reader.cancel().catch(() => {}); controller.error(error); }
        },
        cancel: (reason) => reader.cancel(reason),
      });
      await cache.put(streamURL, new Response(progress, { headers: response.headers }));
      if (item.hasCover) {
        const cover = await fetch(this.mediaURL(item.id, 'cover'));
        if (cover.ok) await cache.put(this.mediaURL(item.id, 'cover'), cover);
      }
      this.notify(`«${item.title}» скачан.${persistent ? '' : ' Браузер может очистить загрузки при нехватке места.'}`);
    } catch (error) {
      await cache.delete(streamURL);
      const message = error.name === 'QuotaExceededError' ? 'Недостаточно места для скачивания' : error.message;
      this.notify(message);
      throw new Error(message);
    } finally { this.downloading = false; }
  },
};

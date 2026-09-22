const state = {
  currentPath: '',
  currentFilePath: '',
  currentPlaylist: null,
  currentTrack: null,
  currentView: 'library',
  activeDeviceId: '',
  deviceId: localStorage.getItem('playerDeviceId') || crypto.randomUUID(),
  deviceStream: null,
  deviceHeartbeat: null,
  downloadBatch: false,
  format: '',
  lastSavedAt: 0,
  pendingTrackId: null,
  playlistLayout: localStorage.getItem('playerPlaylistLayout') === 'list' ? 'list' : 'grid',
  queue: [],
  queueIndex: -1,
  repeat: 0,
  removeTrackCover: false,
  shuffle: false,
  sort: '',
  isAdmin: false,
  userId: null,
  userName: '',
  visibleTracks: [],
};

const elements = Object.fromEntries([
  'removePlaylistDownloadsButton', 'clearDownloadsButton',
  'accountButton', 'accountForm', 'accountModal', 'addTrackModal', 'adminButton', 'app', 'artistForm', 'artistModal', 'artistResults', 'audio', 'breadcrumbs', 'clearNotificationsButton', 'colorModeSelect', 'coverImage', 'coverPlaceholder', 'currentTime', 'deletePlaylistButton', 'deviceSelect', 'duration', 'editPlaylistButton', 'editUserForm', 'editUserModal', 'empty', 'emptyText', 'emptyTitle', 'enablePushButton', 'folders', 'formatSelect', 'libraryDescription', 'libraryTitle', 'login',
  'fileDropzone', 'filesButton', 'folderForm', 'folderInput', 'folderModal', 'loginError', 'loginForm', 'logoutButton', 'menuButton', 'musicForm', 'musicModal', 'musicPath', 'newBackupButton', 'newFolderButton', 'newPlaylistButton', 'newSourceButton', 'newUserButton', 'nextButton', 'operationsButton', 'playButton', 'playFolderButton',
  'playerArtist', 'playerTitle', 'playlistChoices', 'playlistCoverDropzone', 'playlistCoverInput', 'playlistCoverPicture', 'playlistCoverPreview', 'playlistCoverPrompt', 'playlistForm', 'playlistLayoutControl', 'playlistLayoutSelect', 'playlistModal', 'playlistModalTitle', 'playlistSubmitButton', 'previousButton', 'progressRange', 'repeatButton', 'scanButton', 'searchInput',
  'donationLaterButton', 'donationModal', 'donationNeverButton', 'renameFileForm', 'renameFileModal', 'shuffleButton', 'sortSelect', 'sourceForm', 'sourceModal', 'sourceProvider', 'sourcesButton', 'supportLink', 'themeForm', 'themeModal', 'themeSelect', 'trackCoverDropzone', 'trackCoverInput', 'trackCoverPicture', 'trackCoverPreview', 'trackCoverPrompt', 'trackCoverRemoveButton', 'trackMetadataForm', 'trackMetadataModal', 'trackTechnicalInfo', 'tracks', 'updateBanner', 'updateBannerLink', 'updateBannerText', 'uploadFilesButton', 'uploadFolderButton', 'uploadMusicButton', 'uploadThemeButton', 'userForm', 'userModal', 'username', 'volumeButton', 'volumeRange', 'volumeWaveLarge', 'volumeWaveSmall', 'watchArtistButton',
].map((id) => [id, document.getElementById(id)]));

async function api(path, options = {}) {
  const response = await offline.request(path, {
    ...options,
    headers: typeof options.body === 'string' ? { 'Content-Type': 'application/json', ...options.headers } : options.headers,
  });
  if (response.status === 401) {
    showLogin();
    throw new Error('Требуется авторизация');
  }
  if (!response.ok) {
    const data = await response.json().catch(() => ({}));
    throw new Error(data.error || 'Ошибка запроса');
  }
  return response.status === 204 ? null : response.json();
}

async function start() {
  try {
    const currentUser = await api('/api/me');
    elements.login.hidden = true;
    elements.app.hidden = false;
    elements.app.classList.toggle('app--sidebar-collapsed', innerWidth > 760 && localStorage.getItem('playerSidebarCollapsed') === 'true');
    elements.username.textContent = currentUser.username;
    state.userId = currentUser.id;
    offline.userId = currentUser.id;
    await offline.sync();
    state.userName = currentUser.username;
    state.isAdmin = currentUser.isAdmin;
    localStorage.setItem('playerLastUser', JSON.stringify({ id: currentUser.id, username: currentUser.username, colorMode: currentUser.colorMode }));
    applyColorMode(currentUser.colorMode);
    elements.colorModeSelect.value = currentUser.colorMode;
    elements.scanButton.hidden = !currentUser.isAdmin;
    elements.adminButton.hidden = !currentUser.isAdmin;
    elements.sourcesButton.hidden = !currentUser.isAdmin;
    elements.filesButton.hidden = !currentUser.isAdmin;
    elements.operationsButton.hidden = !currentUser.isAdmin;
    await loadThemes(currentUser.theme);
    await loadLibrary('');
    const sourceStatus = new URLSearchParams(location.search).get('source');
    if (sourceStatus) {
      history.replaceState(null, '', location.pathname);
      if (sourceStatus === 'connected' && currentUser.isAdmin) {
        setActiveNavigation('sources');
        await loadSources();
      } else if (sourceStatus !== 'connected') {
        elements.libraryTitle.textContent = 'Не удалось подключить облако';
      }
    }
    if (currentUser.isAdmin) loadUpdateNotice().catch(() => {});
    await setupDevice();
    await restorePlayback();
    window.setTimeout(showDonationReminder, 1500);
  } catch (error) {
    if (!error.network || !await startOffline()) showLogin();
  }
}

localStorage.setItem('playerDeviceId', state.deviceId);

function applyColorMode(mode) {
  if (mode === 'light' || mode === 'dark') document.documentElement.dataset.colorMode = mode;
  else delete document.documentElement.dataset.colorMode;
}

function deviceName() {
  const platform = navigator.userAgentData?.platform || navigator.platform || 'Браузер';
  const mobile = /Android|iPhone|iPad/i.test(navigator.userAgent);
  return `${mobile ? 'Смартфон' : 'Компьютер'} · ${platform}`;
}

async function setupDevice() {
  await api('/api/devices/register', { method: 'POST', body: JSON.stringify({ id: state.deviceId, name: deviceName() }) });
  await loadDevices();
  state.deviceStream?.close();
  state.deviceStream = new EventSource('/api/devices/events');
  state.deviceStream.onmessage = async (event) => {
    const message = JSON.parse(event.data);
    if (message.type === 'device') {
      const wasActive = isOutputDevice();
      state.activeDeviceId = message.deviceId;
      if (wasActive && !isOutputDevice()) elements.audio.pause();
      if (isOutputDevice()) await restorePlayback(true);
      await loadDevices();
    }
    if (message.type === 'control' && isOutputDevice()) await applyControl(message);
    if (message.type === 'state' && !isOutputDevice()) await restorePlayback();
  };
  clearInterval(state.deviceHeartbeat);
  state.deviceHeartbeat = setInterval(async () => {
    await api('/api/devices/register', { method: 'POST', body: JSON.stringify({ id: state.deviceId, name: deviceName() }) }).catch(() => {});
    await loadDevices().catch(() => {});
  }, 20000);
}

async function loadDevices() {
  const devices = await api('/api/devices');
  state.activeDeviceId = devices.find((item) => item.active)?.id || '';
  elements.deviceSelect.closest('.device-picker').hidden = devices.length < 2;
  elements.deviceSelect.replaceChildren(...devices.map((item) => {
    const option = document.createElement('option');
    option.value = item.id;
    option.textContent = item.id === state.deviceId ? 'Это устройство' : item.name;
    return option;
  }));
  elements.deviceSelect.value = state.activeDeviceId;
}

function isOutputDevice() {
  return state.activeDeviceId === state.deviceId;
}

async function sendControl(action, position = 0) {
  if (isOutputDevice() || offline.disconnected) {
    await applyControl({ action, position });
    return;
  }
  await api('/api/playback/control', { method: 'POST', body: JSON.stringify({ action, position }) });
}

function offlineKey() {
  return `playerOfflineTracks:${state.userId}`;
}

function offlineTracks() {
  try {
    return JSON.parse(localStorage.getItem(offlineKey()) || '[]');
  } catch (_) {
    return [];
  }
}

async function startOffline() {
  if (!offline.supported) return false;
  let lastUser;
  try {
    lastUser = JSON.parse(localStorage.getItem('playerLastUser'));
  } catch (_) {
    return false;
  }
  if (!lastUser) return false;
  state.userId = lastUser.id;
  offline.userId = lastUser.id;
  offline.setDisconnected(true);
  const cache = await caches.open(offline.mediaCache);
  const saved = [];
  for (const track of offlineTracks()) {
    if (await cache.match(offline.mediaURL(track.id))) saved.push(track);
  }
  localStorage.setItem(offlineKey(), JSON.stringify(saved));
  state.userName = lastUser.username;
  state.isAdmin = false;
  state.activeDeviceId = state.deviceId;
  elements.username.textContent = lastUser.username;
  applyColorMode(lastUser.colorMode);
  elements.colorModeSelect.value = lastUser.colorMode || 'system';
  elements.login.hidden = true;
  elements.app.hidden = false;
  for (const id of ['scanButton', 'adminButton', 'sourcesButton', 'filesButton', 'operationsButton']) elements[id].hidden = true;
  elements.deviceSelect.closest('.device-picker').hidden = true;
  loadOffline();
  return true;
}

function loadOffline() {
  state.currentView = 'offline';
  state.currentPlaylist = null;
  renderTrackView('Офлайн', offlineTracks(), 'offline');
}

async function loadNotifications() {
  state.currentView = 'notifications';
  const items = await api('/api/notifications');
  elements.libraryTitle.textContent = 'Уведомления';
  elements.breadcrumbs.replaceChildren();
  elements.folders.replaceChildren();
  elements.tracks.hidden = false;
  elements.tracks.className = 'users';
  elements.tracks.replaceChildren(...items.map((item) => {
    const row = document.createElement('div');
    row.className = 'user-row';
    const title = document.createElement('strong');
    title.textContent = item.title;
    const body = document.createElement('span');
    body.className = 'user-row__role';
    body.textContent = item.body;
    const date = document.createElement('time');
    date.className = 'user-row__role';
    date.dateTime = item.createdAt;
    date.textContent = new Date(item.createdAt).toLocaleString();
    row.append(title, body, date);
    return row;
  }));
  elements.empty.hidden = items.length > 0;
  setEmpty('Уведомлений пока нет', 'Здесь появятся события сервера и музыкальные новинки.');
  showLibraryActions();
}

async function loadSources() {
  state.currentView = 'sources';
  const items = await api('/api/admin/sources');
  elements.libraryTitle.textContent = 'Источники музыки';
  elements.breadcrumbs.replaceChildren();
  elements.folders.replaceChildren();
  elements.tracks.hidden = false;
  elements.tracks.className = 'sources';
  elements.tracks.replaceChildren(...items.map((item) => {
    const row = document.createElement('div');
    row.className = 'source-row';
    const name = document.createElement('strong');
    name.className = 'source-row__name';
    name.textContent = item.name;
    const provider = document.createElement('span');
    provider.className = 'source-row__meta';
    provider.textContent = `${item.provider} · ${item.remotePath || 'корень'}`;
    const status = document.createElement('span');
    status.className = item.lastError ? 'source-row__status source-row__status--error' : 'source-row__status';
    status.textContent = item.lastError || (item.lastSyncAt ? `Обновлено ${new Date(item.lastSyncAt).toLocaleString()}` : 'Не синхронизирован');
    const sync = document.createElement('button');
    sync.className = 'button';
    sync.textContent = 'Синхронизировать';
    sync.addEventListener('click', async () => {
      sync.disabled = true;
      try { await api(`/api/admin/sources/${item.id}/sync`, { method: 'POST' }); await loadSources(); } finally { sync.disabled = false; }
    });
    const remove = document.createElement('button');
    remove.className = 'user-row__delete';
    remove.textContent = 'Удалить';
    remove.addEventListener('click', async () => { await api(`/api/admin/sources/${item.id}`, { method: 'DELETE' }); await loadSources(); });
    row.append(name, provider, status, sync, remove);
    return row;
  }));
  elements.empty.hidden = items.length > 0;
  setEmpty('Источников пока нет', 'Подключите Google Drive, Яндекс Диск, Dropbox или OneDrive.');
  showLibraryActions();
}

async function loadFiles(path = '') {
  const data = await api(`/api/admin/files?path=${encodeURIComponent(path)}`);
  state.currentView = 'files';
  state.currentFilePath = data.path;
  elements.libraryTitle.textContent = data.path.split('/').at(-1) || 'Локальные файлы';
  const root = makeFileBreadcrumb('Файлы', '');
  elements.breadcrumbs.replaceChildren(root);
  const parts = data.path ? data.path.split('/') : [];
  parts.forEach((part, index) => elements.breadcrumbs.append(' / ', makeFileBreadcrumb(part, parts.slice(0, index + 1).join('/'))));
  elements.folders.replaceChildren();
  elements.tracks.hidden = false;
  elements.tracks.className = 'file-list';
  const parentPath = data.path.split('/').slice(0, -1).join('/');
  const rows = data.path ? [makeParentFileRow(parentPath)] : [];
  elements.tracks.replaceChildren(...rows, ...data.items.map(makeFileRow));
  elements.fileDropzone.hidden = false;
  elements.empty.hidden = data.items.length > 0;
  setEmpty('Папка пуста', 'Создайте папку или загрузите аудиофайлы.');
  showLibraryActions();
}

function makeParentFileRow(path) {
  const row = document.createElement('button');
  row.className = 'file-row file-row--folder file-row--parent';
  const icon = document.createElement('span');
  icon.className = 'file-row__icon';
  icon.textContent = '↑';
  const name = document.createElement('strong');
  name.className = 'file-row__name';
  name.textContent = 'На уровень выше';
  const meta = document.createElement('span');
  meta.className = 'file-row__meta';
  meta.textContent = 'Перетащите сюда для переноса';
  row.append(icon, name, meta);
  row.addEventListener('click', () => loadFiles(path));
  makeFileDropTarget(row, path);
  return row;
}

function makeFileBreadcrumb(name, path) {
  const button = document.createElement('button');
  button.className = 'breadcrumbs__item';
  button.textContent = name;
  button.addEventListener('click', () => loadFiles(path));
  makeFileDropTarget(button, path);
  return button;
}

function draggedFilePath(dataTransfer) {
  return dataTransfer.getData('application/x-resonyr-path');
}

async function moveFile(path, destinationPath) {
  const name = path.split('/').at(-1);
  await api(`/api/admin/files?path=${encodeURIComponent(path)}`, { method: 'PUT', body: JSON.stringify({ name, destinationPath }) });
  await loadFiles(state.currentFilePath);
}

function makeFileDropTarget(element, destinationPath) {
  element.addEventListener('dragover', (event) => {
    if (!Array.from(event.dataTransfer.types).includes('application/x-resonyr-path')) return;
    event.preventDefault();
    event.stopPropagation();
    event.dataTransfer.dropEffect = 'move';
    element.classList.add('file-drop-target');
  });
  element.addEventListener('dragleave', () => element.classList.remove('file-drop-target'));
  element.addEventListener('drop', async (event) => {
    const path = draggedFilePath(event.dataTransfer);
    if (!path) return;
    event.preventDefault();
    event.stopPropagation();
    element.classList.remove('file-drop-target');
    try {
      await moveFile(path, destinationPath);
    } catch (error) {
      window.alert(error.message);
    }
  });
}

function makeFileRow(item) {
  const row = document.createElement('div');
  row.className = `file-row${item.isDir ? ' file-row--folder' : ''}`;
  row.draggable = true;
  const icon = document.createElement('span');
  icon.className = 'file-row__icon';
  icon.textContent = item.isDir ? '▰' : '♫';
  const name = document.createElement('strong');
  name.className = 'file-row__name';
  name.textContent = item.name;
  const meta = document.createElement('span');
  meta.className = 'file-row__meta';
  meta.textContent = item.isDir ? 'Папка' : `${formatBytes(item.size)} · ${new Date(item.modifiedAt).toLocaleString()}`;
  const rename = document.createElement('button');
  rename.className = 'button';
  rename.textContent = 'Переименовать';
  rename.addEventListener('click', (event) => {
    event.stopPropagation();
    elements.renameFileForm.elements.path.value = item.path;
    elements.renameFileForm.elements.name.value = item.name;
    elements.renameFileModal.showModal();
  });
  const remove = document.createElement('button');
  remove.className = 'user-row__delete';
  remove.textContent = 'Удалить';
  remove.addEventListener('click', async (event) => {
    event.stopPropagation();
    if (!window.confirm(`Удалить «${item.name}» без возможности восстановления?`)) return;
    await api(`/api/admin/files?path=${encodeURIComponent(item.path)}`, { method: 'DELETE' });
    await loadFiles(state.currentFilePath);
  });
  const actions = document.createElement('div');
  actions.className = 'file-row__actions';
  actions.append(rename, remove);
  row.append(icon, name, meta, actions);
  row.addEventListener('dragstart', (event) => {
    event.dataTransfer.effectAllowed = 'move';
    event.dataTransfer.setData('application/x-resonyr-path', item.path);
    event.dataTransfer.setData('text/plain', item.path);
    row.classList.add('file-row--dragging');
  });
  row.addEventListener('dragend', () => row.classList.remove('file-row--dragging'));
  if (item.isDir) {
    row.addEventListener('click', () => loadFiles(item.path));
    makeFileDropTarget(row, item.path);
  }
  return row;
}

function formatBytes(value) {
  if (value < 1024) return `${value} Б`;
  if (value < 1024 * 1024) return `${Math.round(value / 1024)} КБ`;
  return `${(value / 1024 / 1024).toFixed(1)} МБ`;
}

function isAudioFile(name) {
  return /\.(aac|flac|m4a|mp3|ogg|opus|wav)$/i.test(name);
}

async function readDroppedEntry(entry, parent = '') {
  if (entry.isFile) {
    const file = await new Promise((resolve, reject) => entry.file(resolve, reject));
    return isAudioFile(file.name) ? [{ file, path: `${parent}${file.name}` }] : [];
  }
  if (!entry.isDirectory) return [];
  const reader = entry.createReader();
  const children = [];
  for (;;) {
    const batch = await new Promise((resolve, reject) => reader.readEntries(resolve, reject));
    if (!batch.length) break;
    children.push(...batch);
  }
  const nested = await Promise.all(children.map((child) => readDroppedEntry(child, `${parent}${entry.name}/`)));
  return nested.flat();
}

async function droppedMusic(dataTransfer) {
  const entries = Array.from(dataTransfer.items || []).map((item) => item.webkitGetAsEntry?.()).filter(Boolean);
  if (entries.length) return (await Promise.all(entries.map((entry) => readDroppedEntry(entry)))).flat();
  return Array.from(dataTransfer.files || []).filter((file) => isAudioFile(file.name)).map((file) => ({ file, path: file.name }));
}

async function uploadMusicTree(items) {
  if (!items.length) throw new Error('В папке нет поддерживаемых аудиофайлов');
  const body = new FormData();
  body.append('relativePaths', JSON.stringify(items.map((item) => item.path)));
  items.forEach((item) => body.append('files', item.file, item.file.name));
  elements.uploadFolderButton.disabled = true;
  elements.fileDropzone.classList.add('file-dropzone--active');
  try {
    await api(`/api/admin/files/upload?path=${encodeURIComponent(state.currentFilePath)}`, { method: 'POST', body });
    await loadFiles(state.currentFilePath);
  } finally {
    elements.uploadFolderButton.disabled = false;
    elements.fileDropzone.classList.remove('file-dropzone--active');
  }
}

async function loadReleases() {
  state.currentView = 'releases';
  const items = await api('/api/watched-artists');
  elements.libraryTitle.textContent = 'Музыкальные новинки';
  elements.breadcrumbs.replaceChildren();
  elements.folders.replaceChildren();
  elements.tracks.hidden = false;
  elements.tracks.className = 'releases';
  elements.tracks.replaceChildren(...items.map((item) => {
    const row = document.createElement('div');
    row.className = 'release-row';
    const name = document.createElement('strong');
    name.className = 'release-row__name';
    name.textContent = item.name;
    const release = document.createElement('span');
    release.className = 'release-row__meta';
    release.textContent = item.lastReleaseTitle ? `${item.lastReleaseTitle} · ${item.lastReleaseDate}` : 'Новых релизов пока нет';
    const remove = document.createElement('button');
    remove.className = 'user-row__delete';
    remove.textContent = 'Не отслеживать';
    remove.addEventListener('click', async () => { await api(`/api/watched-artists/${item.id}`, { method: 'DELETE' }); await loadReleases(); });
    row.append(name, release, remove);
    return row;
  }));
  elements.empty.hidden = items.length > 0;
  setEmpty('Список отслеживания пуст', 'Добавьте исполнителя, чтобы получать push о новых релизах.');
  showLibraryActions();
}

async function loadOperations() {
  state.currentView = 'operations';
  const [logs, backups, update] = await Promise.all([api('/api/admin/logs'), api('/api/admin/backups'), api('/api/admin/update')]);
  elements.libraryTitle.textContent = 'Система';
  elements.breadcrumbs.replaceChildren();
  const updateCard = document.createElement(update.configured && update.url?.startsWith('https://github.com/') ? 'a' : 'div');
  updateCard.className = 'folder';
  if (updateCard instanceof HTMLAnchorElement) updateCard.href = update.url;
  const updateTitle = document.createElement('strong');
  updateTitle.className = 'folder__name';
  updateTitle.textContent = update.updateAvailable ? `Доступна версия ${update.latestVersion}` : update.configured ? `Версия ${update.currentVersion} актуальна` : 'Проверка обновлений не настроена';
  const updateMeta = document.createElement('small');
  updateMeta.className = 'track__artist';
  updateMeta.textContent = update.configured ? 'GitHub Releases' : 'Укажите UPDATE_REPOSITORY в .env';
  updateCard.append(updateTitle, updateMeta);
  elements.folders.replaceChildren(updateCard, ...backups.map((item) => {
    const link = document.createElement('a');
    link.className = 'folder';
    link.href = `/api/admin/backups/${encodeURIComponent(item.name)}`;
    const name = document.createElement('strong');
    name.className = 'folder__name';
    name.textContent = item.name;
    const meta = document.createElement('small');
    meta.className = 'track__artist';
    meta.textContent = `${Math.ceil(item.size / 1024)} КБ · ${new Date(item.createdAt).toLocaleString()}`;
    link.append(name, meta);
    return link;
  }));
  elements.tracks.hidden = false;
  elements.tracks.className = 'users';
  elements.tracks.replaceChildren(...logs.map((item) => {
    const row = document.createElement('div');
    row.className = 'user-row';
    const user = document.createElement('strong');
    user.textContent = item.username;
    const event = document.createElement('span');
    event.className = 'user-row__role';
    event.textContent = item.event;
    const date = document.createElement('time');
    date.className = 'user-row__role';
    date.textContent = new Date(item.createdAt).toLocaleString();
    row.append(user, event, date);
    return row;
  }));
  elements.empty.hidden = true;
  setEmpty('Системных событий пока нет', 'Здесь появятся журнал действий и резервные копии.');
  showLibraryActions();
}

async function loadUpdateNotice() {
  const update = await api('/api/admin/update');
  elements.updateBanner.hidden = !update.updateAvailable;
  if (!update.updateAvailable) return;
  elements.updateBannerText.textContent = `Доступна новая версия Resonyr ${update.latestVersion}`;
  elements.updateBannerLink.href = update.url;
}

function vapidKey(value) {
  const padding = '='.repeat((4 - value.length % 4) % 4);
  const data = atob((value + padding).replaceAll('-', '+').replaceAll('_', '/'));
  return Uint8Array.from(data, (char) => char.charCodeAt(0));
}

async function enablePush() {
  if (!('serviceWorker' in navigator) || !('PushManager' in window)) throw new Error('Push не поддерживается этим браузером');
  if (await Notification.requestPermission() !== 'granted') throw new Error('Разрешение на уведомления не выдано');
  const registration = await navigator.serviceWorker.ready;
  const { publicKey } = await api('/api/push/key');
  const subscription = await registration.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: vapidKey(publicKey) });
  await api('/api/push/subscription', { method: 'PUT', body: JSON.stringify(subscription) });
  elements.enablePushButton.textContent = 'Push включён';
  elements.enablePushButton.disabled = true;
}

async function removeOfflineTracks(trackIds) {
  if (offline.downloading || offline.removing || state.downloadBatch) throw new Error('Дождитесь завершения текущей операции с загрузками');
  offline.removing = true;
  try {
    const ids = new Set(trackIds);
    const cache = await caches.open(offline.mediaCache);
    for (const id of ids) {
      await Promise.all([cache.delete(offline.mediaURL(id)), cache.delete(offline.mediaURL(id, 'cover'))]);
      localStorage.setItem(offlineKey(), JSON.stringify(offlineTracks().filter((track) => track.id !== id)));
    }
    offline.notify('Загрузки удалены с этого устройства. Музыка на сервере сохранена.');
  } finally {
    offline.removing = false;
    if (state.currentView === 'offline') loadOffline();
    else if (['library', 'playlist', 'favorites', 'search', 'history'].includes(state.currentView)) renderTracks(state.visibleTracks);
    showLibraryActions();
  }
}

async function removePlaylistDownloads(item) {
  const downloadedIds = new Set(offlineTracks().map((track) => track.id));
  const trackIds = state.currentView === 'playlist' && state.currentPlaylist?.id === item.id ? state.visibleTracks.map((track) => track.id) : item.trackIds || [];
  const ids = trackIds.filter((id) => downloadedIds.has(id));
  if (!ids.length) return;
  if (!window.confirm(`Удалить скачанные треки плейлиста «${item.name}» с этого устройства (${ids.length})? Общие треки перестанут быть скачанными и в других плейлистах. Сам плейлист и файлы на сервере останутся.`)) return;
  await removeOfflineTracks(ids);
  if (state.currentView === 'playlists') await loadPlaylists();
}

async function toggleOffline(item, batch = false) {
  if (offline.removing || offline.downloading || (state.downloadBatch && !batch)) throw new Error('Дождитесь завершения текущей операции с загрузками');
  const items = offlineTracks();
  const index = items.findIndex((track) => track.id === item.id);
  const cache = await caches.open(offline.mediaCache);
  const streamURL = offline.mediaURL(item.id);
  const coverURL = offline.mediaURL(item.id, 'cover');
  if (index >= 0) {
    await removeOfflineTracks([item.id]);
    return false;
  } else {
    await offline.download(item);
    items.push(item);
  }
  try {
    localStorage.setItem(offlineKey(), JSON.stringify(items));
  } catch (error) {
    if (index < 0) await Promise.all([cache.delete(streamURL), cache.delete(coverURL)]);
    throw error;
  }
  return index < 0;
}

async function downloadTracks(tracks) {
  if (state.downloadBatch || offline.downloading || offline.removing) throw new Error('Дождитесь завершения текущей операции с загрузками');
  state.downloadBatch = true;
  try {
    const cache = await caches.open(offline.mediaCache);
    const downloadedIds = new Set();
    for (const track of offlineTracks()) {
      if (await cache.match(offline.mediaURL(track.id))) downloadedIds.add(track.id);
    }
    localStorage.setItem(offlineKey(), JSON.stringify(offlineTracks().filter((track) => downloadedIds.has(track.id))));
    for (const item of tracks) {
      if (!downloadedIds.has(item.id)) {
        await toggleOffline(item, true);
        downloadedIds.add(item.id);
      }
    }
  } finally { state.downloadBatch = false; }
}

async function applyControl(message) {
  if (message.action === 'load') await restorePlayback(true);
  if (message.action === 'toggle') elements.audio.paused ? await elements.audio.play() : elements.audio.pause();
  if (message.action === 'play') await elements.audio.play();
  if (message.action === 'pause') elements.audio.pause();
  if (message.action === 'next') move(1);
  if (message.action === 'previous') move(-1);
  if (message.action === 'seek' && elements.audio.duration) elements.audio.currentTime = message.position;
}

function showLogin() {
  elements.app.hidden = true;
  elements.login.hidden = false;
}

function setActiveNavigation(action) {
  if (innerWidth <= 760) document.querySelector('.sidebar').classList.remove('sidebar--open');
  document.querySelectorAll('[data-action]').forEach((item) => {
    item.classList.toggle('nav__item--active', item.dataset.action === action);
  });
}

async function loadThemes(activeTheme = elements.themeSelect.value) {
  const themes = await api('/api/themes');
  elements.themeSelect.replaceChildren(...themes.map((theme) => {
    const option = document.createElement('option');
    option.value = theme.id;
    option.textContent = `Оформление: ${theme.name}`;
    return option;
  }));
  elements.themeSelect.value = activeTheme;
  applyTheme(activeTheme);
}

function applyTheme(theme) {
  let link = document.getElementById('userTheme');
  if (!theme) {
    link?.remove();
    return;
  }
  if (!link) {
    link = document.createElement('link');
    link.id = 'userTheme';
    link.rel = 'stylesheet';
    document.head.append(link);
  }
  link.href = `/api/themes/${theme}/theme.css`;
}

async function loadLibrary(path, recursive = false) {
  state.currentPath = path;
  const query = new URLSearchParams({ path });
  if (recursive) query.set('recursive', '1');
  if (state.sort) query.set('sort', state.sort);
  if (state.format) query.set('format', state.format);
  const data = await api(`/api/library?${query}`);
  if (recursive) return data.tracks;
  state.currentView = 'library';
  state.currentPlaylist = null;
  showLibraryActions();
  renderLibrary(data);
  return data.tracks;
}

function renderLibrary(data) {
  const pathParts = data.path ? data.path.split('/') : [];
  elements.libraryTitle.textContent = pathParts.at(-1) || 'Медиатека';
  elements.breadcrumbs.replaceChildren(makeBreadcrumb('Медиатека', ''));
  pathParts.forEach((part, index) => {
    elements.breadcrumbs.append(' / ', makeBreadcrumb(part, pathParts.slice(0, index + 1).join('/')));
  });
  elements.folders.replaceChildren(...data.folders.map(makeFolder));
  renderTracks(data.tracks);
  elements.empty.hidden = data.folders.length > 0 || data.tracks.length > 0;
  setEmpty('Здесь пока нет музыки', 'Добавьте аудиофайлы в папку music и запустите сканирование.');
}

function setEmpty(title, text) {
  elements.emptyTitle.textContent = title;
  elements.emptyText.textContent = text;
}

function makeBreadcrumb(name, path) {
  const button = document.createElement('button');
  button.className = 'breadcrumbs__item';
  button.textContent = name;
  button.addEventListener('click', () => loadLibrary(path));
  return button;
}

function makeFolder(item) {
  const button = document.createElement('button');
  button.className = 'folder';
  const icon = document.createElement('span');
  icon.className = 'folder__icon';
  icon.textContent = '▰';
  const name = document.createElement('span');
  name.className = 'folder__name';
  name.textContent = item.name;
  button.append(icon, name);
  button.addEventListener('click', () => loadLibrary(item.path));
  return button;
}

function renderTracks(tracks) {
  state.visibleTracks = tracks;
  elements.tracks.className = 'tracks';
  elements.tracks.hidden = tracks.length === 0;
  elements.tracks.replaceChildren(...tracks.map((item, index) => makeTrack(item, index, tracks)));
}

function makeTrack(item, index, tracks) {
  const row = document.createElement('div');
  row.className = `track${state.currentTrack?.id === item.id ? ' track--active' : ''}`;
  row.dataset.id = item.id;

  const number = document.createElement('span');
  number.className = 'track__number';
  number.textContent = index + 1;
  const picture = document.createElement('picture');
  picture.className = 'track__picture';
  const image = document.createElement('img');
  image.className = 'track__image';
  image.src = item.hasCover ? offline.mediaURL(item.id, 'cover') : '/icon.svg';
  image.alt = '';
  image.loading = 'lazy';
  picture.append(image);
  const info = document.createElement('div');
  info.className = 'track__info';
  const title = document.createElement('strong');
  title.className = 'track__title';
  title.textContent = item.title;
  const artist = document.createElement('div');
  artist.className = 'track__artist';
  artist.textContent = item.artist || 'Неизвестный исполнитель';
  info.append(title, artist);
  const album = document.createElement('span');
  album.className = 'track__album';
  album.textContent = item.album || 'Без альбома';
  const format = document.createElement('span');
  format.className = 'track__format';
  format.textContent = `${item.format.toUpperCase()}${item.duration ? ` · ${formatTime(item.duration)}` : ''}`;
  format.title = [item.genre, item.year, item.bitrate ? `${Math.round(item.bitrate / 1000)} kbps` : '', item.sampleRate ? `${item.sampleRate} Hz` : ''].filter(Boolean).join(' · ');
  const favorite = document.createElement('button');
  favorite.className = `track__favorite${item.favorite ? ' track__favorite--active' : ''}`;
  favorite.textContent = item.favorite ? '♥' : '♡';
  favorite.ariaLabel = 'Избранное';
  favorite.addEventListener('click', async (event) => {
    event.stopPropagation();
    item.favorite = !item.favorite;
    await api(`/api/tracks/${item.id}/favorite`, { method: 'PUT', body: JSON.stringify({ favorite: item.favorite }) });
    favorite.textContent = item.favorite ? '♥' : '♡';
    favorite.classList.toggle('track__favorite--active', item.favorite);
  });
  const add = document.createElement('button');
  add.className = 'track__add';
  add.textContent = '＋';
  add.ariaLabel = 'Добавить в плейлист';
  add.addEventListener('click', async (event) => {
    event.stopPropagation();
    state.pendingTrackId = item.id;
    const playlists = await api('/api/playlists');
    elements.playlistChoices.replaceChildren(...playlists.filter((playlist) => playlist.permission !== 'view').map(makePlaylistChoice));
    elements.addTrackModal.showModal();
  });
  const edit = document.createElement('button');
  edit.className = 'track__add';
  edit.textContent = '✎';
  edit.ariaLabel = 'Изменить данные трека';
  edit.addEventListener('click', (event) => {
    event.stopPropagation();
    openTrackMetadata(item);
  });
  const download = document.createElement('button');
  download.className = 'track__add';
  const downloaded = offlineTracks().some((track) => track.id === item.id);
  download.textContent = downloaded ? '✓' : '↓';
  download.ariaLabel = downloaded ? 'Удалить офлайн-копию' : 'Скачать для офлайн-прослушивания';
  download.title = downloaded ? 'Удалить загрузку с устройства' : 'Скачать на устройство';
  download.disabled = !offline.supported;
  if (!offline.supported) download.title = 'Для скачивания откройте плеер по HTTPS';
  download.addEventListener('click', async (event) => {
    event.stopPropagation();
    download.disabled = true;
    try {
      const saved = await toggleOffline(item);
      download.textContent = saved ? '✓' : '↓';
      download.ariaLabel = saved ? 'Удалить офлайн-копию' : 'Скачать для офлайн-прослушивания';
      download.title = saved ? 'Удалить загрузку с устройства' : 'Скачать на устройство';
      showLibraryActions();
      if (!saved && state.currentView === 'offline') loadOffline();
    } catch (error) {
      offline.notify(error.message);
    } finally {
      download.disabled = false;
    }
  });
  const actions = document.createElement('div');
  actions.className = 'track__actions';
  if (state.currentView === 'playlist' && state.currentPlaylist?.permission === 'edit') {
    const remove = document.createElement('button');
    remove.className = 'track__add';
    remove.textContent = '−';
    remove.ariaLabel = 'Удалить из плейлиста';
    remove.addEventListener('click', async (event) => {
      event.stopPropagation();
      await api(`/api/playlists/${state.currentPlaylist.id}/tracks/${item.id}`, { method: 'DELETE' });
      renderTrackView(state.currentPlaylist.name, await api(`/api/playlists/${state.currentPlaylist.id}`), 'playlist');
    });
    actions.append(remove);
  }
  actions.append(download);
  if (state.currentView !== 'offline') {
    if (state.isAdmin) actions.append(edit);
    actions.append(add, favorite);
  }
  if (state.currentView === 'offline') actions.append(add, favorite);
  row.append(number, picture, info, album, format, actions);
  row.addEventListener('click', () => requestPlayQueue(tracks, index));
  return row;
}

async function requestPlayQueue(tracks, index = 0) {
  const queue = state.shuffle ? [...tracks].sort(() => Math.random() - 0.5) : [...tracks];
  const currentTrack = queue[Math.min(index, queue.length - 1)];
  if (!currentTrack) return;
  state.queue = queue;
  state.queueIndex = queue.indexOf(currentTrack);
  state.currentTrack = currentTrack;
  const playLocally = isOutputDevice();
  if (offline.disconnected) {
    playCurrent();
    return;
  }
  if (playLocally) playCurrent();
  await api('/api/playback', { method: 'PUT', body: JSON.stringify({ currentTrackId: currentTrack.id, position: 0, queue: queue.map((item) => item.id), isPlaying: true }) });
  if (!playLocally) await sendControl('load');
}

function playCurrent() {
  const item = state.queue[state.queueIndex];
  if (!item) return;
  state.currentTrack = item;
  elements.audio.src = offline.mediaURL(item.id);
  elements.playerTitle.textContent = item.title;
  elements.playerArtist.textContent = item.artist || 'Неизвестный исполнитель';
  elements.coverImage.hidden = !item.hasCover;
  elements.coverPlaceholder.hidden = item.hasCover;
  elements.coverImage.src = item.hasCover ? offline.mediaURL(item.id, 'cover') : '';
  elements.audio.play();
  api('/api/history', { method: 'POST', body: JSON.stringify({ trackId: item.id, position: 0 }) }).catch(() => {});
  persistPlayback();
  updatePlayButton();
  document.querySelectorAll('.track').forEach((row) => row.classList.toggle('track--active', Number(row.dataset.id) === item.id));
  navigator.mediaSession && (navigator.mediaSession.metadata = new MediaMetadata({
    title: item.title,
    artist: item.artist,
    album: item.album,
    artwork: item.hasCover ? [{ src: offline.mediaURL(item.id, 'cover') }] : [],
  }));
}

function showLibraryActions() {
  const downloadedIds = new Set(offlineTracks().map((track) => track.id));
  elements.clearDownloadsButton.hidden = downloadedIds.size === 0 || !['library', 'offline', 'playlists', 'playlist'].includes(state.currentView);
  elements.removePlaylistDownloadsButton.hidden = state.currentView !== 'playlist' || !state.visibleTracks.some((track) => downloadedIds.has(track.id));
  elements.formatSelect.closest('.select').hidden = state.currentView !== 'library';
  elements.sortSelect.closest('.select').hidden = state.currentView !== 'library';
  elements.newPlaylistButton.hidden = state.currentView !== 'playlists';
  elements.playlistLayoutControl.hidden = state.currentView !== 'playlists';
  elements.editPlaylistButton.hidden = state.currentView !== 'playlist' || state.currentPlaylist?.permission !== 'edit';
  elements.newUserButton.hidden = state.currentView !== 'admin';
  elements.uploadThemeButton.hidden = state.currentView !== 'admin';
  elements.uploadMusicButton.hidden = !state.isAdmin || state.currentView !== 'library';
  elements.newFolderButton.hidden = state.currentView !== 'files';
  elements.uploadFilesButton.hidden = state.currentView !== 'files';
  elements.uploadFolderButton.hidden = state.currentView !== 'files';
  elements.fileDropzone.hidden = state.currentView !== 'files';
  elements.newSourceButton.hidden = state.currentView !== 'sources';
  elements.watchArtistButton.hidden = state.currentView !== 'releases';
  elements.newBackupButton.hidden = state.currentView !== 'operations';
  elements.deletePlaylistButton.hidden = state.currentView !== 'playlist';
  elements.clearNotificationsButton.hidden = state.currentView !== 'notifications';
  elements.enablePushButton.hidden = state.currentView !== 'notifications';
  elements.playFolderButton.hidden = state.currentView === 'admin' || state.currentView === 'sources' || state.currentView === 'files' || state.currentView === 'operations' || state.currentView === 'releases' || state.currentView === 'playlists' || state.currentView === 'notifications';
  elements.folders.className = state.currentView === 'playlists' && state.playlistLayout === 'list' ? 'folders folders--playlist-list' : 'folders';
  elements.libraryDescription.textContent = state.currentView === 'playlist' ? state.currentPlaylist?.description || '' : '';
  elements.libraryDescription.hidden = !elements.libraryDescription.textContent;
}

function renderTrackView(title, tracks, view) {
  state.currentView = view;
  elements.libraryTitle.textContent = title;
  elements.breadcrumbs.replaceChildren();
  elements.folders.replaceChildren();
  renderTracks(tracks);
  elements.empty.hidden = tracks.length > 0;
  setEmpty(view === 'favorites' ? 'Нет избранных треков' : view === 'history' ? 'История пока пуста' : view === 'search' ? 'Ничего не найдено' : 'В плейлисте нет треков', 'Здесь появятся композиции после первого добавления или прослушивания.');
  showLibraryActions();
}

async function loadPlaylists() {
  state.currentView = 'playlists';
  state.currentPlaylist = null;
  const playlists = await api('/api/playlists');
  elements.libraryTitle.textContent = 'Плейлисты';
  elements.breadcrumbs.replaceChildren();
  elements.tracks.replaceChildren();
  elements.tracks.hidden = true;
  elements.playlistLayoutSelect.value = state.playlistLayout;
  elements.folders.replaceChildren(...playlists.map(makePlaylist));
  elements.empty.hidden = playlists.length > 0;
  setEmpty('Плейлистов пока нет', 'Создайте первый ручной плейлист.');
  showLibraryActions();
}

function makePlaylist(item) {
  const card = document.createElement('article');
  card.className = state.playlistLayout === 'list' ? 'playlist-card playlist-card--list' : 'playlist-card';
  const button = document.createElement('button');
  button.className = 'playlist-card__open';
  const picture = document.createElement('picture');
  picture.className = 'playlist-card__picture';
  const image = document.createElement('img');
  image.className = 'playlist-card__image';
  image.src = item.hasCover ? `/api/playlists/${item.id}/cover` : '/icon.svg';
  image.alt = `Обложка плейлиста «${item.name}»`;
  picture.append(image);
  const name = document.createElement('span');
  name.className = 'playlist-card__name';
  name.textContent = item.name;
  const description = document.createElement('span');
  description.className = 'playlist-card__description';
  description.textContent = item.description;
  const meta = document.createElement('small');
  meta.className = 'playlist-card__meta';
  const downloadedIds = new Set(offlineTracks().map((track) => track.id));
  const downloadedCount = item.trackIds.filter((id) => downloadedIds.has(id)).length;
  const offlineStatus = downloadedCount === item.trackCount && item.trackCount > 0 ? 'скачан' : downloadedCount > 0 ? `${downloadedCount}/${item.trackCount} скачано` : `${item.trackCount} треков`;
  meta.textContent = `${item.owner} · ${offlineStatus}`;
  button.append(picture, name, description, meta);
  button.addEventListener('click', () => openPlaylist(item));
  const download = document.createElement('button');
  download.className = 'playlist-card__download';
  download.type = 'button';
  download.textContent = downloadedCount === item.trackCount && item.trackCount > 0 ? '✓' : '↓';
  download.title = downloadedCount === item.trackCount && item.trackCount > 0 ? 'Удалить загрузки плейлиста с устройства' : 'Скачать плейлист';
  download.ariaLabel = download.title;
  download.disabled = !offline.supported || item.trackCount === 0;
  download.addEventListener('click', async () => {
    download.disabled = true;
    try {
      if (downloadedCount === item.trackCount && item.trackCount > 0) {
        await removePlaylistDownloads(item);
        return;
      }
      download.textContent = '…';
      await downloadTracks(await api(`/api/playlists/${item.id}`));
      await loadPlaylists();
    } catch (error) {
      offline.notify(error.message);
      download.disabled = false;
      download.textContent = 'Повторить';
    } finally { download.disabled = !offline.supported || item.trackCount === 0; }
  });
  card.append(button);
  card.append(download);
  return card;
}

async function openPlaylist(item) {
  const tracks = await api(`/api/playlists/${item.id}`);
  state.currentPlaylist = item;
  renderTrackView(item.name, tracks, 'playlist');
}

function makePlaylistChoice(item) {
  const button = document.createElement('button');
  button.className = 'modal__choice';
  button.textContent = item.name;
  button.addEventListener('click', async () => {
    await api(`/api/playlists/${item.id}/tracks`, { method: 'POST', body: JSON.stringify({ trackId: state.pendingTrackId }) });
    elements.addTrackModal.close();
  });
  return button;
}

async function loadAdmin() {
  state.currentView = 'admin';
  const users = await api('/api/admin/users');
  elements.libraryTitle.textContent = 'Пользователи';
  elements.breadcrumbs.replaceChildren();
  elements.folders.replaceChildren();
  elements.tracks.hidden = false;
  elements.tracks.className = 'users';
  elements.tracks.replaceChildren(...users.map(makeUser));
  elements.empty.hidden = users.length > 0;
  showLibraryActions();
}

function makeUser(item) {
  const row = document.createElement('div');
  row.className = 'user-row';
  const name = document.createElement('strong');
  name.textContent = item.username;
  const role = document.createElement('span');
  role.className = 'user-row__role';
  role.textContent = item.isAdmin ? 'Администратор' : 'Пользователь';
  const remove = document.createElement('button');
  remove.className = 'user-row__delete';
  remove.textContent = 'Удалить';
  remove.hidden = item.id === state.userId;
  remove.addEventListener('click', async () => {
    await api(`/api/admin/users/${item.id}`, { method: 'DELETE' });
    await loadAdmin();
  });
  const edit = document.createElement('button');
  edit.className = 'user-row__edit';
  edit.textContent = 'Изменить';
  edit.hidden = item.id === state.userId;
  edit.addEventListener('click', () => {
    elements.editUserForm.elements.id.value = item.id;
    elements.editUserForm.elements.username.value = item.username;
    elements.editUserForm.elements.password.value = '';
    elements.editUserModal.showModal();
  });
  row.append(name, role, edit, remove);
  return row;
}

async function restorePlayback(autoplay = false) {
  const playback = await api('/api/playback');
  if (!playback.queue.length) return;
  state.queue = await api(`/api/tracks?ids=${playback.queue.join(',')}`);
  state.queueIndex = Math.max(0, state.queue.findIndex((item) => item.id === playback.currentTrackId));
  const item = state.queue[state.queueIndex];
  if (!item) return;
  state.currentTrack = item;
  elements.audio.src = offline.mediaURL(item.id);
  elements.playerTitle.textContent = item.title;
  elements.playerArtist.textContent = item.artist || 'Неизвестный исполнитель';
  elements.coverImage.hidden = !item.hasCover;
  elements.coverPlaceholder.hidden = item.hasCover;
  elements.coverImage.src = item.hasCover ? offline.mediaURL(item.id, 'cover') : '';
  elements.audio.addEventListener('loadedmetadata', async () => {
    elements.audio.currentTime = playback.position;
    if (autoplay && playback.isPlaying && isOutputDevice()) await elements.audio.play().catch(() => {});
  }, { once: true });
  if (!isOutputDevice()) updateRemotePlayButton(playback.isPlaying);
}

function persistPlayback() {
  if (!state.currentTrack || !isOutputDevice()) return;
  api('/api/playback', {
    method: 'PUT',
    body: JSON.stringify({ currentTrackId: state.currentTrack.id, position: elements.audio.currentTime || 0, queue: state.queue.map((item) => item.id), isPlaying: !elements.audio.paused }),
  }).catch(() => {});
}

function updateRemotePlayButton(isPlaying) {
  elements.playButton.textContent = isPlaying ? '❚❚' : '▶';
}

function move(direction) {
  if (!state.queue.length) return;
  state.queueIndex = (state.queueIndex + direction + state.queue.length) % state.queue.length;
  playCurrent();
}

function updatePlayButton() {
  elements.playButton.textContent = elements.audio.paused ? '▶' : '❚❚';
}

function formatTime(seconds) {
  if (!Number.isFinite(seconds)) return '0:00';
  return `${Math.floor(seconds / 60)}:${String(Math.floor(seconds % 60)).padStart(2, '0')}`;
}

elements.loginForm.addEventListener('submit', async (event) => {
  event.preventDefault();
  const form = new FormData(elements.loginForm);
  elements.loginError.textContent = '';
  try {
    await api('/api/login', { method: 'POST', body: JSON.stringify(Object.fromEntries(form)) });
    await start();
  } catch (error) {
    elements.loginError.textContent = error.message;
  }
});

elements.logoutButton.addEventListener('click', async () => {
  await api('/api/logout', { method: 'POST' });
  elements.audio.pause();
  localStorage.removeItem('playerLastUser');
  offline.userId = null;
  showLogin();
});
elements.accountButton.addEventListener('click', () => {
  elements.accountForm.reset();
  elements.accountForm.elements.username.value = state.userName;
  elements.accountModal.showModal();
});
elements.scanButton.addEventListener('click', async () => {
  elements.scanButton.disabled = true;
  try {
    await api('/api/admin/scan', { method: 'POST' });
    await loadLibrary(state.currentPath);
  } finally {
    elements.scanButton.disabled = false;
  }
});
elements.searchInput.addEventListener('input', async () => {
  const query = elements.searchInput.value.trim();
  if (!query) {
    setActiveNavigation('home');
    await loadLibrary(state.currentPath);
    return;
  }
  setActiveNavigation('');
  const tracks = await api(`/api/search?q=${encodeURIComponent(query)}`);
  renderTrackView('Результаты поиска', tracks, 'search');
});
elements.playFolderButton.addEventListener('click', async () => {
  const tracks = state.currentView === 'library' ? await loadLibrary(state.currentPath, true) : state.visibleTracks;
  requestPlayQueue(tracks);
});
elements.sortSelect.addEventListener('change', () => {
  state.sort = elements.sortSelect.value;
  loadLibrary(state.currentPath);
});
elements.formatSelect.addEventListener('change', () => {
  state.format = elements.formatSelect.value.toLowerCase();
  loadLibrary(state.currentPath);
});
elements.playlistLayoutSelect.addEventListener('change', () => {
  state.playlistLayout = elements.playlistLayoutSelect.value;
  localStorage.setItem('playerPlaylistLayout', state.playlistLayout);
  loadPlaylists();
});
elements.menuButton.addEventListener('click', () => {
  if (innerWidth <= 760) document.querySelector('.sidebar').classList.toggle('sidebar--open');
  else {
    elements.app.classList.toggle('app--sidebar-collapsed');
    localStorage.setItem('playerSidebarCollapsed', elements.app.classList.contains('app--sidebar-collapsed'));
  }
});
document.querySelector('[data-action="home"]').addEventListener('click', () => {
  elements.searchInput.value = '';
  setActiveNavigation('home');
  if (offline.disconnected) { loadOffline(); return; }
  loadLibrary('');
});
document.querySelector('[data-action="favorites"]').addEventListener('click', async () => {
  setActiveNavigation('favorites');
  const tracks = (await api(`/api/search?q=${encodeURIComponent('%')}`)).filter((item) => item.favorite);
  renderTrackView('Избранное', tracks, 'favorites');
});
document.querySelector('[data-action="playlists"]').addEventListener('click', () => { setActiveNavigation('playlists'); loadPlaylists(); });
document.querySelector('[data-action="history"]').addEventListener('click', async () => { setActiveNavigation('history'); renderTrackView('История', await api('/api/history'), 'history'); });
document.querySelector('[data-action="notifications"]').addEventListener('click', () => { setActiveNavigation('notifications'); loadNotifications(); });
elements.sourcesButton.addEventListener('click', () => { setActiveNavigation('sources'); loadSources(); });
elements.filesButton.addEventListener('click', () => { setActiveNavigation('files'); loadFiles(); });
elements.operationsButton.addEventListener('click', () => { setActiveNavigation('operations'); loadOperations(); });
document.querySelector('[data-action="releases"]').addEventListener('click', () => { setActiveNavigation('releases'); loadReleases(); });
elements.adminButton.addEventListener('click', () => { setActiveNavigation('admin'); loadAdmin(); });

function donationStorageKey(suffix) {
  return `resonyrDonation${suffix}-${state.userId}`;
}

function postponeDonationReminder() {
  localStorage.setItem(donationStorageKey('LastShown'), String(Date.now()));
  elements.donationModal.close();
}

function showDonationReminder() {
  if (!state.userId || localStorage.getItem(donationStorageKey('Never')) === 'true') return;
  const lastShown = Number(localStorage.getItem(donationStorageKey('LastShown'))) || 0;
  if (Date.now() - lastShown < 7 * 24 * 60 * 60 * 1000) return;
  if (document.querySelector('dialog[open]')) {
    window.setTimeout(showDonationReminder, 30000);
    return;
  }
  localStorage.setItem(donationStorageKey('LastShown'), String(Date.now()));
  elements.donationModal.showModal();
}

let playlistCoverURL = '';

function setPlaylistCover(file) {
  const error = elements.playlistForm.querySelector('[data-form-error]');
  if (!file || !['image/jpeg', 'image/png'].includes(file.type)) {
    elements.playlistCoverInput.value = '';
    resetPlaylistCover();
    error.textContent = 'Выберите изображение JPEG или PNG';
    return;
  }
  const files = new DataTransfer();
  files.items.add(file);
  elements.playlistCoverInput.files = files.files;
  if (playlistCoverURL) URL.revokeObjectURL(playlistCoverURL);
  playlistCoverURL = URL.createObjectURL(file);
  elements.playlistCoverPreview.src = playlistCoverURL;
  elements.playlistCoverPicture.hidden = false;
  elements.playlistCoverPrompt.hidden = true;
  error.textContent = '';
}

function resetPlaylistCover() {
  if (playlistCoverURL) URL.revokeObjectURL(playlistCoverURL);
  playlistCoverURL = '';
  elements.playlistCoverPreview.removeAttribute('src');
  elements.playlistCoverPicture.hidden = true;
  elements.playlistCoverPrompt.hidden = false;
}

let trackCoverURL = '';

function resetTrackCover() {
  if (trackCoverURL) URL.revokeObjectURL(trackCoverURL);
  trackCoverURL = '';
  elements.trackCoverInput.value = '';
  elements.trackCoverPreview.removeAttribute('src');
  elements.trackCoverPicture.hidden = true;
  elements.trackCoverPrompt.hidden = false;
}

function setTrackCover(file) {
  const error = elements.trackMetadataForm.querySelector('[data-form-error]');
  if (!file || !['image/jpeg', 'image/png'].includes(file.type)) {
    resetTrackCover();
    error.textContent = 'Выберите изображение JPEG или PNG';
    return;
  }
  const files = new DataTransfer();
  files.items.add(file);
  elements.trackCoverInput.files = files.files;
  if (trackCoverURL) URL.revokeObjectURL(trackCoverURL);
  trackCoverURL = URL.createObjectURL(file);
  elements.trackCoverPreview.src = trackCoverURL;
  elements.trackCoverPicture.hidden = false;
  elements.trackCoverPrompt.hidden = true;
  elements.trackCoverRemoveButton.hidden = false;
  state.removeTrackCover = false;
  error.textContent = '';
}

function openTrackMetadata(item) {
  const form = elements.trackMetadataForm;
  form.reset();
  state.pendingTrackId = item.id;
  state.removeTrackCover = false;
  const values = {
    id: item.id, title: item.title, artists: item.artists?.join('; ') || item.artist,
    album: item.album, albumArtist: item.albumArtist, year: item.year || '',
    trackNumber: item.trackNumber || '', discNumber: item.discNumber || '', genre: item.genre,
    composer: item.composer, comment: item.comment,
  };
  Object.entries(values).forEach(([name, value]) => { form.elements[name].value = value || ''; });
  resetTrackCover();
  if (item.hasCover) {
    elements.trackCoverPreview.src = `/api/tracks/${item.id}/cover`;
    elements.trackCoverPicture.hidden = false;
    elements.trackCoverPrompt.hidden = true;
  }
  elements.trackCoverRemoveButton.hidden = !item.hasCover;
  elements.trackTechnicalInfo.textContent = [item.format?.toUpperCase(), item.duration ? formatTime(item.duration) : '', item.bitrate ? `${Math.round(item.bitrate / 1000)} kbps` : '', item.sampleRate ? `${item.sampleRate} Hz` : '', item.path].filter(Boolean).join(' · ');
  form.querySelector('[data-form-error]').textContent = '';
  elements.trackMetadataModal.showModal();
}

function openPlaylistForm(item = null) {
  elements.playlistForm.reset();
  resetPlaylistCover();
  elements.playlistForm.elements.id.value = item?.id || '';
  elements.playlistForm.elements.name.value = item?.name || '';
  elements.playlistForm.elements.description.value = item?.description || '';
  elements.playlistModalTitle.textContent = item ? 'Редактировать плейлист' : 'Новый плейлист';
  elements.playlistSubmitButton.textContent = item ? 'Сохранить' : 'Создать';
  elements.playlistForm.querySelector('[data-form-error]').textContent = '';
  if (item?.hasCover) {
    elements.playlistCoverPreview.src = `/api/playlists/${item.id}/cover`;
    elements.playlistCoverPicture.hidden = false;
    elements.playlistCoverPrompt.hidden = true;
  }
  elements.playlistModal.showModal();
}

elements.newPlaylistButton.addEventListener('click', () => openPlaylistForm());
elements.editPlaylistButton.addEventListener('click', () => openPlaylistForm(state.currentPlaylist));
elements.playlistCoverInput.addEventListener('change', () => setPlaylistCover(elements.playlistCoverInput.files[0]));
['dragenter', 'dragover'].forEach((eventName) => elements.playlistCoverDropzone.addEventListener(eventName, (event) => {
  event.preventDefault();
  elements.playlistCoverDropzone.classList.add('cover-upload--active');
}));
['dragleave', 'drop'].forEach((eventName) => elements.playlistCoverDropzone.addEventListener(eventName, (event) => {
  event.preventDefault();
  elements.playlistCoverDropzone.classList.remove('cover-upload--active');
}));
elements.playlistCoverDropzone.addEventListener('drop', (event) => setPlaylistCover(event.dataTransfer.files[0]));
elements.trackCoverInput.addEventListener('change', () => setTrackCover(elements.trackCoverInput.files[0]));
['dragenter', 'dragover'].forEach((eventName) => elements.trackCoverDropzone.addEventListener(eventName, (event) => {
  event.preventDefault();
  elements.trackCoverDropzone.classList.add('cover-upload--active');
}));
['dragleave', 'drop'].forEach((eventName) => elements.trackCoverDropzone.addEventListener(eventName, (event) => {
  event.preventDefault();
  elements.trackCoverDropzone.classList.remove('cover-upload--active');
}));
elements.trackCoverDropzone.addEventListener('drop', (event) => setTrackCover(event.dataTransfer.files[0]));
elements.trackCoverRemoveButton.addEventListener('click', () => {
  resetTrackCover();
  state.removeTrackCover = true;
  elements.trackCoverRemoveButton.hidden = true;
});
elements.uploadMusicButton.addEventListener('click', () => {
  elements.musicPath.value = '';
  elements.musicModal.showModal();
});
elements.uploadFilesButton.addEventListener('click', () => {
  elements.musicPath.value = state.currentFilePath;
  elements.musicModal.showModal();
});
elements.uploadFolderButton.addEventListener('click', () => elements.folderInput.click());
elements.fileDropzone.addEventListener('click', () => elements.folderInput.click());
elements.fileDropzone.addEventListener('keydown', (event) => {
  if (event.key === 'Enter' || event.key === ' ') {
    event.preventDefault();
    elements.folderInput.click();
  }
});
elements.folderInput.addEventListener('change', async () => {
  const items = Array.from(elements.folderInput.files).filter((file) => isAudioFile(file.name)).map((file) => ({ file, path: file.webkitRelativePath || file.name }));
  try {
    await uploadMusicTree(items);
  } catch (error) {
    window.alert(error.message);
  } finally {
    elements.folderInput.value = '';
  }
});
['dragenter', 'dragover'].forEach((type) => elements.fileDropzone.addEventListener(type, (event) => {
  event.preventDefault();
  elements.fileDropzone.classList.add('file-dropzone--active');
}));
elements.fileDropzone.addEventListener('dragleave', (event) => {
  if (!elements.fileDropzone.contains(event.relatedTarget)) elements.fileDropzone.classList.remove('file-dropzone--active');
});
elements.fileDropzone.addEventListener('drop', async (event) => {
  event.preventDefault();
  try {
    const path = draggedFilePath(event.dataTransfer);
    if (path) await moveFile(path, state.currentFilePath);
    else await uploadMusicTree(await droppedMusic(event.dataTransfer));
  } catch (error) {
    window.alert(error.message);
  } finally {
    elements.fileDropzone.classList.remove('file-dropzone--active');
  }
});
elements.newFolderButton.addEventListener('click', () => elements.folderModal.showModal());
elements.newUserButton.addEventListener('click', () => elements.userModal.showModal());
elements.uploadThemeButton.addEventListener('click', () => elements.themeModal.showModal());
elements.newSourceButton.addEventListener('click', () => {
  elements.sourceForm.reset();
  elements.sourceModal.showModal();
});
elements.newBackupButton.addEventListener('click', async () => { elements.newBackupButton.disabled = true; try { await api('/api/admin/backups', { method: 'POST' }); await loadOperations(); } finally { elements.newBackupButton.disabled = false; } });
elements.watchArtistButton.addEventListener('click', () => {
  elements.artistResults.replaceChildren();
  elements.artistModal.showModal();
});
elements.deletePlaylistButton.addEventListener('click', async () => {
  await api(`/api/playlists/${state.currentPlaylist.id}`, { method: 'DELETE' });
  await loadPlaylists();
});
elements.clearNotificationsButton.addEventListener('click', async () => {
  await api('/api/notifications', { method: 'DELETE' });
  await loadNotifications();
});
elements.removePlaylistDownloadsButton.addEventListener('click', () => removePlaylistDownloads(state.currentPlaylist).catch((error) => offline.notify(error.message)));
elements.clearDownloadsButton.addEventListener('click', async () => {
  if (!window.confirm('Удалить все скачанные песни с этого устройства? Плейлисты, избранное и музыка на сервере останутся.')) return;
  try {
    await removeOfflineTracks(offlineTracks().map((track) => track.id));
    if (state.currentView === 'playlists') await loadPlaylists();
  } catch (error) { offline.notify(error.message); }
});
elements.enablePushButton.addEventListener('click', () => enablePush().catch((error) => { elements.enablePushButton.textContent = error.message; }));
document.querySelectorAll('[data-close]').forEach((button) => button.addEventListener('click', () => elements[button.dataset.close].close()));
elements.playlistForm.addEventListener('submit', async (event) => {
  event.preventDefault();
  const form = new FormData(elements.playlistForm);
  const error = elements.playlistForm.querySelector('[data-form-error]');
  const editingId = Number(form.get('id')) || 0;
  let createdItem;
  error.textContent = '';
  elements.playlistSubmitButton.disabled = true;
  try {
    const details = JSON.stringify({ name: form.get('name'), description: form.get('description') });
    if (editingId) await api(`/api/playlists/${editingId}`, { method: 'PUT', body: details });
    else createdItem = await api('/api/playlists', { method: 'POST', body: details });
    const playlistId = editingId || createdItem.id;
    const cover = form.get('cover');
    if (cover?.size) {
      const upload = new FormData();
      upload.set('cover', cover);
      await api(`/api/playlists/${playlistId}/cover`, { method: 'PUT', body: upload });
    }
    elements.playlistModal.close();
    elements.playlistForm.reset();
    resetPlaylistCover();
    if (editingId) {
      const updatedItem = (await api('/api/playlists')).find((item) => item.id === editingId);
      if (updatedItem) await openPlaylist(updatedItem);
      else await loadPlaylists();
    } else await loadPlaylists();
  } catch (requestError) {
    if (createdItem) await api(`/api/playlists/${createdItem.id}`, { method: 'DELETE' }).catch(() => {});
    error.textContent = requestError.message;
  } finally {
    elements.playlistSubmitButton.disabled = false;
  }
});
elements.trackMetadataForm.addEventListener('submit', async (event) => {
  event.preventDefault();
  const form = new FormData(elements.trackMetadataForm);
  const error = elements.trackMetadataForm.querySelector('[data-form-error]');
  const submit = elements.trackMetadataForm.querySelector('[type="submit"]');
  form.set('removeCover', String(state.removeTrackCover));
  error.textContent = '';
  submit.disabled = true;
  try {
    const result = await api(`/api/admin/tracks/${state.pendingTrackId}`, { method: 'PUT', body: form });
    const coverChanged = result.coverChanged;
    delete result.coverChanged;
    if (!coverChanged) delete result.hasCover;
    state.visibleTracks.forEach((item) => { if (item.id === result.id) Object.assign(item, result); });
    state.queue.forEach((item) => { if (item.id === result.id) Object.assign(item, result); });
    if (state.currentTrack?.id === result.id) {
      Object.assign(state.currentTrack, result);
      elements.playerTitle.textContent = result.title;
      elements.playerArtist.textContent = result.artist || 'Неизвестный исполнитель';
      elements.coverImage.hidden = !state.currentTrack.hasCover;
      elements.coverPlaceholder.hidden = state.currentTrack.hasCover;
      elements.coverImage.src = state.currentTrack.hasCover ? `/api/tracks/${result.id}/cover?v=${Date.now()}` : '';
    }
    renderTracks(state.visibleTracks);
    elements.trackMetadataModal.close();
    resetTrackCover();
  } catch (requestError) {
    error.textContent = requestError.message;
  } finally {
    submit.disabled = false;
  }
});
elements.musicForm.addEventListener('submit', async (event) => {
  event.preventDefault();
  const error = elements.musicForm.querySelector('[data-form-error]');
  const submit = elements.musicForm.querySelector('[type="submit"], .button--primary');
  error.textContent = '';
  submit.disabled = true;
  try {
    const uploadPath = elements.musicPath.value;
    const result = await api(`/api/admin/files/upload?path=${encodeURIComponent(uploadPath)}`, { method: 'POST', body: new FormData(elements.musicForm) });
    elements.musicModal.close();
    elements.musicForm.reset();
    if (state.currentView === 'files') await loadFiles(state.currentFilePath);
    else {
      await loadLibrary('');
      elements.libraryTitle.textContent = `Загружено файлов: ${result.uploaded}`;
    }
  } catch (requestError) {
    error.textContent = requestError.message;
  } finally {
    submit.disabled = false;
  }
});
elements.folderForm.addEventListener('submit', async (event) => {
  event.preventDefault();
  const error = elements.folderForm.querySelector('[data-form-error]');
  error.textContent = '';
  try {
    await api('/api/admin/files/folders', { method: 'POST', body: JSON.stringify({ path: state.currentFilePath, name: elements.folderForm.elements.name.value }) });
    elements.folderModal.close();
    elements.folderForm.reset();
    await loadFiles(state.currentFilePath);
  } catch (requestError) {
    error.textContent = requestError.message;
  }
});
elements.renameFileForm.addEventListener('submit', async (event) => {
  event.preventDefault();
  const form = new FormData(elements.renameFileForm);
  const error = elements.renameFileForm.querySelector('[data-form-error]');
  error.textContent = '';
  try {
    await api(`/api/admin/files?path=${encodeURIComponent(form.get('path'))}`, { method: 'PUT', body: JSON.stringify({ name: form.get('name') }) });
    elements.renameFileModal.close();
    await loadFiles(state.currentFilePath);
  } catch (requestError) {
    error.textContent = requestError.message;
  }
});
elements.userForm.addEventListener('submit', async (event) => {
  event.preventDefault();
  const form = new FormData(elements.userForm);
  await api('/api/admin/users', { method: 'POST', body: JSON.stringify({ username: form.get('username'), password: form.get('password'), isAdmin: form.has('isAdmin') }) });
  elements.userModal.close();
  elements.userForm.reset();
  await loadAdmin();
});
elements.accountForm.addEventListener('submit', async (event) => {
  event.preventDefault();
  const error = elements.accountForm.querySelector('[data-form-error]');
  error.textContent = '';
  try {
    const result = await api('/api/account', { method: 'PUT', body: JSON.stringify(Object.fromEntries(new FormData(elements.accountForm))) });
    state.userName = result.username;
    elements.username.textContent = result.username;
    elements.accountModal.close();
  } catch (requestError) {
    error.textContent = requestError.message;
  }
});
elements.editUserForm.addEventListener('submit', async (event) => {
  event.preventDefault();
  const form = new FormData(elements.editUserForm);
  const error = elements.editUserForm.querySelector('[data-form-error]');
  error.textContent = '';
  try {
    await api(`/api/admin/users/${form.get('id')}`, { method: 'PUT', body: JSON.stringify({ username: form.get('username'), password: form.get('password') }) });
    elements.editUserModal.close();
    await loadAdmin();
  } catch (requestError) {
    error.textContent = requestError.message;
  }
});
elements.themeForm.addEventListener('submit', async (event) => {
  event.preventDefault();
  await api('/api/admin/themes', { method: 'POST', body: new FormData(elements.themeForm) });
  elements.themeModal.close();
  elements.themeForm.reset();
  await loadThemes();
});
elements.sourceForm.addEventListener('submit', async (event) => {
  event.preventDefault();
  const error = elements.sourceForm.querySelector('[data-form-error]');
  error.textContent = '';
  try {
    const result = await api('/api/admin/sources/oauth/start', { method: 'POST', body: JSON.stringify(Object.fromEntries(new FormData(elements.sourceForm))) });
    location.assign(result.url);
  } catch (requestError) {
    error.textContent = requestError.message;
  }
});
elements.donationLaterButton.addEventListener('click', postponeDonationReminder);
elements.donationNeverButton.addEventListener('click', () => {
  localStorage.setItem(donationStorageKey('Never'), 'true');
  elements.donationModal.close();
});
elements.donationModal.querySelector('.donation__link').addEventListener('click', postponeDonationReminder);
elements.supportLink.addEventListener('click', () => localStorage.setItem(donationStorageKey('LastShown'), String(Date.now())));
elements.artistForm.addEventListener('submit', async (event) => {
  event.preventDefault();
  const error = elements.artistForm.querySelector('[data-form-error]');
  error.textContent = '';
  try {
    const query = new FormData(elements.artistForm).get('name');
    const items = await api(`/api/artists/search?q=${encodeURIComponent(query)}`);
    elements.artistResults.replaceChildren(...items.map((item) => {
      const card = document.createElement('article');
      card.className = 'artist-card';
      const picture = document.createElement('picture');
      picture.className = 'artist-card__picture';
      const image = document.createElement('img');
      image.className = 'artist-card__image';
      image.src = item.image;
      image.alt = '';
      image.addEventListener('error', () => { image.src = '/icon.svg'; }, { once: true });
      picture.append(image);
      const info = document.createElement('span');
      info.className = 'artist-card__info';
      const name = document.createElement('strong');
      name.className = 'artist-card__name';
      name.textContent = item.name;
      const meta = document.createElement('small');
      meta.className = 'artist-card__meta';
      const years = [item.begin, item.end].filter(Boolean).join('–');
      meta.textContent = [item.disambiguation, item.type, item.area || item.country, years].filter(Boolean).join(' · ') || `Совпадение ${item.score}%`;
      info.append(name, meta);
      const add = document.createElement('button');
      add.className = 'button button--primary';
      add.type = 'button';
      add.textContent = 'Выбрать';
      add.addEventListener('click', async () => {
        add.disabled = true;
        try {
          await api('/api/watched-artists', { method: 'POST', body: JSON.stringify({ mbid: item.mbid }) });
          elements.artistModal.close();
          elements.artistForm.reset();
          await loadReleases();
        } catch (requestError) {
          error.textContent = requestError.message;
          add.disabled = false;
        }
      });
      card.append(picture, info, add);
      return card;
    }));
    if (!items.length) error.textContent = 'Исполнители не найдены';
  } catch (requestError) {
    error.textContent = requestError.message;
  }
});
elements.themeSelect.addEventListener('change', async () => {
  await api('/api/settings/theme', { method: 'PUT', body: JSON.stringify({ theme: elements.themeSelect.value }) });
  applyTheme(elements.themeSelect.value);
});
elements.colorModeSelect.addEventListener('change', async () => {
  await api('/api/settings/color-mode', { method: 'PUT', body: JSON.stringify({ colorMode: elements.colorModeSelect.value }) });
  applyColorMode(elements.colorModeSelect.value);
});
elements.deviceSelect.addEventListener('change', async () => {
  await api('/api/devices/active', { method: 'PUT', body: JSON.stringify({ id: elements.deviceSelect.value }) });
});
elements.playButton.addEventListener('click', () => sendControl('toggle'));
elements.previousButton.addEventListener('click', () => sendControl('previous'));
elements.nextButton.addEventListener('click', () => sendControl('next'));
elements.shuffleButton.addEventListener('click', () => {
  state.shuffle = !state.shuffle;
  elements.shuffleButton.classList.toggle('icon-button--active', state.shuffle);
});
elements.repeatButton.addEventListener('click', () => {
  state.repeat = (state.repeat + 1) % 3;
  elements.repeatButton.textContent = state.repeat === 1 ? '↻₁' : '↻';
  elements.repeatButton.classList.toggle('icon-button--active', state.repeat > 0);
});
elements.audio.addEventListener('play', () => { updatePlayButton(); persistPlayback(); });
elements.audio.addEventListener('pause', () => { updatePlayButton(); persistPlayback(); });
elements.audio.addEventListener('timeupdate', () => {
  elements.currentTime.textContent = formatTime(elements.audio.currentTime);
  elements.duration.textContent = formatTime(elements.audio.duration);
  elements.progressRange.value = elements.audio.duration ? (elements.audio.currentTime / elements.audio.duration) * 100 : 0;
  if (Date.now() - state.lastSavedAt > 5000) {
    state.lastSavedAt = Date.now();
    persistPlayback();
  }
});
elements.audio.addEventListener('ended', () => {
  if (state.repeat === 1) playCurrent();
  else if (state.queueIndex < state.queue.length - 1 || state.repeat === 2) move(1);
});
elements.progressRange.addEventListener('input', () => {
  if (elements.audio.duration) sendControl('seek', elements.audio.duration * Number(elements.progressRange.value) / 100);
});
const savedVolume = Number(localStorage.getItem('resonyrVolume'));
const initialVolume = Number.isFinite(savedVolume) && savedVolume >= 0 && savedVolume <= 1 ? savedVolume : 0.8;
let lastVolume = initialVolume > 0 ? initialVolume : 0.8;

function setVolume(value) {
  const volume = Math.max(0, Math.min(1, Number(value)));
  elements.audio.volume = volume;
  elements.volumeRange.value = String(volume);
  elements.volumeRange.style.setProperty('--volume-level', `${volume * 100}%`);
  elements.volumeWaveSmall.hidden = volume === 0;
  elements.volumeWaveLarge.hidden = volume < 0.5;
  elements.volumeButton.ariaLabel = volume === 0 ? 'Включить звук' : 'Выключить звук';
  if (volume > 0) lastVolume = volume;
  localStorage.setItem('resonyrVolume', String(volume));
}

elements.volumeRange.addEventListener('input', () => setVolume(elements.volumeRange.value));
elements.volumeButton.addEventListener('click', () => setVolume(elements.audio.volume > 0 ? 0 : lastVolume));
setVolume(initialVolume);

if ('mediaSession' in navigator) {
  navigator.mediaSession.setActionHandler('play', () => sendControl('play'));
  navigator.mediaSession.setActionHandler('pause', () => sendControl('pause'));
  navigator.mediaSession.setActionHandler('previoustrack', () => sendControl('previous'));
  navigator.mediaSession.setActionHandler('nexttrack', () => sendControl('next'));
}

if ('serviceWorker' in navigator) {
  let serviceWorkerRefreshing = false;
  const hadController = Boolean(navigator.serviceWorker.controller);
  navigator.serviceWorker.addEventListener('controllerchange', () => {
    if (!hadController || serviceWorkerRefreshing) return;
    serviceWorkerRefreshing = true;
    location.reload();
  });
  navigator.serviceWorker.register('/service-worker.js').catch(() => {});
}
window.addEventListener('offline', () => { offline.setDisconnected(true); startOffline(); });
window.addEventListener('online', () => offline.sync());
window.addEventListener('resonyr-auth', () => showLogin());
window.addEventListener('resonyr-connection', () => {
  if (offline.disconnected && state.userId) {
    state.activeDeviceId = state.deviceId;
    state.deviceStream?.close();
  }
  if (!offline.disconnected && state.userId && offline.restoredUser) {
    state.isAdmin = offline.restoredUser.isAdmin;
    for (const id of ['scanButton', 'adminButton', 'sourcesButton', 'filesButton', 'operationsButton']) elements[id].hidden = !state.isAdmin;
    setupDevice().catch(() => {});
  }
});
setInterval(() => offline.sync(), 15000);
elements.audio.addEventListener('error', () => offline.notify('Не удалось воспроизвести трек. Без связи доступны только полностью скачанные песни.'));
start();

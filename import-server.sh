#!/bin/sh
set -eu

compose() {
  if docker compose version >/dev/null 2>&1; then docker compose "$@"
  else docker-compose "$@"
  fi
}

archive="${1:-}"
if [ ! -f "$archive" ]; then
  printf 'Использование: ./import-server.sh resonyr-transfer-*.tar.gz\n' >&2
  exit 1
fi
transferDir="$(mktemp -d)"
trap 'rm -rf "$transferDir"' EXIT
if tar -tzf "$archive" | grep -Ev '^(\./)?(server\.env|database\.dump|themes\.tar\.gz|uploads\.tar\.gz|music\.tar\.gz)?$' | grep -q .; then
  printf 'Архив содержит посторонние пути.\n' >&2
  exit 1
fi
tar -C "$transferDir" -xzf "$archive"
if [ ! -f "$transferDir/server.env" ] || [ ! -f "$transferDir/database.dump" ]; then
  printf 'Архив переноса повреждён.\n' >&2
  exit 1
fi
if [ -f .env ]; then cp .env ".env.before-import-$(date -u +%Y%m%d-%H%M%S)"; fi
cp "$transferDir/server.env" .env
chmod 600 .env
musicPath="$(sed -n 's/^MUSIC_PATH=//p' .env)"
uploadsPath="$(sed -n 's/^UPLOADS_PATH=//p' .env)"
musicPath="${musicPath:-./music}"
uploadsPath="${uploadsPath:-./uploads}"
case "$musicPath" in /|.|..|"${HOME:-/nonexistent}") printf 'Опасный MUSIC_PATH в архиве.\n' >&2; exit 1 ;; esac
case "$uploadsPath" in /|.|..|"${HOME:-/nonexistent}") printf 'Опасный UPLOADS_PATH в архиве.\n' >&2; exit 1 ;; esac
mkdir -p "$musicPath" "$uploadsPath"
if [ -f "$transferDir/music.tar.gz" ]; then tar -C "$musicPath" -xzf "$transferDir/music.tar.gz"; fi
if [ -f "$transferDir/uploads.tar.gz" ]; then tar -C "$uploadsPath" -xzf "$transferDir/uploads.tar.gz"; fi
./install.sh
RESTORE_CONFIRM=YES ./restore.sh "$transferDir/database.dump"
if [ -s "$transferDir/themes.tar.gz" ]; then gzip -dc "$transferDir/themes.tar.gz" | compose exec -T app tar -C /data -xf -; fi
printf 'Перенос завершён. Перезапустите Resonyr: docker compose restart app\n'

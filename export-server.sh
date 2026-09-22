#!/bin/sh
set -eu

compose() {
  if docker compose version >/dev/null 2>&1; then docker compose "$@"
  else docker-compose "$@"
  fi
}

archive="${1:-resonyr-transfer-$(date -u +%Y%m%d-%H%M%S).tar.gz}"
transferDir="$(mktemp -d)"
trap 'rm -rf "$transferDir"' EXIT
cp .env "$transferDir/server.env"
compose exec -T postgres pg_dump -U player -d player --format=custom > "$transferDir/database.dump"
compose exec -T app sh -c 'if [ -d /data/themes ]; then tar -C /data -czf - themes; else tar -czf - -T /dev/null; fi' > "$transferDir/themes.tar.gz"
uploadsPath="$(sed -n 's/^UPLOADS_PATH=//p' .env)"
uploadsPath="${uploadsPath:-./uploads}"
if [ -d "$uploadsPath" ]; then tar -C "$uploadsPath" -czf "$transferDir/uploads.tar.gz" .; fi
if [ "${INCLUDE_MUSIC:-NO}" = "YES" ]; then
  musicPath="$(sed -n 's/^MUSIC_PATH=//p' .env)"
  musicPath="${musicPath:-./music}"
  tar -C "$musicPath" -czf "$transferDir/music.tar.gz" .
fi
tar -C "$transferDir" -czf "$archive" .
chmod 600 "$archive"
printf 'Архив переноса создан: %s\nОн содержит секреты сервера — храните и передавайте его защищённо.\n' "$archive"

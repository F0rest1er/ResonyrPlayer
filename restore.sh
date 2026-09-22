#!/bin/sh
set -eu

backupFile="${1:-}"
if [ ! -f "$backupFile" ] || [ "${RESTORE_CONFIRM:-}" != "YES" ]; then
  printf 'Использование: RESTORE_CONFIRM=YES ./restore.sh /path/to/player.dump\n' >&2
  exit 1
fi

if docker compose version >/dev/null 2>&1; then
  docker compose exec -T postgres pg_restore --clean --if-exists -U player -d player < "$backupFile"
else
  docker-compose exec -T postgres pg_restore --clean --if-exists -U player -d player < "$backupFile"
fi

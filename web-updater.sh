#!/bin/sh
set -eu
cd "$(dirname "$0")"

compose() {
  if docker compose version >/dev/null 2>&1; then docker compose "$@"
  else docker-compose "$@"
  fi
}
writeState() {
  compose exec -T app sh -c 'printf "%s" "$1" > /data/web-update/status' sh "$1"
}
finishJob() {
  writeState "$1"
  compose exec -T app sh -c 'rm -f /data/web-update/job/ready; rmdir /data/web-update/job'
}

mkdir .web-updater-lock 2>/dev/null || { printf 'Процесс уже запущен. После аварийной остановки удалите пустую .web-updater-lock.\n' >&2; exit 1; }
trap 'rmdir .web-updater-lock' EXIT
trap 'exit 0' INT TERM
printf 'Обновления из веб-интерфейса включены, пока этот процесс работает.\n'
while :; do
  if ! compose exec -T app sh -c 'mkdir -p /data/web-update; touch /data/web-update/heartbeat'; then
    sleep 5
    continue
  fi
  requestedTag="$(compose exec -T app sh -c 'head -c 81 /data/web-update/job/ready 2>/dev/null' || true)"
  if [ -n "$requestedTag" ]; then
    if [ "${#requestedTag}" -gt 80 ] || [ "$requestedTag" != "$(printf '%s' "$requestedTag" | LC_ALL=C tr -cd 'A-Za-z0-9.-')" ] || ! printf '%s\n' "$requestedTag" | LC_ALL=C grep -Eq '^v?[0-9]+\.[0-9]+\.[0-9]+([.-][A-Za-z0-9.-]+)?$'; then
      finishJob failed
      continue
    fi
    repository="$(compose exec -T app printenv UPDATE_REPOSITORY)"
    origin="$(git remote get-url origin)"
    case "$origin" in
      "https://github.com/$repository"|"https://github.com/$repository.git"|"git@github.com:$repository.git") ;;
      *) printf 'origin не совпадает с UPDATE_REPOSITORY.\n' >&2; finishJob failed; continue ;;
    esac
    writeState installing
    if sh ./update.sh "$requestedTag"; then
      finishJob completed
    else
      printf 'Обновление не завершено. Проверьте ошибки выше; музыка и база не удалялись.\n' >&2
      finishJob failed || exit 1
    fi
  fi
  sleep 5
done

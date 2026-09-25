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
  compose exec -T app sh -c 'rm -f /data/web-update/job/ready /data/web-update/job/restore.dump; rmdir /data/web-update/job'
}

restoreDatabase() {
  backupPath="$(mktemp)"
  if ! compose exec -T app cat /data/web-update/job/restore.dump > "$backupPath"; then
    rm -f "$backupPath"
    return 1
  fi
  restoreActive=true
  if ! compose stop app; then rm -f "$backupPath"; return 1; fi
  restoreResult=0
  compose exec -T postgres pg_restore --single-transaction --exit-on-error --clean --if-exists --no-owner --no-privileges -U player -d player < "$backupPath" || restoreResult=1
  rm -f "$backupPath"
  compose start app || return 1
  restoreActive=false
  attempt=0
  while [ "$attempt" -lt 60 ]; do
    if compose exec -T app wget -q --spider http://127.0.0.1:8080/health; then return "$restoreResult"; fi
    attempt=$((attempt + 1))
    sleep 2
  done
  return 1
}

cleanup() {
  if [ "${restoreActive:-false}" = true ]; then compose start app || true; fi
  if [ -n "${backupPath:-}" ] && [ -f "$backupPath" ]; then rm -f "$backupPath"; fi
  rmdir .web-updater-lock
}

mkdir .web-updater-lock 2>/dev/null || { printf 'Процесс уже запущен. После аварийной остановки удалите пустую .web-updater-lock.\n' >&2; exit 1; }
trap cleanup EXIT
trap 'exit 0' INT TERM
printf 'Обновления из веб-интерфейса включены, пока этот процесс работает.\n'
while :; do
  if ! compose exec -T app sh -c 'mkdir -p /data/web-update; touch /data/web-update/heartbeat'; then
    sleep 5
    continue
  fi
  requestedTag="$(compose exec -T app sh -c 'head -c 81 /data/web-update/job/ready 2>/dev/null' || true)"
  if [ -n "$requestedTag" ]; then
    if [ "$requestedTag" = restore ]; then
      writeState restoring
      if restoreDatabase; then finishJob restored; else finishJob failed || exit 1; fi
      continue
    fi
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
    if output="$(sh ./update.sh "$requestedTag" 2>&1)"; then
      finishJob completed
    else
      printf '%s\n' "$output" >&2
      lastLine="$(printf '%s\n' "$output" | tail -n 3 | tr '\n' ' ' | cut -c 1-200)"
      compose exec -T app sh -c 'printf "%s" "$1" > /data/web-update/error' sh "$lastLine"
      finishJob failed || exit 1
    fi
  fi
  sleep 5
done

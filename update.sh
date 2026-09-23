#!/bin/sh
set -eu

compose() {
  if docker compose version >/dev/null 2>&1; then docker compose "$@"
  else docker-compose "$@"
  fi
}

update() {

latestTag="${1:-}"
if [ -n "$latestTag" ]; then
  if [ "${#latestTag}" -gt 80 ] || [ "$latestTag" != "$(printf '%s' "$latestTag" | LC_ALL=C tr -cd 'A-Za-z0-9.-')" ] || ! printf '%s\n' "$latestTag" | LC_ALL=C grep -Eq '^v?[0-9]+\.[0-9]+\.[0-9]+([.-][A-Za-z0-9.-]+)?$'; then
    printf 'Некорректный тег релиза.\n' >&2
    exit 1
  fi
  git reset --hard HEAD
  git fetch -f origin "refs/tags/$latestTag:refs/tags/$latestTag"
  git checkout -f --detach "$latestTag"
else
  git reset --hard HEAD
  git fetch --tags -f origin
  latestTag="$(git tag --sort=-version:refname | sed -n '1p')"
  if [ -n "$latestTag" ]; then
    git checkout -f --detach "$latestTag"
  else
    git fetch origin main
    git checkout -f --detach origin/main
    latestTag="$(git rev-parse --short HEAD)"
  fi
fi
temporaryEnv="$(mktemp)"
awk -v version="$latestTag" 'BEGIN { found=0 } /^VERSION=/ { print "VERSION=" version; found=1; next } { print } END { if (!found) print "VERSION=" version }' .env > "$temporaryEnv"
mv "$temporaryEnv" .env
chmod 600 .env
sh ./install.sh
attempt=0
while [ "$attempt" -lt 60 ]; do
  if compose exec -T app wget -q --spider http://127.0.0.1:8080/health; then
    printf 'Resonyr обновлён до %s.\n' "$latestTag"
    exit 0
  fi
  attempt=$((attempt + 1))
  sleep 2
done
printf 'Новая версия не прошла проверку работоспособности. Проверьте docker compose logs app.\n' >&2
exit 1
}
update "$@"

#!/bin/sh
set -eu

git fetch --tags --prune
if ! git diff --quiet || ! git diff --cached --quiet; then
  printf 'Есть несохранённые изменения. Сначала сохраните их или выполните обновление вручную.\n' >&2
  exit 1
fi
latestTag="$(git tag --sort=-version:refname | sed -n '1p')"
if [ -z "$latestTag" ]; then
  printf 'В репозитории пока нет релизов.\n' >&2
  exit 1
fi
git checkout --detach "$latestTag"
temporaryEnv="$(mktemp)"
awk -v version="$latestTag" 'BEGIN { found=0 } /^VERSION=/ { print "VERSION=" version; found=1; next } { print } END { if (!found) print "VERSION=" version }' .env > "$temporaryEnv"
mv "$temporaryEnv" .env
chmod 600 .env
./install.sh
printf 'Resonyr обновлён до %s.\n' "$latestTag"

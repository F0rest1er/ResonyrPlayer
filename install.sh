#!/bin/sh
set -eu

compose() {
  if docker compose version >/dev/null 2>&1; then docker compose "$@"
  else docker-compose "$@"
  fi
}

if [ ! -f .env ]; then
  adminPassword="$(openssl rand -hex 12)"
  postgresPassword="$(openssl rand -hex 24)"
  configuredDomain="${DOMAIN:-}"
  if [ -n "$configuredDomain" ] && ! printf '%s' "$configuredDomain" | awk '
    length($0) > 253 || $0 !~ /\./ { exit 1 }
    { count = split($0, labels, "."); for (labelIndex = 1; labelIndex <= count; labelIndex++) if (length(labels[labelIndex]) > 63 || labels[labelIndex] !~ /^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?$/) exit 1 }
  '; then
    printf 'Некорректный домен: %s. Укажите имя без http://, пути и порта.\n' "$configuredDomain" >&2
    exit 1
  fi
  configuredDomain="$(printf '%s' "$configuredDomain" | tr '[:upper:]' '[:lower:]')"
  configuredRepository="${UPDATE_REPOSITORY:-}"
  configuredVersion="${VERSION:-dev}"
  if [ -n "$configuredDomain" ]; then
    publicURL="https://$configuredDomain"
    appBind="127.0.0.1"
    secureCookies="true"
  else
    publicURL="http://localhost:8080"
    appBind="0.0.0.0"
    secureCookies="false"
  fi
  umask 077
  printf '%s\n' \
    'PORT=8080' \
    "APP_BIND=$appBind" \
    "DOMAIN=$configuredDomain" \
    "PUBLIC_URL=$publicURL" \
    'MUSIC_PATH=./music' \
    'UPLOADS_PATH=./uploads' \
    'ADMIN_USERNAME=admin' \
    "ADMIN_PASSWORD=$adminPassword" \
    'SCAN_INTERVAL=15m' \
    'BACKUP_INTERVAL=24h' \
    "UPDATE_REPOSITORY=$configuredRepository" \
    "VERSION=$configuredVersion" \
    "SECURE_COOKIES=$secureCookies" \
    'GOOGLE_CLIENT_ID=' \
    'GOOGLE_CLIENT_SECRET=' \
    'YANDEX_CLIENT_ID=' \
    'YANDEX_CLIENT_SECRET=' \
    'DROPBOX_CLIENT_ID=' \
    'DROPBOX_CLIENT_SECRET=' \
    'ONEDRIVE_CLIENT_ID=' \
    'ONEDRIVE_CLIENT_SECRET=' \
    "POSTGRES_PASSWORD=$postgresPassword" \
    "APP_SECRET=$(openssl rand -hex 32)" \
    'TZ=Europe/Moscow' > .env
  freshInstall=true
fi

if ! grep -q '^APP_SECRET=' .env; then
  umask 077
  printf 'APP_SECRET=%s\n' "$(openssl rand -hex 32)" >> .env
fi

mkdir -p music uploads
if grep -q '^DOMAIN=..' .env; then
  compose --profile domain up -d --build
else
  compose up -d --build
fi

if [ "${freshInstall:-false}" = true ]; then
  if [ -n "${configuredDomain:-}" ]; then address="https://$configuredDomain"
  else address="http://IP_СЕРВЕРА:8080"
  fi
  printf '\nResonyr установлен: %s\nЛогин: admin\nПароль: %s\nВажно: войдите и сразу смените логин и пароль в настройках аккаунта.\n' "$address" "$adminPassword"
fi

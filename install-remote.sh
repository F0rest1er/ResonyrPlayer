#!/bin/sh
set -eu

repository="${PLAYER_REPOSITORY:-https://github.com/F0rest1er/ResonyrPlayer.git}"
domain="${DOMAIN:-}"
installDir="${PLAYER_INSTALL_DIR:-$PWD/player}"

if [ -n "${1:-}" ]; then
  case "$1" in
    *github.com*|*.git)
      repository="$1"
      domain="${2:-$domain}"
      ;;
    *)
      domain="$1"
      ;;
  esac
fi

installPackage() {
  if command -v apt-get >/dev/null 2>&1; then
    sudo apt-get update
    sudo apt-get install -y "$@"
  elif command -v dnf >/dev/null 2>&1; then
    sudo dnf install -y "$@"
  elif command -v apk >/dev/null 2>&1; then
    sudo apk add "$@"
  else
    printf 'Поддерживаются Debian/Ubuntu, Fedora и Alpine.\n' >&2
    exit 1
  fi
}

command -v git >/dev/null 2>&1 || installPackage git
command -v openssl >/dev/null 2>&1 || installPackage openssl
if ! command -v docker >/dev/null 2>&1; then
  if command -v apt-get >/dev/null 2>&1; then
    installPackage docker.io
    sudo apt-get install -y docker-compose-v2 || sudo apt-get install -y docker-compose
  elif command -v dnf >/dev/null 2>&1; then installPackage docker docker-compose-plugin
  else installPackage docker docker-cli-compose
  fi
  sudo systemctl enable --now docker 2>/dev/null || sudo rc-service docker start 2>/dev/null || true
fi

if [ -d "$installDir/.git" ]; then
  git -C "$installDir" fetch --tags --prune
else
  git clone --depth 1 "$repository" "$installDir"
  git -C "$installDir" fetch --tags --prune
fi

latestTag="$(git -C "$installDir" tag --sort=-version:refname | sed -n '1p')"
if [ -n "$latestTag" ]; then
  git -C "$installDir" checkout --detach "$latestTag"
else
  latestTag="$(git -C "$installDir" rev-parse --short HEAD)"
fi
repositorySlug="$(printf '%s' "$repository" | sed -E 's#^https?://github.com/##; s#^git@github.com:##; s#\.git$##')"
case "$repositorySlug" in */*) export UPDATE_REPOSITORY="$repositorySlug" ;; esac
export DOMAIN="$domain"
export VERSION="$latestTag"
cd "$installDir"
./install.sh

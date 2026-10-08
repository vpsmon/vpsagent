#!/usr/bin/env bash
set -euo pipefail
umask 077

REPO=vpsmon/vpsagent
APP_NAME=vpsagent
REMOTE_DIR=/opt/vpsagent
BIN_PATH="$REMOTE_DIR/$APP_NAME"

[[ $EUID -eq 0 ]] || { echo 'Run this updater with sudo.' >&2; exit 1; }
[[ $(uname -s) == Linux ]] || { echo 'vpsagent updates require Linux.' >&2; exit 1; }
[[ -f $BIN_PATH && -f /etc/systemd/system/$APP_NAME.service ]] || { echo 'vpsagent is not installed.' >&2; exit 1; }
[[ ! -L $REMOTE_DIR && ! -L $BIN_PATH ]] || { echo 'Refusing to update through a symbolic link.' >&2; exit 1; }
for dependency in getconf curl sha256sum systemctl install; do
  command -v "$dependency" >/dev/null 2>&1 || { echo "Missing command: $dependency" >&2; exit 1; }
done
case $(uname -m) in
  x86_64)
    if [[ $(getconf LONG_BIT) == 32 ]]; then GOARCH=386; else GOARCH=amd64; fi ;;
  i386|i486|i586|i686) GOARCH=386 ;;
  aarch64)
    if [[ $(getconf LONG_BIT) == 32 ]]; then GOARCH=arm; else GOARCH=arm64; fi ;;
  armv6l|armv7l|armv8l) GOARCH=arm ;;
  *) echo "Unsupported Linux architecture: $(uname -m)." >&2; exit 1 ;;
esac

RELEASE_JSON=$(curl --proto '=https' --proto-redir '=https' -fsSL "https://api.github.com/repos/$REPO/releases/latest")
RELEASE_TAG=$(printf '%s\n' "$RELEASE_JSON" | grep '"tag_name":' | head -n 1 | cut -d '"' -f 4 || true)
ASSET_NAME="$APP_NAME-linux-$GOARCH"
DOWNLOAD_URL=$(printf '%s\n' "$RELEASE_JSON" | grep '"browser_download_url":' | grep "/${ASSET_NAME}\"" | cut -d '"' -f 4 | head -n 1 || true)
EXPECTED_URL="https://github.com/$REPO/releases/download/$RELEASE_TAG/$ASSET_NAME"
[[ -n $RELEASE_TAG && $DOWNLOAD_URL == "$EXPECTED_URL" ]] || { echo 'No matching agent release found.' >&2; exit 1; }
EXPECTED_SHA=$(curl --proto '=https' --proto-redir '=https' -fsSL "https://github.com/$REPO/releases/download/$RELEASE_TAG/checksums.txt" | awk -v asset="$ASSET_NAME" '$2 == asset {print $1}')
[[ $EXPECTED_SHA =~ ^[[:xdigit:]]{64}$ ]] || { echo 'No valid release checksum found.' >&2; exit 1; }
CURRENT_SHA=$(sha256sum "$BIN_PATH" | awk '{print $1}')
if [[ $CURRENT_SHA == "$EXPECTED_SHA" ]]; then
  echo "vpsagent is already up to date ($RELEASE_TAG)."
  exit 0
fi

TMP_BIN="$REMOTE_DIR/.$APP_NAME.tmp.$$"
BACKUP_BIN="$REMOTE_DIR/.$APP_NAME.previous.$$"
trap 'rm -f "$TMP_BIN" "$BACKUP_BIN" "$BIN_PATH.new"' EXIT
curl --proto '=https' --proto-redir '=https' -fsSL "$DOWNLOAD_URL" -o "$TMP_BIN"
printf '%s  %s\n' "$EXPECTED_SHA" "$TMP_BIN" | sha256sum -c - >/dev/null
install -m 0755 -o root -g root "$BIN_PATH" "$BACKUP_BIN"
install -m 0755 -o root -g root "$TMP_BIN" "$BIN_PATH.new"
mv -f "$BIN_PATH.new" "$BIN_PATH"
if systemctl restart "$APP_NAME" && { sleep 2; systemctl is-active --quiet "$APP_NAME"; }; then
  :
else
  mv -f "$BACKUP_BIN" "$BIN_PATH"
  systemctl restart "$APP_NAME" || true
  echo 'Update failed; restored the previous agent binary.' >&2
  exit 1
fi
echo "vpsagent updated to $RELEASE_TAG."
echo "Status: systemctl status $APP_NAME"

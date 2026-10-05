#!/usr/bin/env bash
set -euo pipefail
umask 077

# Installs only vpsagent. The local vpsmon dashboard is not changed.
REPO=vpsmon/vpsagent
APP_NAME=vpsagent
REMOTE_DIR=/opt/vpsagent
SERVICE_USER=vpsagent
SERVICE_GROUP=vpsagent
CLOUD_URL=
SETUP_TOKEN=
ALLOW_INSECURE_LOCAL=false
ENABLE_REMOTE_UPDATES=false

usage() {
  echo 'Usage: install.sh [--cloud-url URL] [--setup-token TOKEN] [--allow-insecure-local] [--enable-remote-updates]' >&2
  exit 2
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --cloud-url) [[ $# -ge 2 ]] || usage; CLOUD_URL=$2; shift 2 ;;
    --setup-token) [[ $# -ge 2 ]] || usage; SETUP_TOKEN=$2; shift 2 ;;
    --enable-remote-updates) ENABLE_REMOTE_UPDATES=true; shift ;;
    --allow-insecure-local) ALLOW_INSECURE_LOCAL=true; shift ;;
    -h|--help) echo 'Usage: install.sh [--cloud-url URL] [--setup-token TOKEN] [--allow-insecure-local] [--enable-remote-updates]'; exit 0 ;;
    *) usage ;;
  esac
done

[[ $EUID -eq 0 ]] || { echo 'Run this installer with sudo.' >&2; exit 1; }
[[ $(uname -s) == Linux ]] || { echo 'vpsagent service installation requires Linux.' >&2; exit 1; }
if [[ -e $REMOTE_DIR || -L $REMOTE_DIR || -e /etc/systemd/system/$APP_NAME.service || -L /etc/systemd/system/$APP_NAME.service ]]; then
  echo 'vpsagent files already exist. Use /opt/vpsagent/update.sh for an installed agent, or remove a partial installation before retrying.' >&2
  exit 1
fi
if [[ -z $CLOUD_URL ]]; then
  read -r -p 'VPSmon Cloud URL (https://...): ' CLOUD_URL </dev/tty || usage
fi
if [[ $CLOUD_URL =~ ^https:// ]]; then
  :
elif [[ $ALLOW_INSECURE_LOCAL == true && $CLOUD_URL =~ ^http://(localhost|127\.0\.0\.1|\[::1\])(:[0-9]{1,5})?$ ]]; then
  :
else
  echo 'Cloud URL must use HTTPS; loopback HTTP requires --allow-insecure-local.' >&2
  exit 1
fi
for dependency in curl sha256sum systemctl getent groupadd useradd install chown mktemp; do
  command -v "$dependency" >/dev/null 2>&1 || { echo "Missing command: $dependency" >&2; exit 1; }
done
case $(uname -m) in
  x86_64) GOARCH=amd64 ;;
  aarch64|armv8l) GOARCH=arm64 ;;
  *) echo 'Unsupported Linux architecture.' >&2; exit 1 ;;
esac

RELEASE_JSON=$(curl --proto '=https' --proto-redir '=https' -fsSL "https://api.github.com/repos/$REPO/releases/latest")
RELEASE_TAG=$(printf '%s\n' "$RELEASE_JSON" | grep '"tag_name":' | head -n 1 | cut -d '"' -f 4 || true)
ASSET_NAME="$APP_NAME-linux-$GOARCH"
DOWNLOAD_URL=$(printf '%s\n' "$RELEASE_JSON" | grep '"browser_download_url":' | grep "/${ASSET_NAME}\"" | cut -d '"' -f 4 | head -n 1 || true)
EXPECTED_URL="https://github.com/$REPO/releases/download/$RELEASE_TAG/$ASSET_NAME"
[[ -n $RELEASE_TAG && $DOWNLOAD_URL == "$EXPECTED_URL" ]] || { echo 'No matching agent release found.' >&2; exit 1; }
TEMP_DIR=$(mktemp -d /opt/.vpsagent-install.XXXXXX)
TMP_BIN="$TEMP_DIR/$APP_NAME"
TMP_UPDATE="$TEMP_DIR/update.sh"
TMP_REMOVE="$TEMP_DIR/remove.sh"
trap 'rm -rf -- "$TEMP_DIR"' EXIT
curl --proto '=https' --proto-redir '=https' -fsSL "$DOWNLOAD_URL" -o "$TMP_BIN"
EXPECTED_SHA=$(curl --proto '=https' --proto-redir '=https' -fsSL "https://github.com/$REPO/releases/download/$RELEASE_TAG/checksums.txt" | awk -v asset="$ASSET_NAME" '$2 == asset {print $1}')
[[ $EXPECTED_SHA =~ ^[[:xdigit:]]{64}$ ]] || { echo 'No valid release checksum found.' >&2; exit 1; }
printf '%s  %s\n' "$EXPECTED_SHA" "$TMP_BIN" | sha256sum -c - >/dev/null
chmod 0700 "$TMP_BIN"
if ! CONNECT_HELP=$("$TMP_BIN" connect --help 2>&1); then
  echo 'Could not inspect the downloaded agent. Check that this VPS can run the release binary.' >&2
  exit 1
fi
# Go's flag package prints single-dash flags; accept either help format.
# Capture help first so grep cannot cause SIGPIPE under pipefail.
if ! grep -Eq -- '^[[:space:]]+-{1,2}setup-token-stdin([[:space:]]|$)' <<< "$CONNECT_HELP"; then
  echo 'Latest release does not support this installer yet. Wait for the next vpsagent release and retry.' >&2
  exit 1
fi

for helper in update remove; do
  helper_tmp="$TEMP_DIR/$helper.sh"
  curl --proto '=https' --proto-redir '=https' -fsSL "https://raw.githubusercontent.com/$REPO/$RELEASE_TAG/scripts/$helper.sh" -o "$helper_tmp"
  bash -n "$helper_tmp"
done

if [[ -z $SETUP_TOKEN ]]; then
  echo 'Copy the one-use setup token from the Cloud Add a server dialog.'
  read -r -s -p 'Paste setup token: ' SETUP_TOKEN </dev/tty || { echo 'Could not read setup token.' >&2; exit 1; }
  printf '\n' >/dev/tty
fi
[[ -n $SETUP_TOKEN ]] || { echo 'Setup token cannot be empty.' >&2; exit 1; }
install -d -m 0755 -o root -g root "$REMOTE_DIR"
CREATED_GROUP=false
CREATED_USER=false
printf 'created_user=%s\ncreated_group=%s\n' "$CREATED_USER" "$CREATED_GROUP" > "$REMOTE_DIR/.managed"
chmod 0600 "$REMOTE_DIR/.managed"
install -m 0755 -o root -g root "$TMP_BIN" "$REMOTE_DIR/$APP_NAME"
install -m 0755 -o root -g root "$TMP_UPDATE" "$REMOTE_DIR/update.sh"
install -m 0755 -o root -g root "$TMP_REMOVE" "$REMOTE_DIR/remove.sh"

CONNECT_ARGS=(--url "$CLOUD_URL" --config "$REMOTE_DIR/cloud.json" --setup-token-stdin)
if [[ $ALLOW_INSECURE_LOCAL == true ]]; then
  CONNECT_ARGS+=(--allow-insecure-local)
fi
if ! printf '%s\n' "$SETUP_TOKEN" | "$REMOTE_DIR/$APP_NAME" connect "${CONNECT_ARGS[@]}"; then
  rm -rf -- "$REMOTE_DIR"
  echo 'Pairing failed. Check the Cloud URL and request a fresh setup token, then retry.' >&2
  exit 1
fi
unset SETUP_TOKEN
if ! getent group "$SERVICE_GROUP" >/dev/null; then
  groupadd --system "$SERVICE_GROUP"
  CREATED_GROUP=true
  printf 'created_user=%s\ncreated_group=%s\n' "$CREATED_USER" "$CREATED_GROUP" > "$REMOTE_DIR/.managed"
fi
if ! id "$SERVICE_USER" >/dev/null 2>&1; then
  useradd --system --gid "$SERVICE_GROUP" --home-dir "$REMOTE_DIR" --shell /usr/sbin/nologin "$SERVICE_USER"
  CREATED_USER=true
  printf 'created_user=%s\ncreated_group=%s\n' "$CREATED_USER" "$CREATED_GROUP" > "$REMOTE_DIR/.managed"
fi
chown "$SERVICE_USER:$SERVICE_GROUP" "$REMOTE_DIR/cloud.json"
chmod 0600 "$REMOTE_DIR/cloud.json"
{
  printf 'VPSAGENT_CONFIG=%s/cloud.json\n' "$REMOTE_DIR"
  if [[ $ALLOW_INSECURE_LOCAL == true ]]; then
    printf 'VPSAGENT_ALLOW_INSECURE=true\n'
  fi
} > "$REMOTE_DIR/.env"
chmod 0600 "$REMOTE_DIR/.env"
chown root:root "$REMOTE_DIR/.env"

cat > "/etc/systemd/system/$APP_NAME.service" <<SERVICE
[Unit]
Description=VPSmon Cloud agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$SERVICE_USER
Group=$SERVICE_GROUP
WorkingDirectory=$REMOTE_DIR
EnvironmentFile=$REMOTE_DIR/.env
ExecStart=$REMOTE_DIR/$APP_NAME
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true

[Install]
WantedBy=multi-user.target
SERVICE
chmod 0644 "/etc/systemd/system/$APP_NAME.service"
systemctl daemon-reload
systemctl enable "$APP_NAME" --quiet
systemctl restart "$APP_NAME"
sleep 2
systemctl is-active --quiet "$APP_NAME" || {
  echo "Agent did not start; inspect journalctl -u $APP_NAME -n 50" >&2
  exit 1
}
if [[ $ENABLE_REMOTE_UPDATES == true ]]; then
  "$REMOTE_DIR/$APP_NAME" enable-remote-updates
fi
echo 'vpsagent installed and connected to VPSmon Cloud.'
echo "Status: systemctl status $APP_NAME"
echo "Update: sudo $REMOTE_DIR/update.sh"
echo "Remove: sudo $REMOTE_DIR/remove.sh"

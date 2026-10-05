#!/usr/bin/env bash
set -euo pipefail

APP_NAME=vpsagent
REMOTE_DIR=/opt/vpsagent
SERVICE_USER=vpsagent
SERVICE_GROUP=vpsagent
ASSUME_YES=false

case ${1:-} in
  '') ;;
  --yes) ASSUME_YES=true ;;
  *) echo 'Usage: remove.sh [--yes]' >&2; exit 2 ;;
esac
[[ $EUID -eq 0 ]] || { echo 'Run this remover with sudo.' >&2; exit 1; }
[[ $(uname -s) == Linux ]] || { echo 'vpsagent removal requires Linux.' >&2; exit 1; }
[[ ! -L $REMOTE_DIR ]] || { echo 'Refusing to remove a symbolic-link installation.' >&2; exit 1; }
if [[ ! -d $REMOTE_DIR && ! -e /etc/systemd/system/$APP_NAME.service ]]; then
  echo 'vpsagent is not installed.'
  exit 0
fi
[[ -f $REMOTE_DIR/.managed && ! -L $REMOTE_DIR/.managed ]] || {
  echo 'Refusing to remove an installation without the installer ownership marker.' >&2
  exit 1
}
created_user=false
created_group=false
grep -qx 'created_user=true' "$REMOTE_DIR/.managed" && created_user=true || true
grep -qx 'created_group=true' "$REMOTE_DIR/.managed" && created_group=true || true
if [[ $ASSUME_YES != true ]]; then
  echo 'This removes the local vpsagent service and its Cloud credential.'
  echo 'Cloud history and the server entry remain until removed in the Cloud dashboard.'
  read -r -p 'Remove vpsagent? [y/N]: ' choice </dev/tty || exit 1
  [[ $choice == [Yy] ]] || { echo 'Aborted.'; exit 0; }
fi
systemctl disable --now vpsagent-update.path vpsagent-update.service 2>/dev/null || true
rm -f /etc/systemd/system/vpsagent.service.d/remote-updates.conf
rm -f /etc/systemd/system/vpsagent-update.path /etc/systemd/system/vpsagent-update.service
rm -rf -- /var/lib/vpsagent-control /var/cache/vpsagent-update
systemctl stop "$APP_NAME" 2>/dev/null || true
systemctl disable "$APP_NAME" 2>/dev/null || true
rm -f "/etc/systemd/system/$APP_NAME.service"
systemctl daemon-reload
rm -rf -- "$REMOTE_DIR"
if [[ $created_user == true ]] && id "$SERVICE_USER" >/dev/null 2>&1; then
  userdel "$SERVICE_USER"
fi
if [[ $created_group == true ]] && getent group "$SERVICE_GROUP" >/dev/null 2>&1; then
  groupdel "$SERVICE_GROUP" || true
fi
echo 'vpsagent removed. Remove the server in VPSmon Cloud if you also want to revoke its token and delete its Cloud history.'

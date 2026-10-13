#!/usr/bin/env bash
set -euo pipefail
umask 077

REMOTE_DIR=/opt/vpsagent
BIN_PATH="$REMOTE_DIR/vpsagent"
[[ $EUID -eq 0 ]] || { echo 'Run this updater with sudo.' >&2; exit 1; }
[[ $(uname -s) == Linux ]] || { echo 'vpsagent updates require Linux.' >&2; exit 1; }
[[ $# -eq 0 ]] || { echo 'Usage: update.sh' >&2; exit 2; }
[[ -f $BIN_PATH && -f /etc/systemd/system/vpsagent.service ]] || { echo 'vpsagent is not installed.' >&2; exit 1; }
[[ ! -L $REMOTE_DIR && ! -L $BIN_PATH ]] || { echo 'Refusing to update through a symbolic link.' >&2; exit 1; }
# The installed authenticated binary owns signature verification, architecture
# selection, locking, disk reserves, durable replacement and rollback.
exec "$BIN_PATH" update

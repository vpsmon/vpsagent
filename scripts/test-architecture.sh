#!/usr/bin/env bash
# Exercise the actual installer/updater selection without touching the host.
set -euo pipefail
cd "$(dirname "$0")/.."
for script in scripts/install.sh scripts/update.sh; do
  selection=$(sed -n '/^case $(uname -m) in$/,/^esac$/p' "$script")
  [[ -n $selection ]]
  while read -r machine bits expected; do
    actual=$(
      uname() { printf '%s\n' "$machine"; }
      getconf() { [[ $1 == LONG_BIT ]]; printf '%s\n' "$bits"; }
      eval "$selection"
      printf '%s' "$GOARCH"
    )
    [[ $actual == "$expected" ]] || { echo "$script: $machine/$bits selected $actual, expected $expected" >&2; exit 1; }
  done <<'CASES'
x86_64 64 amd64
x86_64 32 386
i386 32 386
i486 32 386
i586 32 386
i686 32 386
aarch64 64 arm64
aarch64 32 arm
armv6l 32 arm
armv7l 32 arm
armv8l 32 arm
CASES
  if (
    uname() { printf '%s\n' mips; }
    eval "$selection"
  ) 2>/dev/null; then
    echo "$script accepted unsupported architecture" >&2
    exit 1
  fi
  echo "$script: architecture checks passed"
done

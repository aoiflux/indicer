#!/usr/bin/env bash
# mounts.sh sets up the special filesystems the failure tests need. Run as
# root:
#
#   wsl -u root --exec bash scripts/wsl/mounts.sh up|down|env
#
#   /mnt/dues-enospc   16 MB tmpfs, writable by everyone: writes fill it, so
#                      tests see a real ENOSPC (DUES_TEST_ENOSPC_DIR)
#   /mnt/dues-ro       read-only tmpfs holding one file (DUES_TEST_RO_DIR)
#
# `env` prints the export lines suite.sh sources.
set -euo pipefail

enospc=/mnt/dues-enospc
ro=/mnt/dues-ro

up() {
	[ "$(id -u)" = 0 ] || { echo "mounts.sh up must run as root" >&2; exit 1; }
	mkdir -p "$enospc" "$ro"
	mountpoint -q "$enospc" || mount -t tmpfs -o size=16m,mode=1777 dues-enospc "$enospc"
	if ! mountpoint -q "$ro"; then
		mount -t tmpfs -o size=1m,mode=0755 dues-ro "$ro"
		echo "read-only fixture" >"$ro/readme.txt"
		mount -o remount,ro "$ro"
	fi
	echo "mounted $enospc (16 MB) and $ro (read-only)"
}

down() {
	[ "$(id -u)" = 0 ] || { echo "mounts.sh down must run as root" >&2; exit 1; }
	mountpoint -q "$enospc" && umount "$enospc"
	mountpoint -q "$ro" && umount "$ro"
	echo "unmounted"
}

case "${1:-}" in
up) up ;;
down) down ;;
env)
	echo "export DUES_TEST_ENOSPC_DIR=$enospc"
	echo "export DUES_TEST_RO_DIR=$ro"
	;;
*) echo "usage: mounts.sh up|down|env" >&2; exit 2 ;;
esac

#!/usr/bin/env bash
# mounts.sh sets up the special filesystems the failure tests need. Run as
# root:
#
#   wsl -u root --exec bash scripts/wsl/mounts.sh up|down|env
#
#   /mnt/dues-enospc   16 MB tmpfs, writable by everyone: writes fill it, so
#                      tests see a real ENOSPC (DUES_TEST_ENOSPC_DIR)
#   /mnt/dues-lowspace 384 MB tmpfs, writable by everyone: room for a
#                      database and DUES's free-space reserve, so a store can
#                      fill it part way (DUES_TEST_LOWSPACE_DIR)
#   /mnt/dues-ro       read-only tmpfs holding one file (DUES_TEST_RO_DIR)
#   /mnt/dues-ro-db    read-only bind of /var/tmp/dues-ro-src, which everyone
#                      can write: a test builds a database in the source and
#                      reads it through the bind, as from read-only media
#                      (DUES_TEST_RO_SRC, DUES_TEST_RO_DB)
#
# `env` prints the export lines suite.sh sources.
set -euo pipefail

enospc=/mnt/dues-enospc
lowspace=/mnt/dues-lowspace
ro=/mnt/dues-ro
rosrc=/var/tmp/dues-ro-src
rodb=/mnt/dues-ro-db

up() {
	[ "$(id -u)" = 0 ] || { echo "mounts.sh up must run as root" >&2; exit 1; }
	mkdir -p "$enospc" "$lowspace" "$ro" "$rosrc" "$rodb"
	chmod 1777 "$rosrc"
	mountpoint -q "$enospc" || mount -t tmpfs -o size=16m,mode=1777 dues-enospc "$enospc"
	mountpoint -q "$lowspace" || mount -t tmpfs -o size=384m,mode=1777 dues-lowspace "$lowspace"
	if ! mountpoint -q "$rodb"; then
		mount --bind "$rosrc" "$rodb"
		mount -o remount,bind,ro "$rodb"
	fi
	if ! mountpoint -q "$ro"; then
		mount -t tmpfs -o size=1m,mode=0755 dues-ro "$ro"
		echo "read-only fixture" >"$ro/readme.txt"
		mount -o remount,ro "$ro"
	fi
	echo "mounted $enospc (16 MB), $lowspace (384 MB), $ro (read-only) and $rodb (read-only view of $rosrc)"
}

down() {
	[ "$(id -u)" = 0 ] || { echo "mounts.sh down must run as root" >&2; exit 1; }
	for m in "$enospc" "$lowspace" "$ro" "$rodb"; do
		if mountpoint -q "$m"; then umount "$m"; fi
	done
	echo "unmounted"
}

case "${1:-}" in
up) up ;;
down) down ;;
env)
	echo "export DUES_TEST_ENOSPC_DIR=$enospc"
	echo "export DUES_TEST_LOWSPACE_DIR=$lowspace"
	echo "export DUES_TEST_RO_DIR=$ro"
	echo "export DUES_TEST_RO_SRC=$rosrc"
	echo "export DUES_TEST_RO_DB=$rodb"
	;;
*) echo "usage: mounts.sh up|down|env" >&2; exit 2 ;;
esac

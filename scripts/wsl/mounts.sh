#!/usr/bin/env bash
# mounts.sh sets up the special filesystems the failure tests need. Run as
# root:
#
#   wsl -u root --exec bash scripts/wsl/mounts.sh up|down|args
#
#   /mnt/dues-enospc   16 MB tmpfs, writable by everyone: writes fill it, so
#                      tests see a real ENOSPC (-dues.enospc-dir)
#   /mnt/dues-lowspace 384 MB tmpfs, writable by everyone: room for a
#                      database and DUES's free-space reserve, so a store can
#                      fill it part way (-dues.lowspace-dir)
#   /mnt/dues-ro-db    read-only bind of /var/tmp/dues-ro-src, which everyone
#                      can write: a test builds a database in the source and
#                      reads it through the bind, as from read-only media
#                      (-dues.ro-src, -dues.ro-db)
#
# `args` prints the go test flags that name them, one per line, for suite.sh
# to pass after -args; it needs no root.
set -euo pipefail

enospc=/mnt/dues-enospc
lowspace=/mnt/dues-lowspace
rosrc=/var/tmp/dues-ro-src
rodb=/mnt/dues-ro-db
# A read-only tmpfs earlier versions mounted; no test used it.
oldro=/mnt/dues-ro

up() {
	[ "$(id -u)" = 0 ] || { echo "mounts.sh up must run as root" >&2; exit 1; }
	mkdir -p "$enospc" "$lowspace" "$rosrc" "$rodb"
	chmod 1777 "$rosrc"
	mountpoint -q "$enospc" || mount -t tmpfs -o size=16m,mode=1777 dues-enospc "$enospc"
	mountpoint -q "$lowspace" || mount -t tmpfs -o size=384m,mode=1777 dues-lowspace "$lowspace"
	if ! mountpoint -q "$rodb"; then
		mount --bind "$rosrc" "$rodb"
		mount -o remount,bind,ro "$rodb"
	fi
	echo "mounted $enospc (16 MB), $lowspace (384 MB) and $rodb (read-only view of $rosrc)"
}

down() {
	[ "$(id -u)" = 0 ] || { echo "mounts.sh down must run as root" >&2; exit 1; }
	for m in "$enospc" "$lowspace" "$rodb" "$oldro"; do
		if mountpoint -q "$m"; then umount "$m"; fi
	done
	echo "unmounted"
}

case "${1:-}" in
up) up ;;
down) down ;;
args)
	echo "-dues.enospc-dir=$enospc"
	echo "-dues.lowspace-dir=$lowspace"
	echo "-dues.ro-src=$rosrc"
	echo "-dues.ro-db=$rodb"
	;;
*) echo "usage: mounts.sh up|down|args" >&2; exit 2 ;;
esac

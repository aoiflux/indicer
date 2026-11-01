#!/usr/bin/env bash
# gen_ntfs_extra.sh builds the NTFS fixtures that fsagen does not cover:
# LZNT1-compressed files and sparse files, written by ntfs-3g. It is separate
# from gen_fixtures.sh so adding it does not regenerate every other fixture.
#
#   wsl -u root --exec bash lib/parser/testdata/gen/gen_ntfs_extra.sh [--dataset-dir DIR]
#
# Requires: ntfs-3g (mkntfs, setfattr, getfattr), zstd, python3.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
source "$here/lib.sh"

echo "NTFS compressed and sparse"
img="$work/ntfs_compressed.img"
prefill "$img" $((8 << 20))
mkntfs -q -F -f -c 4096 -s 512 -L FXCOMP -T "$img" >/dev/null
ntfs-3g -o compression "$img" "$mnt"

# Files created in a directory with the compressed attribute (0x800) are
# compressed by ntfs-3g in 16-cluster units.
mkdir "$mnt/z"
setfattr -h -n system.ntfs_attrib_be -v 0x00000800 "$mnt/z"
python3 - "$mnt" <<'PY'
import sys
root = sys.argv[1]
text = b"".join(b"line %06d of the compressible fixture\n" % i for i in range(6000))
open(root + "/z/text.txt", "wb").write(text[:200000])
PY
content 1 70000 >"$mnt/z/random.bin"
{
	python3 -c 'import sys; sys.stdout.buffer.write(b"abcdefgh" * 8192)'
	content 2 65536
	head -c 65536 /dev/zero
	python3 -c 'import sys; sys.stdout.buffer.write(b"tail " * 3000)'
} >"$mnt/z/mixed.bin"
printf 'small and resident\n' >"$mnt/z/small.txt"

# An uncompressed sparse file: a hole, 4 KiB of data, a hole.
truncate -s 1M "$mnt/sparse.bin"
content 3 4096 | dd of="$mnt/sparse.bin" bs=4096 seek=128 conv=notrunc status=none
content 9 50000 >"$mnt/plain.bin"
sync

for f in z/text.txt z/random.bin z/mixed.bin; do
	attr="$(getfattr -h -e hex -n system.ntfs_attrib_be "$mnt/$f" | sed -n 's/.*=//p')"
	[ $((attr & 0x800)) -ne 0 ] || { echo "$f is not compressed ($attr)" >&2; exit 1; }
done
scan_mount "$work/ntfs_compressed.truth.jsonl"
umount "$mnt"
publish ntfs ntfs_compressed "$img" raw --part "0:ntfs:$work/ntfs_compressed.truth.jsonl"

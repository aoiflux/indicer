#!/usr/bin/env bash
# gen_fixtures.sh builds the parser fixture images and their ground-truth
# manifests.
#
# Every image is made by third-party tools (mkfs.*, mtools, hfsutils, the
# kernel's own filesystem drivers, fsagen, ewfacquire, qemu-img), never by
# DUES, so a fixture cannot share a misreading of a format with the parser it
# tests.
#
# Run it in WSL as root (loop mounts need root):
#
#   wsl -u root --exec bash lib/parser/testdata/gen/gen_fixtures.sh
#
# Outputs:
#   lib/parser/testdata/fixtures/<group>/<name>.img.zst   compressed image
#   lib/parser/testdata/fixtures/<group>/<name>.manifest.json
#   $DUES_DATASET_DIR/<group>/<name>.*                     uncompressed copies
#                                                          (default /mnt/e/dataset/dues_synth)
#
# Requires: dosfstools mtools exfatprogs xfsprogs e2fsprogs hfsutils hfsprogs
# gdisk ewf-tools qemu-utils zstd python3, and fsagen.exe through WSL interop
# ($FSAGEN, default /mnt/o/research/fsagen/fsagen.exe).
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../../../.." && pwd)"
out="$repo/lib/parser/testdata/fixtures"
dataset="${DUES_DATASET_DIR:-/mnt/e/dataset/dues_synth}"
fsagen="${FSAGEN:-/mnt/o/research/fsagen/fsagen.exe}"
py="python3 $here/fixtures.py"
work="$(mktemp -d /tmp/dues-fx.XXXXXX)"
mnt="$work/mnt"
mkdir -p "$mnt"
cleanup() {
	mountpoint -q "$mnt" && umount "$mnt" || true
	rm -rf "$work"
}
trap cleanup EXIT

[ "$(id -u)" = 0 ] || { echo "run as root (loop mounts)" >&2; exit 1; }
gen_version="gen_fixtures.sh $(git -C "$repo" rev-parse --short HEAD 2>/dev/null || echo unknown)"

content() { $py content "$1" "$2"; }

# prefill IMG SIZE: create IMG filled with 0xA5 rather than zeros, so blocks a
# filesystem allocates but never writes (unwritten extents, tail slack) hold
# visible garbage. A parser that reads them as data then gets a wrong digest
# instead of passing by luck. A constant pattern still compresses to nothing.
prefill() { head -c "$2" /dev/zero | tr '\000' '\245' >"$1"; }

# publish GROUP NAME MANIFEST-ARGS... : compress the image (or segments) into
# the repo, copy the originals to the dataset dir and write the manifest.
publish() {
	local group="$1" name="$2" image="$3" fmt="$4"
	shift 4
	mkdir -p "$out/$group" "$dataset/$group"
	local first="${image%%,*}"
	local IFS_SAVE="$IFS"
	IFS=','
	for seg in $image; do
		cp -f "$seg" "$dataset/$group/"
		zstd -q -19 -f "$seg" -o "$out/$group/$(basename "$seg").zst"
	done
	IFS="$IFS_SAVE"
	$py manifest "$image" "$fmt" "$gen_version" "$out/$group/$name.manifest.json" "$@"
	echo "  $group/$name: $(du -ch "$out/$group/"*.zst 2>/dev/null | tail -1 | cut -f1) compressed in $group"
}

# scan_mount TRUTH : record the mounted volume's ground truth.
scan_mount() { $py scan "$mnt" >"$1"; }

# -------------------------------------------------------------------- FAT --
echo "FAT"
mk_fat() { # NAME FATBITS SIZE_KIB
	local name="$1" bits="$2" kib="$3" img="$work/$1.img"
	rm -f "$img"
	mkfs.vfat -C -F "$bits" -i 0d0e5f1"$bits" -n "FX$bits" "$img" "$kib" >/dev/null
	mount -o loop,uid=0,gid=0 "$img" "$mnt"
	printf 'hello dues\n' >"$mnt/hello.txt"
	: >"$mnt/empty.txt"
	mkdir -p "$mnt/sub/deeper"
	content 1 5000 >"$mnt/sub/a.bin"
	content 2 70000 >"$mnt/sub/deeper/b.bin"
	content 3 300 >"$mnt/Long File Name With Spaces.txt"
	# Fragment: fill, punch a gap, then write a file larger than the gap.
	content 4 20000 >"$mnt/f1.bin"
	content 5 8192 >"$mnt/gap.bin"
	content 6 20000 >"$mnt/f2.bin"
	sync
	rm "$mnt/gap.bin"
	sync
	content 7 30000 >"$mnt/frag.bin"
	# A deleted file whose clusters nothing reuses afterwards.
	content 8 3000 >"$mnt/deleted.bin"
	sha_del="$(sha256sum "$mnt/deleted.bin" | cut -d' ' -f1)"
	sync
	rm "$mnt/deleted.bin"
	sync
	scan_mount "$work/$name.truth.jsonl"
	umount "$mnt"
	printf '{"path":"/deleted.bin","kind":"file","deleted":true,"size":3000,"sha256":"%s","recoverable":"full"}\n' "$sha_del" >"$work/$name.deleted.jsonl"
	publish fat "$name" "$img" raw --part "0:fat:$work/$name.truth.jsonl:$work/$name.deleted.jsonl"
}
mk_fat fat12 12 1024
mk_fat fat16 16 16384
mk_fat fat32 32 34816

# ------------------------------------------------------------------ exFAT --
echo "exFAT"
img="$work/exfat.img"
truncate -s 8M "$img"
mkfs.exfat -L FXEXFAT "$img" >/dev/null
mount -o loop "$img" "$mnt"
printf 'hello dues\n' >"$mnt/hello.txt"
: >"$mnt/empty.txt"
mkdir -p "$mnt/sub"
content 11 5000 >"$mnt/sub/a.bin"
content 12 40000 >"$mnt/f1.bin"
content 13 16384 >"$mnt/gap.bin"
content 14 40000 >"$mnt/f2.bin"
sync
rm "$mnt/gap.bin"
sync
content 15 60000 >"$mnt/frag.bin"
content 16 3000 >"$mnt/deleted.bin"
sha_del="$(sha256sum "$mnt/deleted.bin" | cut -d' ' -f1)"
sync
rm "$mnt/deleted.bin"
sync
scan_mount "$work/exfat.truth.jsonl"
umount "$mnt"
printf '{"path":"/deleted.bin","kind":"file","deleted":true,"size":3000,"sha256":"%s","recoverable":"full"}\n' "$sha_del" >"$work/exfat.deleted.jsonl"
publish exfat exfat "$img" raw --part "0:exfat:$work/exfat.truth.jsonl:$work/exfat.deleted.jsonl"

# -------------------------------------------------------------------- ext --
echo "ext4"
img="$work/ext4.img"
prefill "$img" 16777216
mkfs.ext4 -q -F -E nodiscard -O inline_data,^has_journal -L FXEXT4 -U 11111111-2222-3333-4444-555555555555 "$img"
mount -o loop "$img" "$mnt"
printf 'tiny inline file\n' >"$mnt/inline.txt"
: >"$mnt/empty.txt"
mkdir -p "$mnt/sub/deeper"
content 21 5000 >"$mnt/sub/a.bin"
content 22 70000 >"$mnt/sub/deeper/b.bin"
fallocate -l 65536 "$mnt/unwritten.bin"
# Sparse: data at 0 and at 256 KiB, a hole between.
content 23 4096 >"$mnt/sparse.bin"
content 24 4096 | dd of="$mnt/sparse.bin" bs=4096 seek=64 conv=notrunc status=none
# Partly written preallocation: first 4 KiB written, rest unwritten.
fallocate -l 32768 "$mnt/prealloc.bin"
content 25 4096 | dd of="$mnt/prealloc.bin" bs=4096 conv=notrunc status=none
ln -s inline.txt "$mnt/fastlink"
ln -s "$(printf 'x%.0s' $(seq 1 80))/target" "$mnt/slowlink"
ln "$mnt/sub/a.bin" "$mnt/hardlink.bin"
content 26 9000 >"$mnt/deleted.bin"
sha_del="$(sha256sum "$mnt/deleted.bin" | cut -d' ' -f1)"
sync
rm "$mnt/deleted.bin"
sync
scan_mount "$work/ext4.truth.jsonl"
umount "$mnt"
# ext4 zeroes the extent tree of a deleted inode, so its data is not
# recoverable from metadata; the record says so rather than expecting it.
printf '{"path":"/deleted.bin","kind":"file","deleted":true,"size":9000,"sha256":"%s","recoverable":"none"}\n' "$sha_del" >"$work/ext4.deleted.jsonl"
publish ext ext4 "$img" raw --part "0:ext:$work/ext4.truth.jsonl:$work/ext4.deleted.jsonl"

# -------------------------------------------------------------------- XFS --
echo "XFS"
img="$work/xfs.img"
prefill "$img" 67108864
# xfsprogs refuses volumes under 300 MB unless it believes it is under test.
QA_CHECK_FS=1 TEST_DEV=1 TEST_DIR=1 mkfs.xfs -q -f -K -L FXXFS -d agcount=4 -m uuid=22222222-3333-4444-5555-666666666666 "$img"
mount -o loop "$img" "$mnt"
printf 'hello dues\n' >"$mnt/hello.txt"
: >"$mnt/empty.txt"
mkdir -p "$mnt/sub/deeper"
content 31 5000 >"$mnt/sub/a.bin"
content 32 70000 >"$mnt/sub/deeper/b.bin"
# Size not a multiple of the block size: the tail block has slack (PAR-4).
content 33 4097 >"$mnt/slack.bin"
fallocate -l 65536 "$mnt/unwritten.bin"
content 34 4096 >"$mnt/sparse.bin"
content 35 4096 | dd of="$mnt/sparse.bin" bs=4096 seek=64 conv=notrunc status=none
# Many extents: write every other block, then fill the gaps.
for i in $(seq 0 2 30); do content $((400 + i)) 4096 | dd of="$mnt/multi.bin" bs=4096 seek="$i" conv=notrunc status=none; sync; done
for i in $(seq 1 2 31); do content $((400 + i)) 4096 | dd of="$mnt/multi.bin" bs=4096 seek="$i" conv=notrunc status=none; done
ln "$mnt/sub/a.bin" "$mnt/hardlink.bin"
sync
scan_mount "$work/xfs.truth.jsonl"
umount "$mnt"
publish xfs xfs "$img" raw --part "0:xfs:$work/xfs.truth.jsonl"

# -------------------------------------------------------------------- HFS --
echo "HFS"
img="$work/hfs_classic.img"
dd if=/dev/zero of="$img" bs=1M count=4 status=none
hformat -l FXHFS "$img" >/dev/null
hmount "$img" >/dev/null
mkdir -p "$work/hfsin"
printf 'hello dues\n' >"$work/hfsin/hello.txt"
content 41 5000 >"$work/hfsin/a.bin"
hcopy -r "$work/hfsin/hello.txt" :hello.txt
hmkdir :sub
hcopy -r "$work/hfsin/a.bin" :sub:a.bin
humount >/dev/null
{
	printf '{"kind":"file","path":"/hello.txt","sha256":"%s","size":11}\n' "$(sha256sum "$work/hfsin/hello.txt" | cut -d' ' -f1)"
	printf '{"kind":"dir","path":"/sub"}\n'
	printf '{"kind":"file","path":"/sub/a.bin","sha256":"%s","size":5000}\n' "$(sha256sum "$work/hfsin/a.bin" | cut -d' ' -f1)"
} >"$work/hfs_classic.truth.jsonl"
publish hfs hfs_classic "$img" raw --part "0:hfs:$work/hfs_classic.truth.jsonl"

img="$work/hfsplus_empty.img"
dd if=/dev/zero of="$img" bs=1M count=8 status=none
mkfs.hfsplus -v FXHFSP "$img" >/dev/null
publish hfs hfsplus_empty "$img" raw --empty "0:hfs"

# ------------------------------------------------------- partition tables --
echo "partitions"
# GPT: FAT16 at 1 MiB, exFAT after it, then a gap of unallocated space.
img="$work/gpt_multi.img"
truncate -s 24M "$img"
# Not -q: with it sgdisk skips writing the table to a regular file.
sgdisk -o \
	-n 1:2048:+8M -t 1:0700 -c 1:fxfat \
	-n 2:0:+8M -t 2:0700 -c 2:fxexfat \
	-U 33333333-4444-5555-6666-777777777777 "$img" >/dev/null
p1=$((2048 * 512))
p2_sector="$(sgdisk -i 2 "$img" | awk '/First sector/ {print $3}')"
p2=$((p2_sector * 512))
part="$work/gpt_p1.img"
rm -f "$part"
mkfs.vfat -C -F 16 -s 2 -i 0d0e5f20 -n GPTFAT "$part" 8192 >/dev/null
mount -o loop "$part" "$mnt"
content 51 5000 >"$mnt/p1.bin"
printf 'partition one\n' >"$mnt/hello.txt"
sync
scan_mount "$work/gpt_p1.truth.jsonl"
umount "$mnt"
dd if="$part" of="$img" bs=512 seek=2048 conv=notrunc status=none
part="$work/gpt_p2.img"
truncate -s 8M "$part"
mkfs.exfat -L GPTEXFAT "$part" >/dev/null
mount -o loop "$part" "$mnt"
content 52 7000 >"$mnt/p2.bin"
sync
scan_mount "$work/gpt_p2.truth.jsonl"
umount "$mnt"
dd if="$part" of="$img" bs=512 seek="$p2_sector" conv=notrunc status=none
publish part gpt_multi "$img" raw \
	--part "$p1:fat:$work/gpt_p1.truth.jsonl" \
	--part "$p2:exfat:$work/gpt_p2.truth.jsonl"

# MBR: FAT12 at 1 MiB and ext4 (populated with mkfs.ext4 -d) after it.
img="$work/mbr_two.img"
truncate -s 12M "$img"
printf 'label: dos\nlabel-id: 0x0d0e5f30\nstart=2048, size=2048, type=1\nstart=4096, size=16384, type=83\n' | sfdisk -q "$img"
part="$work/mbr_p1.img"
rm -f "$part"
mkfs.vfat -C -F 12 -i 0d0e5f31 -n MBRFAT "$part" 1024 >/dev/null
mount -o loop "$part" "$mnt"
content 61 3000 >"$mnt/m1.bin"
sync
scan_mount "$work/mbr_p1.truth.jsonl"
umount "$mnt"
dd if="$part" of="$img" bs=512 seek=2048 conv=notrunc status=none
mkdir -p "$work/mbrsrc/dir"
content 62 6000 >"$work/mbrsrc/m2.bin"
printf 'second partition\n' >"$work/mbrsrc/dir/note.txt"
part="$work/mbr_p2.img"
truncate -s 8M "$part"
mkfs.ext4 -q -F -O ^has_journal -U 44444444-5555-6666-7777-888888888888 -d "$work/mbrsrc" "$part"
mount -o loop,ro "$part" "$mnt"
scan_mount "$work/mbr_p2.truth.jsonl"
umount "$mnt"
dd if="$part" of="$img" bs=512 seek=4096 conv=notrunc status=none
publish part mbr_two "$img" raw \
	--part "$((2048 * 512)):fat:$work/mbr_p1.truth.jsonl" \
	--part "$((4096 * 512)):ext:$work/mbr_p2.truth.jsonl"

# ----------------------------------------------------------------- fsagen --
# fsagen writes NTFS, ext4 and FAT32 images itself and records ground truth,
# including what a deletion leaves recoverable. It is a Windows program run
# through WSL interop, so it writes into the dataset directory.
echo "fsagen"
fsagen_image() { # NAME FS SIZE YAML GROUP
	local name="$1" fs="$2" size="$3" yaml="$4" group="$5"
	local dir="$dataset/fsagen-work"
	mkdir -p "$dir"
	rm -rf "$dir/$name.img" "$dir/$name.img.fsagen"
	cp "$yaml" "$dir/$name.yaml"
	(cd "$dir" && "$fsagen" --seed 7 --manifest "$name.yaml" --image "$name.img" 		--image-fs "$fs" --image-size "$size" --alloc first-fit --partition none >/dev/null)
	cp "$dir/$name.img" "$work/$name.img"
	local fstype="$fs"
	case "$fs" in fat32) fstype=fat ;; ext4) fstype=ext ;; esac
	publish "$group" "$name" "$work/$name.img" raw --answer-key "0:$fstype:$dir/$name.img.fsagen/answer-key.jsonl"
}
fsagen_image ntfs_fsagen ntfs 16M "$here/fsagen_ntfs.yaml" ntfs
fsagen_image ext4_fsagen ext4 16M "$here/fsagen_basic.yaml" ext
fsagen_image fat32_fsagen fat32 40M "$here/fsagen_basic.yaml" fat

# ---------------------------------------------------- container formats --
echo "containers"
# EWF, three segments: a 3 MiB FAT12 volume full of random data, stored
# uncompressed with 1 MiB segments.
src="$work/ewf_src.img"
rm -f "$src"
mkfs.vfat -C -F 12 -i 0d0e5f40 -n EWFFAT "$src" 3072 >/dev/null
mount -o loop "$src" "$mnt"
for i in 1 2 3 4 5; do content $((70 + i)) 60000 >"$mnt/r$i.bin"; done
sync
scan_mount "$work/ewf.truth.jsonl"
umount "$mnt"
(cd "$work" && ewfacquire -u -q -t "$work/fat12_multiseg" -c none -S 1MiB -d sha1 \
	-C fixture -D "dues fixture" -E 1 -e dues -N none -m fixed -M physical -f encase6 "$src" >/dev/null)
segs="$(ls "$work"/fat12_multiseg.E?? | sort | paste -sd, -)"
publish ewf fat12_multiseg "$segs" ewf --decoded "$src" --part "0:fat:$work/ewf.truth.jsonl"

# VHD fixed and dynamic, and VHDX, all wrapping the FAT16 fixture volume.
base="$work/fat16.img"
qemu-img convert -q -f raw -O vpc -o subformat=fixed,force_size=on "$base" "$work/fat16_fixed.vhd"
qemu-img convert -q -f raw -O vpc -o subformat=dynamic,force_size=on "$base" "$work/fat16_dynamic.vhd"
qemu-img convert -q -f raw -O vhdx "$base" "$work/fat16.vhdx"
for v in fat16_fixed.vhd fat16_dynamic.vhd fat16.vhdx; do
	publish vhd "${v%.*}_${v##*.}" "$work/$v" vhd --decoded "$base" \
		--part "0:fat:$work/fat16.truth.jsonl:$work/fat16.deleted.jsonl"
done

du -sh "$out"

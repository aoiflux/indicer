# lib.sh holds what the fixture generators share: paths, the work directory,
# the options, and the helpers that publish an image and its ground truth.
# Source it from a generator; it expects to run in WSL as root. It reads the
# generator's arguments:
#
#   --dataset-dir DIR  where the uncompressed copies go
#                      (default /mnt/e/dataset/dues_synth)
#   --fsagen PATH      fsagen.exe, run through WSL interop
#                      (default /mnt/o/research/fsagen/fsagen.exe)
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../../../.." && pwd)"
out="$repo/lib/parser/testdata/fixtures"
dataset=/mnt/e/dataset/dues_synth
fsagen=/mnt/o/research/fsagen/fsagen.exe
while [ $# -gt 0 ]; do
	case "$1" in
	--dataset-dir | --fsagen)
		[ $# -ge 2 ] || { echo "$1 needs a value" >&2; exit 2; }
		if [ "$1" = --dataset-dir ]; then dataset="$2"; else fsagen="$2"; fi
		shift 2
		;;
	*) echo "unknown option $1" >&2; exit 2 ;;
	esac
done
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

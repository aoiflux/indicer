#!/usr/bin/env bash
# gen_legacy_dbs.sh builds the legacy database fixtures: DUES databases written
# by a past release, kept so every later change can prove it still reads them.
#
# The databases must be written by the old code, not by today's, so the script
# builds the dues binary from a git commit (default: the last commit before the
# correctness pass) rather than from the working tree.
#
# It runs on Windows under Git Bash. Inputs are made in WSL (mkfs.vfat, mtools,
# ewfacquirestream), stores run with the Windows binary so the recorded blob
# paths are Windows absolute paths, and archives are packed in WSL with zstd.
#
# Output, under internal/testutil/legacyfixture/testdata/:
#   <mode>.tar.zst   the database directory (kvdb, blob, graph)
#   golden.json      per mode: flags, password, inputs and their sha256, the
#                    `dues list` JSON, and what restoring each object returned
#
# Usage: scripts/fixtures/gen_legacy_dbs.sh [COMMIT]
set -euo pipefail

commit="${1:-41dbc74}"
repo="$(cd "$(dirname "$0")/../.." && pwd)"
out="$repo/internal/testutil/legacyfixture/testdata"
work="$(mktemp -d "${TMPDIR:-/tmp}/dues-legacyfx.XXXXXX")"
trap '[ -n "${KEEP:-}" ] || rm -rf "$work"' EXIT

# Git Bash rewrites arguments that look like POSIX paths; WSL paths must reach
# wsl.exe untouched.
wslx() { MSYS_NO_PATHCONV=1 wsl.exe "$@"; }
to_wsl() { wslx --exec wslpath -a "$(cygpath -w "$1")" | tr -d '\r'; }

echo "building dues at $commit"
mkdir -p "$work/src"
git -C "$repo" archive "$commit" | tar -x -C "$work/src"
(cd "$work/src" && CGO_ENABLED=0 go build -trimpath -o "$work/dues.exe" .)
dues="$work/dues.exe"

echo "making inputs"
mkdir -p "$work/in"
win_in="$(to_wsl "$work/in")"
wslx --exec sh -euc '
  cd "$1"
  # A 1 MiB FAT12 volume. a.bin starts inside the first 256 KiB chunk and ends
  # in the second, so it straddles a chunk boundary (RST-1).
  mkfs.vfat -C -i 0d0e5f10 -n DUESFX small.img 1024 >/dev/null
  python3 "$2"
  printf "hello dues\n" > hello.txt
  printf "" > empty.txt
  mcopy -i small.img pad.bin a.bin hello.txt empty.txt ::/
  mmd -i small.img ::/sub
  mcopy -i small.img hello.txt ::/sub/h2.txt
  # The FAT volume wrapped in a single-segment E01.
  ewfacquirestream -q -t small -c deflate:best -d sha1 -C fixture -D legacy -E 1 -e dues -N none -m fixed -M physical < small.img >/dev/null
  # Ground truth for the files inside the volume, keyed by volume path.
  for f in pad.bin a.bin hello.txt empty.txt; do printf "/%s\t%s\t%s\n" "$f" "$(sha256sum "$f" | cut -d" " -f1)" "$(stat -c %s "$f")"; done > volume-files.tsv
  printf "/sub/h2.txt\t%s\t%s\n" "$(sha256sum hello.txt | cut -d" " -f1)" "$(stat -c %s hello.txt)" >> volume-files.tsv
  rm -f pad.bin a.bin hello.txt empty.txt
' sh "$win_in" "$(to_wsl "$repo/scripts/fixtures/legacy_inputs.py")"
ls -la "$work/in"

# mode | store flags (global and store options) | inputs
modes=(
  "default||small.img blob.bin"
  "encrypted|--password fixture-pass|small.img blob.bin"
  "quick|--quick|small.img blob.bin"
  "quick-password|--quick --password fixture-pass|small.img blob.bin"
  "container|--container|small.img blob.bin"
  "container-password|--container --password fixture-pass|small.img blob.bin"
  "container-bidx|--container --hierarchical|small.img blob.bin"
  "sha3|--hash-algo sha3|small.img blob.bin"
  "chunk64k|--chonksize 64|small.img blob.bin"
  "revrel|--revrel|small.img blob.bin"
  "hocho|--hash-strategy hocho|small.img blob.bin"
  "e01|--password fixture-pass|small.E01"
  "duplicate|--password fixture-pass|small.img small.img blob.bin"
)

mkdir -p "$out"
golden="$work/golden.ndjson"
: > "$golden"
for spec in "${modes[@]}"; do
  IFS='|' read -r mode flags inputs <<<"$spec"
  echo "== $mode: $flags"
  mdir="$work/db/$mode"
  mkdir -p "$mdir/run"
  # shellcheck disable=SC2086
  for f in $inputs; do
    (cd "$mdir/run" && "$dues" store "$work/in/$f" --dbpath "$mdir/data" $flags >"$mdir/store-$f.log" 2>&1) || echo "store $f exited $?" >>"$mdir/store-errors.log"
  done
  pwflag=()
  case "$flags" in *--password*) pwflag=(--password fixture-pass) ;; esac
  qflag=()
  case "$flags" in *--quick*) qflag=(--quick) ;; esac
  cflag=()
  case "$flags" in *--container*) cflag=(--container) ;; esac
  case "$flags" in *--hierarchical*) cflag+=(--hierarchical) ;; esac
  chflag=()
  case "$flags" in *--chonksize*) chflag=(--chonksize 64) ;; esac
  (cd "$mdir/run" && "$dues" list --dbpath "$mdir/data" "${pwflag[@]}" "${qflag[@]}" "${cflag[@]}" "${chflag[@]}" >"$mdir/list.json" 2>"$mdir/list.err") || true

  # Restore every evidence, partition and indexed file the list reports and
  # record what came back, so the golden documents today's behaviour too.
  python3 - "$mdir" <<'PY' >"$mdir/restore-plan.tsv"
import json, re, sys
d = sys.argv[1]
txt = open(f"{d}/list.json", encoding="utf-8", errors="replace").read()
m = re.search(r"\{.*\}", txt, re.S)
doc = json.loads(m.group(0)) if m else {"evidence": []}
for e in doc.get("evidence", []):
    print("evidence", e["hash"], e["name"], sep="\t")
    for p in e.get("partitions", []) or []:
        print("partition", p["hash"], p["fileName"], sep="\t")
        for f in p.get("indexedFiles", []) or []:
            print("indexed", f["hash"], f["path"], sep="\t")
PY
  # Windows Python writes CRLF; the plan is read line by line below.
  tr -d '\r' <"$mdir/restore-plan.tsv" >"$mdir/restore-plan.lf" && mv "$mdir/restore-plan.lf" "$mdir/restore-plan.tsv"
  i=0
  : >"$mdir/restore.tsv"
  while IFS=$'\t' read -r kind hash name; do
    i=$((i + 1))
    target="$mdir/run/r$i.bin"
    if (cd "$mdir/run" && "$dues" restore "$hash" -f "$target" --dbpath "$mdir/data" "${pwflag[@]}" "${qflag[@]}" "${cflag[@]}" "${chflag[@]}" >"$mdir/restore-$i.log" 2>&1) && [ -f "$target" ]; then
      sum="$(sha256sum "$target" | cut -d' ' -f1)"
      size="$(stat -c %s "$target")"
      printf '%s\t%s\t%s\tok\t%s\t%s\t\n' "$kind" "$hash" "$name" "$sum" "$size" >>"$mdir/restore.tsv"
    else
      err="$(tr -d '\r' <"$mdir/restore-$i.log" | grep -v '^\s*$' | tail -1)"
      printf '%s\t%s\t%s\terror\t\t\t%s\n' "$kind" "$hash" "$name" "$err" >>"$mdir/restore.tsv"
    fi
    rm -f "$target"
  done <"$mdir/restore-plan.tsv"

  python3 - "$mode" "$flags" "$inputs" "$work/in" "$mdir" "$commit" <<'PY' >>"$golden"
import hashlib, json, os, re, sys
mode, flags, inputs, indir, d, commit = sys.argv[1:7]
def sha(p):
    h = hashlib.sha256()
    with open(p, "rb") as f:
        for b in iter(lambda: f.read(1 << 20), b""):
            h.update(b)
    return h.hexdigest()
txt = open(f"{d}/list.json", encoding="utf-8", errors="replace").read()
m = re.search(r"\{.*\}", txt, re.S)
restores = []
for line in open(f"{d}/restore.tsv", encoding="utf-8", errors="replace"):
    kind, h, name, status, s, size, err = line.rstrip("\n").split("\t")
    restores.append({"kind": kind, "hash": h, "name": name, "status": status,
                     "sha256": s or None, "size": int(size) if size else None, "error": err or None})
errs = open(f"{d}/store-errors.log").read() if os.path.exists(f"{d}/store-errors.log") else ""
def tail(path, n=6):
    # Progress bars redraw with \r; keep only the final state of each line.
    text = open(path, encoding="utf-8", errors="replace").read()
    lines = [l.split("\r")[-1].strip() for l in text.split("\n")]
    return [l for l in lines if l][-n:]
store_logs = {n: tail(f"{d}/store-{n}.log") for n in dict.fromkeys(inputs.split())}
print(json.dumps({
    "mode": mode,
    "commit": commit,
    "flags": flags.split(),
    "password": "fixture-pass" if "--password" in flags else None,
    "archive": f"{mode}.tar.zst",
    "inputs": [{"name": n, "sha256": sha(os.path.join(indir, n)), "size": os.path.getsize(os.path.join(indir, n))} for n in inputs.split()],
    "store_errors": errs or None,
    "store_logs": store_logs,
    "volume_files": {p: {"sha256": s, "size": int(n)} for p, s, n in
                     (l.rstrip("\n").split("\t") for l in open(os.path.join(indir, "volume-files.tsv"), encoding="utf-8") if l.strip())},
    "list": json.loads(m.group(0)) if m else None,
    "list_error": None if m else txt.strip()[-400:],
    "restores": restores,
}, sort_keys=True))
PY

  # Pack the database. Badger's value log is preallocated, so it is mostly
  # zeros and compresses to almost nothing.
  wslx --exec sh -euc 'cd "$1" && tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@0 -cf - data | zstd -q -19 -o "$2" -f' \
    sh "$(to_wsl "$mdir")" "$(to_wsl "$out")/$mode.tar.zst"
done

python3 - "$golden" "$out/golden.json" <<'PY'
import json, sys
rows = [json.loads(l) for l in open(sys.argv[1], encoding="utf-8")]
with open(sys.argv[2], "w", encoding="utf-8", newline="\n") as f:
    json.dump({"schema": 1, "fixtures": rows}, f, indent=2, sort_keys=True)
    f.write("\n")
PY
ls -la "$out"
du -sh "$out"

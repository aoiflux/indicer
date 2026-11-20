#!/usr/bin/env python3
"""Helpers for gen_fixtures.sh.

  fixtures.py content SEED SIZE              write SIZE deterministic bytes to stdout
  fixtures.py scan MOUNTDIR > truth.jsonl    record every entry under a mounted volume
  fixtures.py manifest ARGS...               write a fixture manifest (see main)

A manifest is the ground truth a parser test checks a fixture image against:
the image digest and size, its partitions, and for every file the path, kind,
size and SHA-256 of its content, plus the properties the test expects the
parser to report (deleted, fragmented, sparse, inline, alternate streams,
hard links). It deliberately holds no offsets: where a file lives on disk is
the parser's job to find, not the test's to assume.
"""

import hashlib
import json
import os
import random
import stat
import subprocess
import sys


def content(seed, size):
    r = random.Random(int(seed))
    out = sys.stdout.buffer
    left = int(size)
    while left > 0:
        n = min(left, 1 << 16)
        out.write(r.randbytes(n))
        left -= n


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for b in iter(lambda: f.read(1 << 20), b""):
            h.update(b)
    return h.hexdigest()


def extent_count(path):
    """Number of extents per FIEMAP (filefrag), or None if unsupported."""
    try:
        out = subprocess.run(["filefrag", path], capture_output=True, text=True, check=True).stdout
    except (OSError, subprocess.CalledProcessError):
        return None
    # "path: 3 extents found"
    try:
        return int(out.rsplit(":", 1)[1].split()[0])
    except (IndexError, ValueError):
        return None


def extent_flags(path):
    """Flags from `filefrag -v`: unwritten, inline, and whether holes exist."""
    flags = set()
    try:
        out = subprocess.run(["filefrag", "-v", path], capture_output=True, text=True, check=True).stdout
    except (OSError, subprocess.CalledProcessError):
        return flags
    last_end = 0
    for line in out.splitlines():
        parts = line.split(":")
        if len(parts) < 4 or not parts[0].strip().isdigit():
            continue
        logical = parts[1].replace("..", " ").split()
        if len(logical) == 2 and int(logical[0]) > last_end:
            flags.add("sparse")
        if len(logical) == 2:
            last_end = int(logical[1]) + 1
        if "unwritten" in line:
            flags.add("unwritten")
        if "inline" in line:
            flags.add("inline")
    return flags


def scan(root):
    """Print one JSON object per entry under root, sorted by path."""
    rows = []
    inodes = {}
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames.sort()
        for name in sorted(dirnames) + sorted(filenames):
            full = os.path.join(dirpath, name)
            rel = "/" + os.path.relpath(full, root).replace(os.sep, "/")
            st = os.lstat(full)
            row = {"path": rel}
            if stat.S_ISLNK(st.st_mode):
                row.update(kind="symlink", target=os.readlink(full), size=st.st_size)
            elif stat.S_ISDIR(st.st_mode):
                row.update(kind="dir")
            elif stat.S_ISREG(st.st_mode):
                row.update(kind="file", size=st.st_size, sha256=sha256_file(full))
                n = extent_count(full)
                flags = sorted(extent_flags(full))
                if n is not None:
                    row["extents"] = n
                    if n > 1:
                        flags.append("fragmented")
                if st.st_size > 0 and st.st_blocks * 512 < st.st_size:
                    flags.append("sparse")
                if flags:
                    row["flags"] = sorted(set(flags))
                if st.st_nlink > 1:
                    inodes.setdefault(st.st_ino, []).append(rel)
            else:
                continue
            rows.append(row)
    for group in inodes.values():
        for row in rows:
            if row["path"] in group:
                row["links"] = sorted(group)
    for row in sorted(rows, key=lambda r: r["path"]):
        print(json.dumps(row, sort_keys=True))


def read_jsonl(path):
    with open(path, encoding="utf-8") as f:
        return [json.loads(l) for l in f if l.strip()]


def from_answer_key(path):
    """Turn an fsagen answer-key.jsonl into manifest file rows."""
    files = {}
    deleted = []
    for ev in read_jsonl(path):
        kind = ev.get("event")
        p = "/" + ev.get("path", "").lstrip("/")
        if kind == "created" and ev.get("kind") == "file":
            files[p] = {"path": p, "kind": "file", "size": ev["size"], "sha256": ev["sha256"]}
        elif kind == "created" and ev.get("kind") == "dir":
            files[p] = {"path": p, "kind": "dir"}
        elif kind in ("updated", "appended", "truncated", "edited") and p in files:
            if "size" in ev:
                files[p]["size"] = ev["size"]
            if "sha256" in ev:
                files[p]["sha256"] = ev["sha256"]
        elif kind == "renamed" and p in files:
            row = files.pop(p)
            row["path"] = "/" + ev["new_path"].lstrip("/")
            files[row["path"]] = row
        elif kind == "deleted":
            files.pop(p, None)
        elif kind == "stream" and p in files:
            files[p].setdefault("streams", []).append(
                {"name": ev["stream"], "size": ev["size"], "sha256": ev["sha256"]})
        elif kind == "linked":
            src = "/" + ev["from"].lstrip("/")
            if src in files:
                row = dict(files[src])
                row["path"] = p
                files[p] = row
                group = sorted({src, p} | set(files[src].get("links", [])))
                for q in group:
                    if q in files:
                        files[q]["links"] = group
        elif kind == "residue":
            res = ev["residue"]
            deleted.append({
                "path": p, "kind": "file", "deleted": True,
                "size": res.get("size"), "sha256": res.get("sha256"),
                "recoverable": res.get("recoverable"),
                "entry_overwritten": res.get("entry", {}).get("overwritten"),
            })
    # Implicit parent directories.
    for p in list(files):
        parts = p.strip("/").split("/")[:-1]
        for i in range(1, len(parts) + 1):
            d = "/" + "/".join(parts[:i])
            files.setdefault(d, {"path": d, "kind": "dir"})
    return sorted(files.values(), key=lambda r: r["path"]), deleted


def manifest(args):
    """manifest IMAGE FORMAT GENERATOR OUT [--part OFFSET:FS:TRUTH[:DELETED]]... [--answer-key OFFSET:FS:PATH]..."""
    image, fmt, generator, out = args[:4]
    rest = args[4:]
    parts = []
    decoded = None
    i = 0
    while i < len(rest):
        flag, val = rest[i], rest[i + 1]
        i += 2
        if flag == "--part":
            fields = val.split(":")
            offset, fs, truth = int(fields[0]), fields[1], fields[2]
            files = read_jsonl(truth)
            deleted = read_jsonl(fields[3]) if len(fields) > 3 and fields[3] else []
            parts.append({"offset": offset, "fs": fs, "files": files, "deleted": deleted})
        elif flag == "--answer-key":
            offset, fs, path = val.split(":", 2)
            files, deleted = from_answer_key(path)
            parts.append({"offset": int(offset), "fs": fs, "files": files, "deleted": deleted})
        elif flag == "--empty":
            offset, fs = val.split(":")
            parts.append({"offset": int(offset), "fs": fs, "files": [], "deleted": []})
        elif flag == "--decoded":
            decoded = val
        else:
            raise SystemExit(f"unknown flag {flag}")
    # IMAGE may list several container segments separated by commas; size and
    # sha256 then describe the decoded disk given by --decoded.
    segments = image.split(",")
    raw = decoded or segments[0]
    doc = {
        "schema": 1,
        "image": os.path.basename(segments[0]),
        "segments": [os.path.basename(s) for s in segments],
        "format": fmt,
        "size": os.path.getsize(raw),
        "sha256": sha256_file(raw),
        "generator": generator,
        "partitions": parts,
    }
    with open(out, "w", encoding="utf-8", newline="\n") as f:
        json.dump(doc, f, indent=1, sort_keys=True)
        f.write("\n")


def main():
    cmd = sys.argv[1]
    if cmd == "content":
        content(sys.argv[2], sys.argv[3])
    elif cmd == "scan":
        scan(sys.argv[2])
    elif cmd == "manifest":
        manifest(sys.argv[2:])
    else:
        raise SystemExit(f"unknown command {cmd}")


if __name__ == "__main__":
    main()

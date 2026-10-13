#!/usr/bin/env bash
# sync.sh copies the working tree (uncommitted changes included) to an ext4
# directory inside WSL and normalises text files to LF, so Linux tests run on
# a native filesystem with the line endings the repository stores.
#
#   scripts/wsl/sync.sh [DEST]     (default ~/.cache/dues-verify/src)
#
# Run it inside WSL from the repository checkout under /mnt.
set -euo pipefail

src="$(cd "$(dirname "$0")/../.." && pwd)"
dest="${1:-$HOME/.cache/dues-verify/src}"
mkdir -p "$dest"

rsync -a --delete \
	--exclude .git/ --exclude .codegraph/ --exclude .vscode/ --exclude .claude/ \
	--exclude bench/ --exclude data/ --exclude tdata/ --exclude dist/ \
	--exclude '/log/' --exclude '*/log/' --exclude '*.exe' \
	"$src/" "$dest/"

# Line endings: the Windows checkout is CRLF (core.autocrlf=true).
find "$dest" -type f \( -name '*.go' -o -name '*.sh' -o -name '*.py' -o -name '*.md' \
	-o -name '*.json' -o -name '*.yaml' -o -name '*.yml' -o -name '*.txt' -o -name '*.env' \
	-o -name 'go.mod' -o -name 'go.sum' -o -name '*.proto' \) \
	-not -path '*/testdata/*' -print0 | xargs -0 -r sed -i 's/\r$//'
chmod +x "$dest"/scripts/*.sh "$dest"/scripts/wsl/*.sh 2>/dev/null || true
echo "synced $src -> $dest"

#!/usr/bin/env bash
# suite.sh runs the Linux-only part of the verify gate inside WSL, against the
# tree sync.sh copied. Run it as the normal user (permission tests need a
# non-root user); it calls root only to set up mounts.
#
#   scripts/wsl/suite.sh [--no-fuzz] [--long]
#
# Steps: race tests, faultinject tests (including subprocess kill points),
# filesystem-failure tests on the special mounts, fuzz smoke, and with --long
# the long tests (DUES_LONG_TESTS=1, without the race detector).
set -uo pipefail

cd "$(dirname "$0")/../.."
fuzz=1
long=0
for arg in "$@"; do
	case "$arg" in
	--no-fuzz) fuzz=0 ;;
	--long) long=1 ;;
	*) echo "suite.sh: unknown argument $arg" >&2; exit 2 ;;
	esac
done

results=()
failed=0
step() {
	local name="$1"
	shift
	echo "== $name"
	if "$@"; then results+=("PASS  $name"); else results+=("FAIL  $name"); failed=1; fi
}

mounts_up() {
	if [ "$(id -u)" = 0 ]; then
		bash scripts/wsl/mounts.sh up
	else
		sudo -n bash scripts/wsl/mounts.sh up 2>/dev/null ||
			wsl.exe -u root --exec bash "$(pwd)/scripts/wsl/mounts.sh" up
	fi
}

fs_suite() {
	mounts_up || return 1
	eval "$(bash scripts/wsl/mounts.sh env)"
	export DUES_TEST_ENOSPC_DIR DUES_TEST_LOWSPACE_DIR DUES_TEST_RO_DIR DUES_TEST_RO_SRC DUES_TEST_RO_DB
	go test -count=1 -run 'ENOSPC|ReadOnly|Permission|DiskFull' ./...
}

fuzz_run() {
	local rc=0
	while read -r pkg fn; do
		case "$pkg" in "" | \#*) continue ;; esac
		# Strict: a panic internal/safe would contain is still a finding.
		DUES_SAFE_STRICT=1 go test "$pkg" -run '^$' -fuzz "^$fn\$" -fuzztime "${FUZZTIME:-60s}" || rc=1
	done <scripts/fuzz-targets.txt
	return $rc
}

step "test (race)" go test -race -count=1 ./...
step "test (faultinject)" go test -count=1 -tags faultinject ./...
step "filesystem failures" fs_suite
[ $long = 1 ] && step "long tests" env DUES_LONG_TESTS=1 go test -count=1 -timeout 90m ./...
[ $fuzz = 1 ] && step "fuzz smoke" fuzz_run

echo
echo "== WSL summary"
printf '%s\n' "${results[@]}"
exit $failed

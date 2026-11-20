#!/usr/bin/env bash
# suite.sh runs the Linux-only part of the verify gate inside WSL, against the
# tree sync.sh copied. Run it as the normal user (permission tests need a
# non-root user); it calls root only to set up mounts.
#
#   scripts/wsl/suite.sh [--fuzztime DURATION] [--no-fuzz] [--long]
#
# Steps: race tests, faultinject tests (including subprocess kill points),
# filesystem-failure tests on the special mounts, fuzz smoke (each target for
# --fuzztime, default 60s), and with --long the long tests (-tags long,
# without the race detector). The race and faultinject steps build with
# -tags requirepty, so the pseudo-terminal tests fail rather than skip.
set -uo pipefail

cd "$(dirname "$0")/../.."
fuzz=1
long=0
fuzztime=60s
while [ $# -gt 0 ]; do
	case "$1" in
	--fuzztime)
		[ $# -ge 2 ] || { echo "suite.sh: --fuzztime needs a duration" >&2; exit 2; }
		fuzztime="$2"
		shift
		;;
	--no-fuzz) fuzz=0 ;;
	--long) long=1 ;;
	*) echo "suite.sh: unknown argument $1" >&2; exit 2 ;;
	esac
	shift
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

# The packages whose tests use the mounts. A test binary refuses a flag it
# does not define, so the run names them; a package that starts using a
# mount (testutil.ENOSPCDir and the rest) must be added here, or its test
# skips.
fs_packages=(./internal/e2e ./internal/diskspace ./internal/products/compression_product)

fs_suite() {
	mounts_up || return 1
	local args
	mapfile -t args < <(bash scripts/wsl/mounts.sh args) || return 1
	# -p 1: the packages share the small volume, which their tests fill.
	go test -p 1 -count=1 -run 'ENOSPC|ReadOnly|Permission|DiskFull' "${fs_packages[@]}" -args "${args[@]}"
}

fuzz_run() {
	local rc=0
	while read -r pkg fn; do
		case "$pkg" in "" | \#*) continue ;; esac
		# -tags safestrict: a panic internal/safe would contain is still a
		# finding. Go minimises each input that finds new coverage for up to
		# 60s by default. In a 60s smoke that kept every worker minimising
		# (0 execs/sec), and the unfinished inputs never reached the cache,
		# so every run repeated it.
		go test -tags safestrict "$pkg" -run '^$' -fuzz "^$fn\$" -fuzztime "$fuzztime" \
			-fuzzminimizetime 5s || rc=1
	done <scripts/fuzz-targets.txt
	return $rc
}

# The race detector slows the cli package to six or seven minutes on its own,
# and past the default ten when the packages run side by side.
step "test (race)" go test -race -count=1 -timeout 30m -tags requirepty ./...
step "test (faultinject)" go test -count=1 -timeout 30m -tags faultinject,requirepty ./...
step "filesystem failures" fs_suite
[ $long = 1 ] && step "long tests" go test -count=1 -timeout 90m -tags long ./...
[ $fuzz = 1 ] && step "fuzz smoke" fuzz_run

echo
echo "== WSL summary"
printf '%s\n' "${results[@]}"
exit $failed

#!/usr/bin/env bash
# verify.sh runs the DUES verification gate on Linux (natively or in WSL).
#
#   scripts/verify.sh [--fast] [--no-vuln] [--no-race] [--no-fuzz]
#
# Steps: format, vet (normal and faultinject builds), cross builds with a
# check that release binaries carry no faultinject tag, staticcheck,
# govulncheck, tests, race tests, faultinject tests, and fuzz smoke.
# --fast skips race, fuzz and govulncheck. Prints a pass/fail summary and
# exits non-zero if any step failed.
set -uo pipefail

cd "$(dirname "$0")/.."
# shellcheck disable=SC1091
. scripts/tools.env

fast=0 vuln=1 race=1 fuzz=1
for a in "$@"; do
	case "$a" in
	--fast) fast=1 vuln=0 race=0 fuzz=0 ;;
	--no-vuln) vuln=0 ;;
	--no-race) race=0 ;;
	--no-fuzz) fuzz=0 ;;
	*) echo "unknown option $a" >&2; exit 2 ;;
	esac
done

results=()
failed=0
step() { # NAME CMD...
	local name="$1"
	shift
	echo "== $name"
	if "$@"; then
		results+=("PASS  $name")
	else
		results+=("FAIL  $name")
		failed=1
	fi
}

fmt_check() {
	local out
	out="$(gofmt -l $(git ls-files -co --exclude-standard '*.go' | grep -v '/testdata/'))"
	[ -z "$out" ] || { echo "$out"; return 1; }
}

cross_build() {
	local tmp
	tmp="$(mktemp -d)"
	local rc=0
	for target in windows/amd64 windows/arm64 linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
		local os="${target%/*}" arch="${target#*/}"
		for pkg in . ./cmd/compression; do
			local bin="$tmp/$(basename "$pkg")-$os-$arch"
			[ "$pkg" = . ] && bin="$tmp/dues-$os-$arch"
			if ! CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -tags netgo,osusergo -o "$bin" "$pkg"; then
				rc=1
				continue
			fi
			if go version -m "$bin" | grep -q faultinject; then
				echo "release binary $bin was built with the faultinject tag"
				rc=1
			fi
		done
	done
	rm -rf "$tmp"
	return $rc
}

staticcheck_run() {
	GOTOOLCHAIN="$ANALYSIS_TOOLCHAIN" go run "$STATICCHECK" ./... &&
		GOTOOLCHAIN="$ANALYSIS_TOOLCHAIN" go run "$STATICCHECK" -tags faultinject ./...
}

vuln_run() {
	local tmp
	tmp="$(mktemp -d)"
	go run "$GOVULNCHECK" ./... &&
		CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "$tmp/dues" . &&
		go run "$GOVULNCHECK" -mode=binary "$tmp/dues"
	local rc=$?
	rm -rf "$tmp"
	return $rc
}

fuzz_run() {
	local rc=0
	while read -r pkg fn; do
		case "$pkg" in "" | \#*) continue ;; esac
		echo "-- fuzz $pkg $fn"
		# A short minimise time keeps the smoke fuzzing; see scripts/wsl/suite.sh.
		go test "$pkg" -run '^$' -fuzz "^$fn\$" -fuzztime "${FUZZTIME:-60s}" \
			-fuzzminimizetime 5s || rc=1
	done <scripts/fuzz-targets.txt
	return $rc
}

step "format" fmt_check
step "vet" go vet ./...
step "vet (faultinject)" go vet -tags faultinject ./...
step "cross build (6 targets)" cross_build
[ $fast = 1 ] || step "staticcheck" staticcheck_run
[ $vuln = 1 ] && step "govulncheck" vuln_run
step "test" go test -count=1 ./...
step "test (faultinject)" go test -count=1 -tags faultinject ./...
[ $race = 1 ] && step "test (race)" go test -race -count=1 ./...
[ $fuzz = 1 ] && step "fuzz smoke" fuzz_run

echo
echo "== summary"
printf '%s\n' "${results[@]}"
exit $failed

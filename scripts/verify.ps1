<#
.SYNOPSIS
  The DUES verification gate on Windows, with the Linux part run in WSL.

.DESCRIPTION
  Runs, in order: CRLF-tolerant format check, go vet (normal and faultinject
  builds, and every test tag), the six-target release build with a check that no binary carries
  the faultinject tag, staticcheck, govulncheck (source and binary), the test
  suite (normal and faultinject builds), then in WSL: race tests,
  faultinject tests, filesystem-failure tests and fuzz smoke.

  Prints a pass/fail line per step and exits 1 if any step failed.

.PARAMETER Fast
  Skip staticcheck, govulncheck, WSL and fuzzing.

.PARAMETER NoWSL
  Skip the WSL part.

.PARAMETER NoVuln
  Skip govulncheck (it needs network access to the vulnerability database).

.PARAMETER NoFuzz
  Skip the fuzz smoke in WSL.

.PARAMETER FuzzTime
  How long each fuzz target runs in WSL, as a Go duration (default 60s).

.PARAMETER Long
  Also run the long tests (scale and soak; -tags long), on Windows and in
  WSL. They add some twenty minutes.
#>
param(
    [switch]$Fast,
    [switch]$NoWSL,
    [switch]$NoVuln,
    [switch]$NoFuzz,
    [string]$FuzzTime = "60s",
    [switch]$Long
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Continue"
$repo = Split-Path -Parent $PSScriptRoot
Set-Location $repo

$tools = @{}
foreach ($line in Get-Content (Join-Path $PSScriptRoot "tools.env")) {
    if ($line -match '^\s*([A-Z_]+)=(.+)$') { $tools[$Matches[1]] = $Matches[2].Trim() }
}

$results = [System.Collections.Generic.List[string]]::new()
$script:failed = $false

function Step([string]$Name, [scriptblock]$Body) {
    Write-Host "== $Name" -ForegroundColor Cyan
    $ok = $true
    try {
        & $Body
        if ($LASTEXITCODE -ne 0) { $ok = $false }
    } catch {
        Write-Host $_ -ForegroundColor Red
        $ok = $false
    }
    if ($ok) { $results.Add("PASS  $Name") } else { $results.Add("FAIL  $Name"); $script:failed = $true }
}

# With-Env sets Go's own variables (GOOS, GOARCH, CGO_ENABLED, GOTOOLCHAIN),
# the toolchain's interface, for the commands in Body. DUES and its tests take
# their settings from flags and build tags.
function With-Env([hashtable]$Vars, [scriptblock]$Body) {
    $saved = @{}
    foreach ($k in $Vars.Keys) {
        $saved[$k] = [Environment]::GetEnvironmentVariable($k)
        [Environment]::SetEnvironmentVariable($k, $Vars[$k])
    }
    try { & $Body } finally {
        foreach ($k in $saved.Keys) { [Environment]::SetEnvironmentVariable($k, $saved[$k]) }
    }
}

Step "format" { go run ./internal/tools/fmtcheck }
Step "vet" { go vet ./... }
Step "vet (faultinject)" { go vet -tags faultinject ./... }
# Every build tag the tests use, so vet compiles each tagged file at least
# once: the files for a tag's absence build in the normal vet.
Step "vet (test tags)" { go vet -tags faultinject,long,requirepty,safestrict,winconsole ./... }

Step "cross build (6 targets)" {
    $tmp = Join-Path ([IO.Path]::GetTempPath()) ("dues-verify-" + [Guid]::NewGuid())
    New-Item -ItemType Directory $tmp | Out-Null
    $bad = $false
    try {
        foreach ($t in "windows/amd64", "windows/arm64", "linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64") {
            $os, $arch = $t.Split("/")
            foreach ($pkg in ".", "./cmd/compression") {
                $name = if ($pkg -eq ".") { "dues" } else { "compression" }
                $bin = Join-Path $tmp "$name-$os-$arch"
                With-Env @{ CGO_ENABLED = "0"; GOOS = $os; GOARCH = $arch } {
                    go build -trimpath -tags netgo,osusergo -o $bin $pkg
                }
                if ($LASTEXITCODE -ne 0) { $bad = $true; continue }
                if ((go version -m $bin) -match "faultinject|safestrict") {
                    Write-Host "release binary $bin carries a test-only tag (faultinject or safestrict)" -ForegroundColor Red
                    $bad = $true
                }
            }
        }
    } finally { Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue }
    if ($bad) { $global:LASTEXITCODE = 1 } else { $global:LASTEXITCODE = 0 }
}

if (-not $Fast) {
    Step "staticcheck" {
        With-Env @{ GOTOOLCHAIN = $tools.ANALYSIS_TOOLCHAIN } {
            go run $tools.STATICCHECK ./...
            if ($LASTEXITCODE -eq 0) { go run $tools.STATICCHECK -tags faultinject ./... }
        }
    }
}

if (-not $Fast -and -not $NoVuln) {
    Step "govulncheck" {
        go run $tools.GOVULNCHECK ./...
        if ($LASTEXITCODE -ne 0) { return }
        $tmp = Join-Path ([IO.Path]::GetTempPath()) ("dues-vuln-" + [Guid]::NewGuid())
        New-Item -ItemType Directory $tmp | Out-Null
        try {
            $bin = Join-Path $tmp "dues"
            With-Env @{ CGO_ENABLED = "0"; GOOS = "linux"; GOARCH = "amd64" } { go build -trimpath -o $bin . }
            if ($LASTEXITCODE -eq 0) { go run $tools.GOVULNCHECK -mode=binary $bin }
        } finally { Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue }
    }
}

if ($Long) {
    Step "test (with long tests)" { go test -count=1 -timeout 90m -tags long ./... }
} else {
    Step "test" { go test -count=1 ./... }
}
Step "test (faultinject)" { go test -count=1 -tags faultinject ./... }

if (-not $Fast -and -not $NoWSL) {
    $wslRepo = (wsl.exe --exec wslpath -a ($repo -replace '\\', '/')).Trim()
    # The copy goes under the WSL account's home, as its password database
    # records it.
    $wslHome = (wsl.exe --exec bash -c 'getent passwd "$(id -u)" | cut -d: -f6').Trim()
    $wslSrc = "$wslHome/.cache/dues-verify/src"
    Step "WSL sync" { wsl.exe --exec bash "$wslRepo/scripts/wsl/sync.sh" $wslSrc }
    $suiteArgs = @("--fuzztime", $FuzzTime)
    if ($NoFuzz) { $suiteArgs += "--no-fuzz" }
    if ($Long) { $suiteArgs += "--long" }
    Step "WSL suite" { wsl.exe --exec bash -lc "cd '$wslSrc' && bash scripts/wsl/suite.sh $suiteArgs" }
}

Write-Host ""
Write-Host "== summary" -ForegroundColor Cyan
$results | ForEach-Object {
    $color = if ($_.StartsWith("PASS")) { "Green" } else { "Red" }
    Write-Host $_ -ForegroundColor $color
}
if ($script:failed) { exit 1 }
exit 0

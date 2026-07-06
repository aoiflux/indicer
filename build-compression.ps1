param(
    [string]$ProductName = "compression",
    [string]$CliPackage = "./cmd/compression",
    [string]$FfiPackage = "./cmd/ffi",
    [string]$OutputDir = "dist/compression",
    [string[]]$Targets = @("windows/amd64"),
    [ValidateSet("auto", "c-shared", "c-archive")]
    [string]$FfiBuildMode = "auto",
    [ValidateSet("cli", "ffi", "all")]
    [string]$Mode = "all",
    [ValidateSet("debug", "release")]
    [string]$BuildProfile = "debug"
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw "Go compiler not found in PATH."
}

$repoRoot = Split-Path -Parent $PSCommandPath
Set-Location $repoRoot
New-Item -ItemType Directory -Path $OutputDir -Force | Out-Null

$prevGoos = $env:GOOS
$prevGoarch = $env:GOARCH
$prevCgo = $env:CGO_ENABLED
$prevGoamd64 = $env:GOAMD64

$commonBuildArgs = @("-trimpath", "-buildvcs=false", "-tags", "notusk")
if ($BuildProfile -eq "release") {
    # -s -w strips symbol tables and DWARF debug info from Go artifacts.
    $commonBuildArgs += @("-ldflags", "-s -w")
    Write-Host "Build profile: release (stripped symbols/debug info)" -ForegroundColor DarkCyan
}
else {
    Write-Host "Build profile: debug" -ForegroundColor DarkGray
}

function Build-Cli {
    param([string]$Goos, [string]$Goarch)

    $ext = if ($Goos -eq "windows") { ".exe" } else { "" }
    $out = Join-Path $OutputDir ("{0}-{1}-{2}{3}" -f $ProductName, $Goos, $Goarch, $ext)

    Write-Host "[CLI] $Goos/$Goarch -> $out" -ForegroundColor Cyan

    $env:GOOS = $Goos
    $env:GOARCH = $Goarch
    $env:GOAMD64 = "v1"
    $env:CGO_ENABLED = "0"

    $args = @("build") + $commonBuildArgs + @("-o", $out, $CliPackage)
    & go @args
    if ($LASTEXITCODE -ne 0) { throw "CLI build failed for $Goos/$Goarch" }
}

function Build-Ffi {
    param([string]$Goos, [string]$Goarch)

    $resolvedFfiBuildMode = $FfiBuildMode
    if ($resolvedFfiBuildMode -eq "auto") {
        if ($Goos -eq "windows") {
            $resolvedFfiBuildMode = "c-shared"
        }
        else {
            $resolvedFfiBuildMode = "c-archive"
        }
    }

    $base = Join-Path $OutputDir ("dues_engine-{0}-{1}" -f $Goos, $Goarch)
    $ext = ".a"
    if ($resolvedFfiBuildMode -eq "c-shared") {
        if ($Goos -eq "windows") {
            $ext = ".dll"
        }
        elseif ($Goos -eq "darwin") {
            $ext = ".dylib"
        }
        else {
            $ext = ".so"
        }
    }
    $out = "$base$ext"

    Write-Host "[FFI:$resolvedFfiBuildMode] $Goos/$Goarch -> $out" -ForegroundColor Yellow
    if ($Goos -eq "windows" -and $resolvedFfiBuildMode -eq "c-archive") {
        Write-Warning "windows c-archive output is GNU .a and is typically not linkable by MSVC toolchains. For Flutter on Windows, prefer -FfiBuildMode c-shared and load the DLL via dart:ffi."
    }

    $env:GOOS = $Goos
    $env:GOARCH = $Goarch
    $env:GOAMD64 = "v1"
    $env:CGO_ENABLED = "1"

    $args = @("build") + $commonBuildArgs + @("-buildmode", $resolvedFfiBuildMode, "-o", $out, $FfiPackage)
    & go @args
    if ($LASTEXITCODE -ne 0) { throw "FFI build failed for $Goos/$Goarch" }
}

try {
    foreach ($target in $Targets) {
        $parts = $target.Split("/")
        if ($parts.Count -ne 2) {
            throw "Invalid target '$target'. Expected os/arch."
        }

        $goos = $parts[0].Trim().ToLowerInvariant()
        $goarch = $parts[1].Trim().ToLowerInvariant()

        switch ($Mode) {
            "cli" { Build-Cli -Goos $goos -Goarch $goarch }
            "ffi" { Build-Ffi -Goos $goos -Goarch $goarch }
            "all" {
                Build-Cli -Goos $goos -Goarch $goarch
                Build-Ffi -Goos $goos -Goarch $goarch
            }
        }
    }
}
finally {
    $env:GOOS = $prevGoos
    $env:GOARCH = $prevGoarch
    $env:CGO_ENABLED = $prevCgo
    $env:GOAMD64 = $prevGoamd64
}

Write-Host "Build complete -> $OutputDir" -ForegroundColor Green

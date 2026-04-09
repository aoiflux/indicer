param(
    [string]$BinaryName = "dues",
    [string]$MainPackage = ".",
    [string]$OutputDir = "dist",
    [string[]]$Targets = @(
        "windows/amd64",
        "windows/arm64",
        "linux/amd64",
        "linux/arm64",
        "darwin/amd64",
        "darwin/arm64"
    ),
    [switch]$NoClean
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

function Write-Banner {
    param(
        [string]$Title,
        [ConsoleColor]$Color = [ConsoleColor]::Cyan
    )

    $line = "=" * 72
    Write-Host ""
    Write-Host $line -ForegroundColor DarkGray
    Write-Host ("  " + $Title) -ForegroundColor $Color
    Write-Host $line -ForegroundColor DarkGray
}

function Write-Label {
    param(
        [string]$Label,
        [string]$Value,
        [ConsoleColor]$LabelColor = [ConsoleColor]::DarkGray,
        [ConsoleColor]$ValueColor = [ConsoleColor]::White
    )

    Write-Host ($Label.PadRight(16)) -NoNewline -ForegroundColor $LabelColor
    Write-Host $Value -ForegroundColor $ValueColor
}

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw "Go compiler not found in PATH. Install Go and retry."
}

$repoRoot = Split-Path -Parent $PSCommandPath
Set-Location $repoRoot

if (-not $NoClean -and (Test-Path $OutputDir)) {
    Remove-Item -Recurse -Force $OutputDir
}

New-Item -ItemType Directory -Path $OutputDir -Force | Out-Null

$prevGoos = $env:GOOS
$prevGoarch = $env:GOARCH
$prevCgo = $env:CGO_ENABLED
$prevGoamd64 = $env:GOAMD64

$ldflags = "-s -w -buildid= -extldflags '-static'"
$baseArgs = @(
    "build",
    "-trimpath",
    "-buildvcs=false",
    "-tags", "netgo,osusergo",
    "-ldflags", $ldflags
)

$buildStart = Get-Date
$targetCount = $Targets.Count

Write-Banner -Title "DUES Release Build"
Write-Label -Label "Repository" -Value $repoRoot -ValueColor Cyan
Write-Label -Label "Output Dir" -Value (Resolve-Path $OutputDir).Path -ValueColor Cyan
Write-Label -Label "Targets" -Value ($Targets -join ", ") -ValueColor Yellow
Write-Label -Label "Mode" -Value "Optimized static-style release build" -ValueColor Green

$pgoFile = Join-Path $repoRoot "default.pgo"
if (Test-Path $pgoFile) {
    $baseArgs += @("-pgo", $pgoFile)
    Write-Label -Label "PGO" -Value ("Enabled (" + $pgoFile + ")") -ValueColor Green
}
else {
    Write-Label -Label "PGO" -Value "Not found (building without -pgo)" -ValueColor DarkYellow
}

try {
    for ($i = 0; $i -lt $targetCount; $i++) {
        $target = $Targets[$i]
        $parts = $target.Split("/")
        if ($parts.Count -ne 2) {
            throw "Invalid target '$target'. Expected format: os/arch (example: linux/amd64)."
        }

        $goos = $parts[0].Trim().ToLowerInvariant()
        $goarch = $parts[1].Trim().ToLowerInvariant()

        if ([string]::IsNullOrWhiteSpace($goos) -or [string]::IsNullOrWhiteSpace($goarch)) {
            throw "Invalid target '$target'. OS and ARCH must not be empty."
        }

        $env:GOOS = $goos
        $env:GOARCH = $goarch
        $env:CGO_ENABLED = "0"

        # Reset amd64 tuning unless the target is amd64.
        Remove-Item Env:GOAMD64 -ErrorAction SilentlyContinue
        if ($goarch -eq "amd64") {
            $env:GOAMD64 = "v1"
        }

        $ext = ""
        if ($goos -eq "windows") {
            $ext = ".exe"
        }

        $outputName = "$BinaryName-$goos-$goarch$ext"
        $outputPath = Join-Path $OutputDir $outputName

        $percent = [int](($i / [double]$targetCount) * 100)
        Write-Progress -Id 1 -Activity "Building release artifacts" -Status ("Starting " + $outputName) -PercentComplete $percent

        Write-Host ""
        Write-Host ("[" + ($i + 1) + "/" + $targetCount + "] ") -NoNewline -ForegroundColor DarkGray
        Write-Host $outputName -NoNewline -ForegroundColor Yellow
        Write-Host "  (CGO=0, trimpath, stripped)" -ForegroundColor DarkCyan

        $args = @()
        $args += $baseArgs
        $args += @("-o", $outputPath, $MainPackage)

        $stepStart = Get-Date
        & go @args
        if ($LASTEXITCODE -ne 0) {
            throw "go build failed for target $target"
        }

        $duration = ((Get-Date) - $stepStart).TotalSeconds
        $sizeMb = [math]::Round(((Get-Item $outputPath).Length / 1MB), 2)
        Write-Host "  OK" -NoNewline -ForegroundColor Green
        Write-Host ("  " + $sizeMb + " MB") -NoNewline -ForegroundColor Cyan
        Write-Host ("  " + ([math]::Round($duration, 2)) + "s") -ForegroundColor DarkGray

        $percentDone = [int]((($i + 1) / [double]$targetCount) * 100)
        Write-Progress -Id 1 -Activity "Building release artifacts" -Status ("Completed " + $outputName) -PercentComplete $percentDone
    }
}
finally {
    Write-Progress -Id 1 -Activity "Building release artifacts" -Completed

    $env:GOOS = $prevGoos
    $env:GOARCH = $prevGoarch
    $env:CGO_ENABLED = $prevCgo

    if ($null -eq $prevGoamd64) {
        Remove-Item Env:GOAMD64 -ErrorAction SilentlyContinue
    }
    else {
        $env:GOAMD64 = $prevGoamd64
    }
}

$buildSeconds = [math]::Round(((Get-Date) - $buildStart).TotalSeconds, 2)
$artifacts = Get-ChildItem $OutputDir | Sort-Object Name

Write-Banner -Title "Build Complete" -Color Green
Write-Label -Label "Duration" -Value ($buildSeconds.ToString() + "s") -ValueColor Green
Write-Label -Label "Artifacts" -Value $artifacts.Count -ValueColor Green
Write-Host ""

$artifacts |
Select-Object @{
    Name       = "Binary"
    Expression = { $_.Name }
}, @{
    Name       = "Size(MB)"
    Expression = { [math]::Round($_.Length / 1MB, 2) }
}, LastWriteTime |
Format-Table -AutoSize

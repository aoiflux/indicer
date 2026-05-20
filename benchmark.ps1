param(
    [int]$Count = 5,
    [string]$Benchtime = "2s",
    [switch]$Profiles,
    [string]$OutputDir = "bench\outputs"
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

$fullRunFile = Join-Path $OutputDir "full_run.txt"
$summaryFile = Join-Path $OutputDir "summary.txt"

function Run-Bench {
    param(
        [string]$Name,
        [string]$Package,
        [string]$Pattern,
        [switch]$WithProfiles
    )

    $outFile = Join-Path $OutputDir ($Name + ".txt")
    $goArgs = @(
        "test", $Package,
        "-run", "^$",
        "-bench", $Pattern,
        "-benchmem",
        "-count", $Count,
        "-benchtime", $Benchtime
    )

    if ($WithProfiles) {
        $cpu = Join-Path $OutputDir ($Name + ".cpu.pprof")
        $mem = Join-Path $OutputDir ($Name + ".mem.pprof")
        $goArgs += @("-cpuprofile", $cpu, "-memprofile", $mem)
    }

    Write-Host "Running benchmark: $Name" -ForegroundColor Cyan
    & go @goArgs | Tee-Object -FilePath $outFile | Tee-Object -FilePath $fullRunFile -Append
}

function Get-NsOpFromFile {
    param(
        [string]$FilePath,
        [string]$BenchPattern
    )

    if (-not (Test-Path $FilePath)) {
        return $null
    }

    $line = Select-String -Path $FilePath -Pattern $BenchPattern | Select-Object -Last 1
    if (-not $line) {
        return $null
    }

    $match = [regex]::Match($line.Line, '\s+([0-9]+)\s+ns/op')
    if (-not $match.Success) {
        return $null
    }

    return [double]$match.Groups[1].Value
}

function Write-RevRelModeSummary {
    param([string]$HotspotFile)

    $unbufferedPattern = "BenchmarkIngestMetadataPathHotspots/process_revrel_dual_write_unbuffered/files_32/chunks_16"
    $bufferedPattern = "BenchmarkIngestMetadataPathHotspots/process_revrel_dual_write_buffered/files_32/chunks_16"

    $unbufferedNs = Get-NsOpFromFile -FilePath $HotspotFile -BenchPattern $unbufferedPattern
    $bufferedNs = Get-NsOpFromFile -FilePath $HotspotFile -BenchPattern $bufferedPattern

    $lines = @(
        "",
        "=== Reverse-Relation Dual-Write Mode Summary ==="
    )

    if ($null -eq $unbufferedNs -or $null -eq $bufferedNs) {
        $lines += "Buffered/unbuffered hotspot lines not found in $HotspotFile"
    }
    else {
        $improvementPct = 0.0
        if ($unbufferedNs -gt 0) {
            $improvementPct = (($unbufferedNs - $bufferedNs) / $unbufferedNs) * 100.0
        }

        $lines += "unbuffered_ns_op: $([math]::Round($unbufferedNs, 0))"
        $lines += "buffered_ns_op: $([math]::Round($bufferedNs, 0))"
        $lines += "buffered_improvement_percent: $([math]::Round($improvementPct, 2))"
    }

    $lines | Tee-Object -FilePath $summaryFile | Out-Null
    $lines | Add-Content -Path $fullRunFile
}

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw "Go compiler not found in PATH. Install Go and retry."
}

$repoRoot = Split-Path -Parent $PSCommandPath
Set-Location $repoRoot

New-Item -ItemType Directory -Force -Path $OutputDir | Out-Null
if (Test-Path $fullRunFile) {
    Remove-Item $fullRunFile -Force
}
if (Test-Path $summaryFile) {
    Remove-Item $summaryFile -Force
}

Run-Bench -Name "service_getchonkmap" -Package "./lib/service" -Pattern "^BenchmarkGetChonkMap$" -WithProfiles:$Profiles
Run-Bench -Name "store_ingest_path" -Package "./lib/store" -Pattern "^BenchmarkIngestDataPath$" -WithProfiles:$Profiles
Run-Bench -Name "store_ingest_metadata_path" -Package "./lib/store" -Pattern "^BenchmarkIngestMetadataPath$" -WithProfiles:$Profiles
Run-Bench -Name "store_ingest_metadata_hotspots" -Package "./lib/store" -Pattern "^BenchmarkIngestMetadataPathHotspots$" -WithProfiles:$Profiles
Run-Bench -Name "store_revrel_path" -Package "./lib/store" -Pattern "^BenchmarkProcessRevRel$" -WithProfiles:$Profiles
Run-Bench -Name "store_repair_scan" -Package "./lib/store" -Pattern "^BenchmarkInspectEvidenceRepairs$" -WithProfiles:$Profiles
Run-Bench -Name "store_restore_path" -Package "./lib/store" -Pattern "^BenchmarkRestoreDataPath$" -WithProfiles:$Profiles

$hotspotFile = Join-Path $OutputDir "store_ingest_metadata_hotspots.txt"
Write-RevRelModeSummary -HotspotFile $hotspotFile

Write-Host "Benchmark run complete. Outputs in $OutputDir" -ForegroundColor Green
Write-Host "Summary: $summaryFile" -ForegroundColor Green
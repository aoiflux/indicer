param(
    [int]$Count = 5,
    [string]$Benchtime = "2s",
    [switch]$Profiles,
    [string]$OutputDir = "bench\outputs",
    [string]$StoreDataset = "",
    [string]$StoreExe = "",
    [int]$StoreCount = 3,
    [int[]]$IOUringQueueDepths = @(256, 512, 1024, 2048),
    [string]$StoreDbRoot = "bench\store_matrix"
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

function Get-Median {
    param([double[]]$Values)

    if ($Values.Count -eq 0) {
        return [double]::NaN
    }

    $sorted = $Values | Sort-Object
    $n = $sorted.Count
    if ($n % 2 -eq 1) {
        return [double]$sorted[[int]($n / 2)]
    }

    $a = [double]$sorted[($n / 2) - 1]
    $b = [double]$sorted[$n / 2]
    return ($a + $b) / 2.0
}

function Get-StoreDurationSeconds {
    param(
        [string]$Exe,
        [string[]]$ExecutionParams
    )

    $output = & $Exe @ExecutionParams 2>&1 | Out-String
    $m = [regex]::Match($output, 'Stored in:\s*([0-9.]+)s')
    if (-not $m.Success) {
        throw "Could not parse store duration from output. Command: $Exe $($ExecutionParams -join ' '). Output:`n$output"
    }

    return [double]$m.Groups[1].Value
}

function Write-IOUringQueueDepthSummary {
    if ([string]::IsNullOrWhiteSpace($StoreDataset)) {
        return
    }

    if (-not (Test-Path $StoreDataset)) {
        throw "Store dataset path not found: $StoreDataset"
    }
    if ($StoreCount -lt 1) {
        throw "StoreCount must be >= 1"
    }
    if ($null -eq $IOUringQueueDepths -or $IOUringQueueDepths.Count -eq 0) {
        throw "IOUringQueueDepths must contain at least one depth"
    }

    $exe = $StoreExe
    $exePrefix = @()
    if ([string]::IsNullOrWhiteSpace($exe)) {
        if (Test-Path ".\\dues.exe") {
            $exe = ".\\dues.exe"
        }
        else {
            $exe = "go"
            $exePrefix = @("run", ".")
        }
    }
    elseif (-not (Get-Command $exe -ErrorAction SilentlyContinue) -and -not (Test-Path $exe)) {
        throw "Store executable not found: $exe"
    }

    New-Item -ItemType Directory -Force -Path $StoreDbRoot | Out-Null

    $lines = @(
        "",
        "=== io_uring Queue-Depth Sweep (store command) ===",
        "dataset: $StoreDataset",
        "executable: $exe",
        "runs_per_depth: $StoreCount"
    )

    $results = @()
    foreach ($depth in $IOUringQueueDepths) {
        if ($depth -le 0) {
            throw "All IOUringQueueDepths must be > 0. Invalid value: $depth"
        }

        $times = @()
        for ($i = 1; $i -le $StoreCount; $i++) {
            $dbPath = Join-Path $StoreDbRoot ("io_uring_${depth}_run_${i}")
            Remove-Item -Recurse -Force $dbPath -ErrorAction SilentlyContinue

            $cmdArgs = @("store", "--io-engine", "io-uring", "--io-uring-queue-depth", "$depth", "-d", $dbPath, $StoreDataset)
            $allArgs = @($exePrefix + $cmdArgs)
            $duration = Get-StoreDurationSeconds -Exe $exe -ExecutionParams $allArgs
            $times += $duration

            Write-Host ("io_uring depth {0} run {1}/{2}: {3:N4}s" -f $depth, $i, $StoreCount, $duration) -ForegroundColor DarkCyan
        }

        $median = Get-Median -Values $times
        $results += [pscustomobject]@{
            Depth  = $depth
            Median = $median
        }
        $lines += ("depth={0} median_seconds={1:N4}" -f $depth, $median)
    }

    $best = $results | Sort-Object Median | Select-Object -First 1
    if ($null -ne $best) {
        $lines += ("recommended_depth={0} median_seconds={1:N4}" -f $best.Depth, $best.Median)
    }

    $lines | Add-Content -Path $summaryFile
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
Write-IOUringQueueDepthSummary

Write-Host "Benchmark run complete. Outputs in $OutputDir" -ForegroundColor Green
Write-Host "Summary: $summaryFile" -ForegroundColor Green
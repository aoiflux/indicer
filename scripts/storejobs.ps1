<#
.SYNOPSIS
Times a folder store one at a time against --store-jobs 0, and splits the
time between the heavy evidence and the rest.

.DESCRIPTION
Each run stores -Corpus into a new container-mode database under -Work, with
--debug, so the log has a StoreFolder ITEM record per evidence. The runs go in
-Order (1 = one at a time, 0 = --store-jobs 0). After each, the database is
removed and the log kept. Before each run, the first too, the disk idles for
-Settle seconds, so no run starts on the heels of another's writes and none
after a longer rest than the others. Every second, typeperf samples the disk
queue and the CPU of dues and of the Defender engine (MsMpEng), in cores: a
heavy evidence that keeps few cores busy waits on the disk. The disks'
temperature is read before each run where Windows allows it (it needs an
administrator).

H is the heavy evidence's time at --store-jobs 0 over their time one at a
time, from the mean of each setting's runs. docs/BENCHMARKING.md, "Heavy
evidence beside small ones", says how to read it.

.EXAMPLE
pwsh -NoProfile -File scripts/storejobs.ps1 -Corpus O:\tmp\sj\spread -Work O:\tmp\sj\work-spread -Dues .\dues.exe
#>
param(
    [Parameter(Mandatory = $true)][string]$Corpus,
    [Parameter(Mandatory = $true)][string]$Work,
    [string]$Dues = ".\dues.exe",
    [string]$Order = "1,0,0,1,1,0",
    # Seconds the disk idles before each run.
    [int]$Settle = 120,
    # Evidence of this many bytes or more count as heavy: cli heavyItem.
    [long]$HeavyBytes = 256MB
)
$ErrorActionPreference = "Stop"

$Dues = (Resolve-Path $Dues).Path
$Corpus = (Resolve-Path $Corpus).Path
if (Test-Path $Work) { throw "$Work exists; give a new directory" }
New-Item -ItemType Directory -Path $Work | Out-Null
$Work = (Resolve-Path $Work).Path

# Keep the machine and its disks awake: idle standby has dropped a USB disk
# under a run before. The u suffix keeps 0x80000000 unsigned.
Add-Type -Namespace Bench -Name Power -MemberDefinition @'
[System.Runtime.InteropServices.DllImport("kernel32.dll")]
public static extern uint SetThreadExecutionState(uint flags);
'@
$continuous, $system, $display = 0x80000000u, 0x1u, 0x2u
[void][Bench.Power]::SetThreadExecutionState($continuous -bor $system -bor $display)

function Get-DiskTemperature {
    try {
        $t = Get-PhysicalDisk | Get-StorageReliabilityCounter -ErrorAction Stop |
            Where-Object { $_.Temperature } | ForEach-Object { "$($_.DeviceId):$($_.Temperature)C" }
        if ($t) { return ($t -join " ") }
    } catch {}
    return "n/a"
}

# The counters sampled, and the column of the typeperf CSV each one heads.
$counters = [ordered]@{
    queue        = '\PhysicalDisk(_Total)\Avg. Disk Queue Length'
    dues_cpu     = '\Process(dues)\% Processor Time'
    defender_cpu = '\Process(MsMpEng)\% Processor Time'
}

# Get-PerfStats is the mean and peak disk queue of a run, and the mean CPU of
# dues and of Defender in cores. A process's samples count only while it ran.
function Get-PerfStats {
    param([string]$Csv)
    $stats = [ordered]@{ queue_mean = $null; queue_max = $null; dues_cores = $null; defender_cores = $null }
    $rows = @(if (Test-Path $Csv) { Import-Csv $Csv })
    if ($rows.Count -eq 0) { return $stats }
    $names = @($rows[0].PSObject.Properties.Name)
    $column = {
        param([string]$Counter)
        $name = $names | Where-Object { $_.EndsWith($Counter) } | Select-Object -First 1
        if (-not $name) { return @() }
        @($rows | ForEach-Object {
                $d = 0.0
                if ([double]::TryParse($_.$name, [Globalization.NumberStyles]::Float, [cultureinfo]::InvariantCulture, [ref]$d)) { $d }
            })
    }
    $queue = @(& $column $counters.queue)
    if ($queue.Count) {
        $m = $queue | Measure-Object -Average -Maximum
        $stats.queue_mean, $stats.queue_max = [math]::Round($m.Average, 2), [math]::Round($m.Maximum, 2)
    }
    foreach ($k in "dues", "defender") {
        $v = @(& $column $counters["${k}_cpu"])
        if ($v.Count) { $stats["${k}_cores"] = [math]::Round(($v | Measure-Object -Average).Average / 100, 2) }
    }
    return $stats
}

function Read-Records {
    param([string]$LogDir)
    Get-ChildItem -Path $LogDir -Filter "dues-*.ndjson" | ForEach-Object {
        Get-Content $_.FullName | ForEach-Object { $_ | ConvertFrom-Json }
    }
}

$runs = @()
$n = 0
foreach ($jobs in $Order.Split(",")) {
    $n++
    $db = Join-Path $Work "db-$n"
    $logs = Join-Path $Work "logs-$n"
    $perfCsv = Join-Path $Work "perf-$n.csv"
    Start-Sleep -Seconds $Settle
    $temp = Get-DiskTemperature
    $perf = Start-Process -FilePath typeperf -PassThru -WindowStyle Hidden -ArgumentList (
        @($counters.Values | ForEach-Object { "`"$_`"" }) + @("-si", "1", "-o", "`"$perfCsv`"", "-f", "CSV", "-y"))
    # typeperf takes a second or two to start; the store waits for its first
    # sample, the header line and one more.
    $deadline = (Get-Date).AddSeconds(10)
    while ((Get-Date) -lt $deadline -and -not ((Test-Path $perfCsv) -and @(Get-Content $perfCsv).Count -ge 2)) {
        Start-Sleep -Milliseconds 100
    }
    $clock = [System.Diagnostics.Stopwatch]::StartNew()
    & $Dues --no-config --log-dir $logs --debug store $Corpus --dbpath $db --container --store-jobs $jobs *> (Join-Path $Work "store-$n.txt")
    $code = $LASTEXITCODE
    $clock.Stop()
    Stop-Process -Id $perf.Id -ErrorAction SilentlyContinue
    if ($code -ne 0) { throw "run $n (--store-jobs $jobs) exited $code; see $(Join-Path $Work "store-$n.txt")" }
    $dbBytes = (Get-ChildItem -Recurse -File $db | Measure-Object -Sum Length).Sum
    Remove-Item -Recurse -Force $db

    $records = @(Read-Records $logs)
    $items = @($records | Where-Object { $_.msg -eq "StoreFolder ITEM" })
    $complete = $records | Where-Object { $_.msg -eq "StoreFolder COMPLETE" } | Select-Object -First 1
    $heavy = @($items | Where-Object { [long]$_.size -ge $HeavyBytes })
    $heavyMs = ($heavy | Measure-Object -Sum duration_ms).Sum
    $sample = Get-PerfStats $perfCsv
    $run = [ordered]@{
        run = $n; jobs = [int]$jobs; wall_s = [math]::Round($clock.Elapsed.TotalSeconds, 1)
        store_ms = $complete.duration_ms; items = $items.Count; heavy = $heavy.Count
        heavy_ms = $heavyMs; rest_ms = $complete.duration_ms - $heavyMs
        db_mib = [math]::Round($dbBytes / 1MB); queue_mean = $sample.queue_mean; queue_max = $sample.queue_max
        dues_cores = $sample.dues_cores; defender_cores = $sample.defender_cores; temperature = $temp
        heavy_items = @($heavy | ForEach-Object {
                [ordered]@{
                    item = $_.item; size = $_.size; duration_ms = $_.duration_ms
                    l0_before = $_.lsm_before_l0_tables; l0_after = $_.lsm_after_l0_tables
                    compacting_before = $_.lsm_before_compacting_tables
                    compacted_mib = [math]::Round(($_.lsm_after_compacted_bytes - $_.lsm_before_compacted_bytes) / 1MB)
                    gc_cycles = $_.gc_cycles; gc_cpu_ms = $_.gc_cpu_ms; gc_pause_us = $_.gc_pause_us
                    heap_live_mib = [math]::Round($_.heap_live_bytes / 1MB); credit_locked = $_.credit_locked
                }
            })
    }
    $runs += $run
    "{0,3}  jobs {1}  wall {2,7:N1} s  heavy {3,8:N0} ms  rest {4,8:N0} ms  queue {5}/{6}  cores dues {7} defender {8}  {9}" -f $n, $jobs, $run.wall_s, $heavyMs, $run.rest_ms, $run.queue_mean, $run.queue_max, $run.dues_cores, $run.defender_cores, $temp
}
[void][Bench.Power]::SetThreadExecutionState($continuous)

function Get-Mean { param($Values) ($Values | Measure-Object -Average).Average }
$one = @($runs | Where-Object { $_.jobs -eq 1 })
$auto = @($runs | Where-Object { $_.jobs -eq 0 })
$summary = [ordered]@{
    corpus = $Corpus; dues = $Dues; order = $Order; heavy_bytes = $HeavyBytes; settle_s = $Settle
    mean_heavy_ms_one = Get-Mean $one.heavy_ms; mean_heavy_ms_auto = Get-Mean $auto.heavy_ms
    mean_rest_ms_one = Get-Mean $one.rest_ms; mean_rest_ms_auto = Get-Mean $auto.rest_ms
    mean_store_ms_one = Get-Mean $one.store_ms; mean_store_ms_auto = Get-Mean $auto.store_ms
}
if ($one.Count -and $auto.Count -and $summary.mean_heavy_ms_one -gt 0) {
    $summary.H = [math]::Round($summary.mean_heavy_ms_auto / $summary.mean_heavy_ms_one, 3)
}
if ($summary.mean_rest_ms_auto -gt 0) {
    $summary.rest_gain = [math]::Round($summary.mean_rest_ms_one / $summary.mean_rest_ms_auto, 2)
}
[ordered]@{ summary = $summary; runs = $runs } | ConvertTo-Json -Depth 6 | Set-Content (Join-Path $Work "storejobs.json")
""
"H = {0}  (heavy evidence at --store-jobs 0 over one at a time)" -f $summary.H
"rest gain = {0}x  wall one {1:N0} ms, auto {2:N0} ms" -f $summary.rest_gain, $summary.mean_store_ms_one, $summary.mean_store_ms_auto
"results: $(Join-Path $Work "storejobs.json")"

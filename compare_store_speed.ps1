param(
    [Parameter(Mandatory = $true)]
    [string]$Dataset,
    [string]$CurrentExe = ".\\dues.exe",
    [string]$MainExe = ".\\dues_main.exe",
    [int]$Count = 5,
    [string]$CurrentCompression = "best",
    [switch]$CurrentSync,
    [string]$DbRoot = ".\\bench_compare"
)

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
    $seconds = ConvertFrom-StoreOutput -Output $output
    if ($null -eq $seconds) {
        throw "Could not parse store duration from output for $Exe. Output:`n$output"
    }
    return $seconds
}

# ConvertFrom-StoreOutput returns the store duration in seconds from dues
# store output: "Stored as evidence ... in <Go duration>" (for example 1m2.5s
# or 850ms), or "Stored in: <seconds>s" from older releases. $null if neither.
function ConvertFrom-StoreOutput {
    param([string]$Output)

    $m = [regex]::Match($Output, 'Stored as evidence .* in ((?:[0-9.]+(?:h|ms|us|\u00B5s|ns|m|s))+)')
    if ($m.Success) {
        $seconds = 0.0
        foreach ($part in [regex]::Matches($m.Groups[1].Value, '([0-9.]+)(h|ms|us|\u00B5s|ns|m|s)')) {
            $v = [double]$part.Groups[1].Value
            switch ($part.Groups[2].Value) {
                'h' { $seconds += $v * 3600 }
                'm' { $seconds += $v * 60 }
                's' { $seconds += $v }
                'ms' { $seconds += $v / 1e3 }
                default { if ($part.Groups[2].Value -eq 'ns') { $seconds += $v / 1e9 } else { $seconds += $v / 1e6 } }
            }
        }
        return $seconds
    }

    $m = [regex]::Match($Output, 'Stored in:\s*([0-9.]+)s')
    if ($m.Success) {
        return [double]$m.Groups[1].Value
    }
    return $null
}

if (-not (Test-Path $Dataset)) {
    throw "Dataset path not found: $Dataset"
}
if (-not (Test-Path $CurrentExe)) {
    throw "Current executable not found: $CurrentExe"
}
if (-not (Test-Path $MainExe)) {
    throw "Main executable not found: $MainExe"
}
if ($Count -lt 1) {
    throw "Count must be >= 1"
}

New-Item -ItemType Directory -Force -Path $DbRoot | Out-Null

$currentTimes = @()
$mainTimes = @()

for ($i = 1; $i -le $Count; $i++) {
    $currentDb = Join-Path $DbRoot ("current_" + $i)
    $mainDb = Join-Path $DbRoot ("main_" + $i)

    Remove-Item -Recurse -Force $currentDb -ErrorAction SilentlyContinue
    Remove-Item -Recurse -Force $mainDb -ErrorAction SilentlyContinue

    $currentArgs = @("--compress-level", $CurrentCompression)
    if ($CurrentSync) {
        $currentArgs += "--sync"
    }
    $currentArgs += @("store", "-d", $currentDb, $Dataset)

    $mainArgs = @("store", "-d", $mainDb, $Dataset)

    $tCurrent = Get-StoreDurationSeconds -Exe $CurrentExe -ExecutionParams $currentArgs
    $tMain = Get-StoreDurationSeconds -Exe $MainExe -ExecutionParams $mainArgs

    $currentTimes += $tCurrent
    $mainTimes += $tMain

    Write-Host ("Run {0}/{1}: current={2:N4}s main={3:N4}s" -f $i, $Count, $tCurrent, $tMain)
}

$medCurrent = Get-Median -Values $currentTimes
$medMain = Get-Median -Values $mainTimes
$speedRatio = $medMain / $medCurrent

Write-Host ""
Write-Host "=== Median Results ==="
Write-Host ("current median: {0:N4}s" -f $medCurrent)
Write-Host ("main median:    {0:N4}s" -f $medMain)
if ($speedRatio -ge 1) {
    Write-Host ("current is {0:N2}x faster than main" -f $speedRatio)
}
else {
    Write-Host ("main is {0:N2}x faster than current" -f (1 / $speedRatio))
}

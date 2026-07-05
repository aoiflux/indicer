param(
    [ValidateSet("compression")]
    [string]$Product = "compression",
    [ValidateSet("cli", "ffi", "all")]
    [string]$Mode = "all",
    [ValidateSet("auto", "c-shared", "c-archive")]
    [string]$FfiBuildMode = "auto",
    [string]$OutputDir = "",
    [string[]]$Targets = @("windows/amd64")
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

$repoRoot = Split-Path -Parent $PSCommandPath
Set-Location $repoRoot

switch ($Product) {
    "compression" {
        $scriptPath = Join-Path $repoRoot "build-compression.ps1"
        if (-not (Test-Path $scriptPath)) {
            throw "Missing script: $scriptPath"
        }

        $params = @{
            Mode         = $Mode
            Targets      = $Targets
            FfiBuildMode = $FfiBuildMode
        }

        if (-not [string]::IsNullOrWhiteSpace($OutputDir)) {
            $params.OutputDir = $OutputDir
        }

        & $scriptPath @params
        if ($LASTEXITCODE -ne 0) {
            throw "Product build failed for $Product"
        }
    }
    default {
        throw "Unsupported product: $Product"
    }
}

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root
if (-not $env:MY_AI_SUM_DATA_DIR) {
  $env:MY_AI_SUM_DATA_DIR = Join-Path $root "data"
}
go run ./cmd/my-ai-sum

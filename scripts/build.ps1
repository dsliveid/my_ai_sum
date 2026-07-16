param(
  [string]$Output = "build\my-ai-sum.exe"
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $Output) | Out-Null
go mod tidy
go build -ldflags="-s -w" -o $Output ./cmd/my-ai-sum
Write-Host "Built $Output"

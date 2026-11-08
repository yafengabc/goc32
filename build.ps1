# Build goc32 only.
$ErrorActionPreference = "Stop"
Push-Location $PSScriptRoot
try {
    New-Item -ItemType Directory -Force bin | Out-Null
    go build -trimpath -ldflags="-s -w" -o bin\goc32.exe .
    Write-Output "done: bin/goc32.exe"
} finally {
    Pop-Location
}

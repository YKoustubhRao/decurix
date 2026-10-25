# Builds Brainiac and brings it up on this Windows machine: it measures the
# machine, chooses the best open models that fit for each kind of work, shows
# the choice and why, installs it after a yes, and serves one OpenAI-compatible
# endpoint. Twin of scripts/build.sh (keep both in step).
#
#   scripts\build.ps1              build, then `brainiac up` (plan, ask, install, serve)
#   scripts\build.ps1 --yes        the same without the question
#   scripts\build.ps1 plan         build, then only show the plan
#   scripts\build.ps1 census       build, then only measure
#
# On a Windows machine whose models run in WSL (Decurix's own setup, where
# Ollama lives in the WSL VM), run the Linux script there instead so Brainiac
# and its models share one memory pool:
#   wsl -d Ubuntu -- ./brainiac/scripts/build.sh
#
# Models and state live in BRAINIAC_HOME (default %USERPROFILE%\.brainiac).

$ErrorActionPreference = 'Stop'

Set-Location (Split-Path -Parent $PSScriptRoot)

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    Write-Error "brainiac: building needs Go (https://go.dev/dl)"
    exit 1
}

New-Item -ItemType Directory -Force -Path bin | Out-Null
$version = (git describe --always --dirty 2>$null)
if (-not $version) { $version = 'dev' }
Write-Host "Building brainiac ($version)..."
# GOTOOLCHAIN=auto fetches the Go that go.mod names when the installed one is older.
$env:GOTOOLCHAIN = 'auto'
& go build -trimpath -ldflags "-X main.version=$version" -o bin\brainiac.exe .\cmd\brainiac
if ($LASTEXITCODE -ne 0) {
    exit $LASTEXITCODE
}

$rest = @($args)
$cmd = 'up'
if ($rest.Count -gt 0 -and @('census', 'plan', 'pull', 'serve', 'up', 'status', 'version') -contains $rest[0]) {
    $cmd = $rest[0]
    $rest = @($rest | Select-Object -Skip 1)
}

& .\bin\brainiac.exe $cmd @rest
exit $LASTEXITCODE

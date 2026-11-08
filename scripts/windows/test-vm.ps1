<#
.SYNOPSIS
    Spins up ONE throwaway desktop container standalone, for quickly testing
    a desktop image or the agent harness without building/running the whole
    decurix control plane.
.DESCRIPTION
    Pairs with scripts/testing/dummy_control_plane.py, a stdlib-only stub
    that accepts the desktop's query/completion callbacks and prints them
    live, so you can watch the agent work.
.PARAMETER Profile
    basic, developer, or ai. Default: basic.
.PARAMETER Harness
    thebot, the only harness since 2026-11-07. Default: thebot.
.PARAMETER Name
    Desktop id/name. Default: test-vm.
.PARAMETER Task
    Optional task text to send once the agent is up.
.PARAMETER Down
    Remove this script's own test containers and exit - never touches
    desktop_* containers created any other way.
#>

param(
    [ValidateSet("basic", "developer", "ai")]
    [string]$DesktopProfile = "basic",

    [ValidateSet("thebot")]
    [string]$Harness = "thebot",

    [string]$Name = "test-vm",

    [string]$Task = "",

    [switch]$Down
)

$ErrorActionPreference = "Stop"

# Every script runs from the repo root, two levels up from scripts/<group>/.
Set-Location (Split-Path -Parent (Split-Path -Parent $PSScriptRoot))

$DesktopContainer = "desktop_$Name"
$DummyCP = "decurix-dummy-cp"

# Same default as internal/desktop/docker.go's buildDesktopEnv - the host
# running the control plane, reached over the ExtraHosts entry every desktop
# gets. Override with your own OLLAMA_URL/OLLAMA_MODEL env vars if needed.
$OllamaUrl = if ($env:OLLAMA_URL) { $env:OLLAMA_URL } else { "http://host.docker.internal:11434" }
$OllamaModel = if ($env:OLLAMA_MODEL) { $env:OLLAMA_MODEL } else { "qwen3:latest" }
# Optional second model, used only for coding builds - TheBot routes
# "coding" separately. Unset means the one model above does everything.
$CodingModel = if ($env:CODING_MODEL) { $env:CODING_MODEL } else { "" }

if ($Down) {
    Write-Host "=== Removing test desktop $DesktopContainer ==="
    docker rm -f $DesktopContainer 2>$null | Out-Null

    # The dummy control plane is only ever used by test-vm's own desktop
    # containers (never by real decurix-managed ones), so it's always safe
    # to remove here - it's a cheap stub, re-created instantly next run.
    Write-Host "=== Removing dummy control plane ==="
    docker rm -f $DummyCP 2>$null | Out-Null

    exit 0
}

Write-Host "========================================"
Write-Host " Decurix standalone VM test"
Write-Host "========================================"
Write-Host "profile=$DesktopProfile harness=$Harness name=$Name"
Write-Host ""

Write-Host "=== Ensuring decurix-network ==="
docker network inspect decurix-network *>$null
if ($LASTEXITCODE -ne 0) {
    docker network create decurix-network | Out-Null
}

Write-Host ""
Write-Host "=== Ensuring image decurix-desktop:$DesktopProfile ==="
docker image inspect "decurix-desktop:$DesktopProfile" *>$null
if ($LASTEXITCODE -ne 0) {
    Write-Host "Image not found locally - building it (this can take a while)..."
    docker build -f "docker/desktop.$DesktopProfile.Dockerfile" -t "decurix-desktop:$DesktopProfile" .
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
}

Write-Host ""
Write-Host "=== Starting dummy control plane ($DummyCP) ==="
$running = docker ps --filter "name=^$DummyCP`$" --format '{{.Names}}'
if ($running -ne $DummyCP) {
    docker rm -f $DummyCP 2>$null | Out-Null
    docker run -d `
        --name $DummyCP `
        --network decurix-network `
        --network-alias $DummyCP `
        -v "${PWD}/scripts/testing:/srv:ro" `
        -w /srv `
        python:3-alpine `
        python dummy_control_plane.py | Out-Null
    Write-Host "Started."
} else {
    Write-Host "Already running."
}

Write-Host ""
Write-Host "=== Starting test desktop ($DesktopContainer) ==="
docker rm -f $DesktopContainer 2>$null | Out-Null

# A fresh agent token and screen password for each test desktop, never a
# fixed one: the agent refuses /request without its token (DECURIX_AGENT_TOKEN,
# as the control plane gives every desktop), and the ports are published on
# this computer's 127.0.0.1 only, never on the network.
function New-RandomHex([int]$Bytes) {
    $buffer = New-Object byte[] $Bytes
    [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($buffer)
    return -join ($buffer | ForEach-Object { $_.ToString('x2') })
}
$AgentToken = New-RandomHex 24
$VncPassword = New-RandomHex 9

docker run -d `
    --name $DesktopContainer `
    --network decurix-network `
    --shm-size=512m `
    -p 127.0.0.1:16901:6901 `
    -p 127.0.0.1:14900:4900 `
    -p 127.0.0.1:18081:8081 `
    --add-host host.docker.internal:host-gateway `
    -e VNC_PW=$VncPassword `
    -e DECURIX_AGENT_TOKEN=$AgentToken `
    -e VNC_RESOLUTION=1280x720 `
    -e DECURIX_DESKTOP_NOVNC=1 `
    -e DESKTOP_ID=$Name `
    -e DESKTOP_NAME=$Name `
    -e CONTROL_PLANE_URL="http://${DummyCP}:8080" `
    -e OLLAMA_URL=$OllamaUrl `
    -e OLLAMA_MODEL=$OllamaModel `
    -e AGENT_HARNESS=$Harness `
    -e CODING_MODEL=$CodingModel `
    "decurix-desktop:$DesktopProfile" | Out-Null

Write-Host ""
Write-Host "=== Waiting for the agent to come up (vm_server.py:8081) ==="
# Checked from inside the container (vm_server.py logs this line itself
# once its HTTPServer starts) rather than by probing the published host
# port - Docker Desktop's host<->container port forwarding has real,
# observed lag/flakiness for rapid successive probes right after a
# container starts, which made a host-side probe loop unreliable here even
# though the container was already serving requests.
$ready = $false
for ($i = 1; $i -le 60; $i++) {
    docker exec $DesktopContainer grep -q "VM agent listening" /tmp/decurix-startup.log 2>$null
    if ($LASTEXITCODE -eq 0) {
        $ready = $true
        break
    }
    Write-Host "Waiting... ($i/60)"
    Start-Sleep -Seconds 2
}
if (-not $ready) {
    Write-Host "WARNING: agent did not come up within 2 minutes - check 'docker logs $DesktopContainer'."
}

if ($Task) {
    Write-Host ""
    Write-Host "=== Sending task ==="
    try {
        $body = @{ content = $Task } | ConvertTo-Json -Compress
        Invoke-RestMethod -Uri "http://127.0.0.1:18081/request" -Method Post -ContentType "application/json" -Headers @{ Authorization = "Bearer $AgentToken" } -Body $body | Out-Null
        Write-Host "Sent."
    } catch {
        Write-Host "WARNING: could not deliver the task yet - the agent may still be starting. Retry with the same -Task once it's up."
    }
}

Write-Host ""
Write-Host "========================================"
Write-Host " Test desktop is up"
Write-Host "========================================"
Write-Host "VNC:                http://localhost:16901/vnc.html  (password: $VncPassword)"
if ($Harness -eq "thebot") {
Write-Host "Agent chat UI:      http://localhost:14900/"
}
Write-Host "Send a task:        Invoke-RestMethod http://localhost:18081/request -Method Post -ContentType application/json -Headers @{ Authorization = 'Bearer ' + (docker exec $DesktopContainer printenv DECURIX_AGENT_TOKEN) } -Body '{\"content\":\"...\"}'"
Write-Host "Watch callbacks:    docker logs -f $DummyCP"
Write-Host "Watch agent logs:   docker exec $DesktopContainer tail -f /tmp/decurix-startup.log"
Write-Host "Tear down:          .\scripts\windows\test-vm.ps1 -Name $Name -Down"

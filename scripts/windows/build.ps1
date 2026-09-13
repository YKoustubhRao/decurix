<#
.SYNOPSIS
    Builds Flowera desktop images, makes sure the desktops can reach Ollama,
    starts Flowera, and opens a Cloudflare Tunnel to it.
.DESCRIPTION
    The Windows twin of scripts/linux/build.sh, step for step:

      1. build the three flowera-desktop images (build-images.ps1)
      2. find an Ollama the desktop containers can actually reach, pull the
         chat and embedding models onto it, and record OLLAMA_URL and
         OLLAMA_MODEL in .env
      3. stop Flowera, remove old desktop containers and volumes
      4. start the control plane with docker compose and wait for :8080
      5. open a Cloudflare quick tunnel and report its URL to the dashboard

    Needs Docker Desktop running. Does not need Administrator.

    Ctrl+C while it runs (including while the tunnel is up) tears the whole
    stack down, so nothing is left running detached.
#>

#region Functions
function Test-CommandAvailable {
    param([string]$CommandName)
    [bool](Get-Command $CommandName -ErrorAction SilentlyContinue -CommandType Application)
}

# Exit-Build is how every deliberate exit leaves the script. It marks the exit
# as intentional so the `finally` below leaves the stack alone: a failed build
# or a missing cloudflared must not stop a Flowera that is already running.
# Ctrl+C never passes through here, and that is what triggers the teardown.
function Exit-Build {
    param([int]$Code)
    $script:IntentionalExit = $true
    exit $Code
}

function Wait-ForDashboard {
    param([int]$MaxAttempts = 60, [int]$DelaySeconds = 2)

    for ($attempt = 1; $attempt -le $MaxAttempts; $attempt++) {
        try {
            # -UseBasicParsing: without it Windows PowerShell 5.1 hands the
            # response to the Internet Explorer engine, which fails outright
            # on a machine where IE was never set up.
            $response = Invoke-WebRequest -Uri "http://127.0.0.1:8080/" `
                -UseBasicParsing -TimeoutSec 5 -ErrorAction Stop
            if ($response.StatusCode -eq 200) {
                Write-Host "Flowera dashboard is running."
                return $true
            }
        } catch {
            # Not up yet.
        }

        Write-Host "Waiting for Flowera... ($attempt/$MaxAttempts)"
        Start-Sleep -Seconds $DelaySeconds
    }

    return $false
}

# Ctrl+C lands here while the script is blocked on the tunnel command, which
# runs forever until interrupted. Without this, "docker compose up -d" and
# the desktop containers it depends on would keep running detached with no
# tunnel pointed at them - a silent, orphaned Flowera. Tear the whole stack
# down instead. Runs from a `finally` because PowerShell has no INT-only
# trap; it also covers the tunnel exiting on its own.
function Invoke-Cleanup {
    Write-Host ""
    Write-Host "=== Stopping Flowera and its containers ==="

    docker compose down

    $containers = docker ps -a --filter "name=desktop_" --format "{{.ID}}"
    if ($containers) {
        docker rm -f $containers
    }

    docker rm -f flowera-ollama 2>$null | Out-Null
}

# .env is read by the control plane (docker/Dockerfile copies it into the
# image) and by docker compose, both on Linux. Written as UTF-8 *without* a
# BOM and with LF endings: Windows PowerShell 5.1's own Set-Content/Out-File
# would add a BOM (or write UTF-16), and a BOM glued to the first key turns
# DEFAULT_PASSWORD into a variable nobody reads.
function Read-EnvVar {
    param([string]$Name)

    if (-not (Test-Path $script:EnvFile)) { return "" }

    $line = Get-Content $script:EnvFile |
        Where-Object { $_ -match "^$([regex]::Escape($Name))=" } |
        Select-Object -Last 1

    if ($line) { return $line.Substring($Name.Length + 1) }
    return ""
}

function Set-EnvVar {
    param([string]$Name, [string]$Value)

    $lines = @()
    if (Test-Path $script:EnvFile) {
        $lines = @(Get-Content $script:EnvFile)
    }

    $pattern = "^$([regex]::Escape($Name))="
    $found = $false

    for ($i = 0; $i -lt $lines.Count; $i++) {
        if ($lines[$i] -match $pattern) {
            $lines[$i] = "$Name=$Value"
            $found = $true
        }
    }

    if (-not $found) {
        $lines += "$Name=$Value"
    }

    $utf8NoBom = New-Object System.Text.UTF8Encoding $false
    [System.IO.File]::WriteAllText(
        $script:EnvFile,
        (($lines -join "`n") + "`n"),
        $utf8NoBom
    )
}

# Invoke-InDesktopImage runs a Python snippet inside flowera-desktop:basic, on
# flowera-network, with the same host.docker.internal mapping every desktop
# gets. That is the only place "can a desktop reach Ollama?" is answered
# honestly: this shell can always reach its own 127.0.0.1, which is exactly
# the configuration that leaves every desktop unable to reach anything.
#
# The code goes in on stdin and its inputs as arguments, never spliced into
# a -c string: Windows PowerShell 5.1 mangles double quotes in arguments to
# native programs, and a multi-line -c argument is at its mercy.
function Invoke-InDesktopImage {
    param([string]$Code, [string[]]$Arguments)

    $Code | docker run --rm -i `
        --network flowera-network `
        --add-host host.docker.internal:host-gateway `
        --entrypoint python3 `
        flowera-desktop:basic `
        - @Arguments
}

function Test-OllamaReachable {
    param([string]$Url)

    $code = @'
import sys, urllib.request
try:
    urllib.request.urlopen(sys.argv[1].rstrip('/') + '/api/tags', timeout=5)
except Exception:
    sys.exit(1)
'@

    Invoke-InDesktopImage -Code $code -Arguments @($Url) *>$null
    return ($LASTEXITCODE -eq 0)
}

function Get-OllamaModels {
    param([string]$Url)

    $code = @'
import json, sys, urllib.request
try:
    with urllib.request.urlopen(sys.argv[1].rstrip('/') + '/api/tags', timeout=5) as r:
        for m in json.load(r).get('models', []):
            print(m.get('name', ''))
except Exception:
    pass
'@

    @(Invoke-InDesktopImage -Code $code -Arguments @($Url) 2>$null |
        Where-Object { $_ })
}

function Test-ModelPresent {
    param([string[]]$Available, [string]$Model)

    $pattern = "^$([regex]::Escape($Model))(:latest)?$"
    [bool]($Available | Where-Object { $_ -match $pattern })
}

function Invoke-OllamaPull {
    param([string]$Url, [string]$Model)

    $code = @'
import json, sys, urllib.request
req = urllib.request.Request(
    sys.argv[1].rstrip('/') + '/api/pull',
    data=json.dumps({'model': sys.argv[2], 'stream': False}).encode(),
    headers={'Content-Type': 'application/json'},
)
try:
    # No timeout: pulling a model is a download, and how long it takes is
    # a property of the network, not of whether it is working.
    urllib.request.urlopen(req)
except Exception as exc:
    print(exc, file=sys.stderr)
    sys.exit(1)
'@

    # Out-Host, so nothing the pull prints becomes part of the return value.
    Invoke-InDesktopImage -Code $code -Arguments @($Url, $Model) | Out-Host
    return ($LASTEXITCODE -eq 0)
}

# The Windows counterpart of build.sh's systemd step. The Ollama app for
# Windows listens on 127.0.0.1 only unless OLLAMA_HOST says otherwise, and it
# reads that from the user's environment when it starts - so set it there
# and restart the app. Docker Desktop normally forwards host.docker.internal
# to the host's loopback, which is why this rarely fires; when it does,
# nothing short of listening on more than loopback makes Ollama reachable.
function Enable-OllamaListenAll {
    $app = Join-Path $env:LOCALAPPDATA "Programs\Ollama\ollama app.exe"

    if (-not (Test-Path $app)) {
        Write-Host "The Ollama app is not in its default location, so it cannot be"
        Write-Host "restarted from here. Set OLLAMA_HOST=0.0.0.0 in your user"
        Write-Host "environment variables, restart Ollama, and re-run this script."
        return $false
    }

    Write-Host "Setting OLLAMA_HOST=0.0.0.0 for your user and restarting Ollama."
    Write-Host "(Windows Firewall may ask whether Ollama may accept connections.)"
    Write-Host ""

    [Environment]::SetEnvironmentVariable("OLLAMA_HOST", "0.0.0.0", "User")
    $env:OLLAMA_HOST = "0.0.0.0"

    Get-Process -Name "ollama app", "ollama" -ErrorAction SilentlyContinue |
        Stop-Process -Force -ErrorAction SilentlyContinue
    Start-Sleep -Seconds 2
    Start-Process -FilePath $app

    for ($i = 1; $i -le 15; $i++) {
        if (Test-OllamaReachable $script:DefaultOllamaUrl) {
            return $true
        }
        Start-Sleep -Seconds 2
    }

    return $false
}
#endregion

#region Main Script
# Every script runs from the repo root, two levels up from scripts/<group>/.
Set-Location (Split-Path -Parent (Split-Path -Parent $PSScriptRoot))

$script:IntentionalExit = $false
$script:EnvFile = Join-Path (Get-Location).Path ".env"
$script:DefaultOllamaUrl = "http://host.docker.internal:11434"
$BundledOllamaUrl = "http://flowera-ollama:11434"

# The chat model is not the only model. TheBot's harness embeds every fact it
# learns, every document it indexes and every failure it records, and Ollama
# refuses to embed with a chat model. See internal/ollama.EmbedModel.
$EmbedModel = "nomic-embed-text"

try {
    # The single most common failure on Windows, and docker's own message
    # for it (a missing named pipe) does not say "start Docker Desktop".
    if (-not (Test-CommandAvailable "docker")) {
        Write-Host "ERROR: docker is not installed. Run .\scripts\windows\setup.ps1"
        Write-Host "as Administrator, or install Docker Desktop yourself."
        Exit-Build 1
    }

    docker info *>$null
    if ($LASTEXITCODE -ne 0) {
        Write-Host "ERROR: Docker is installed but not running."
        Write-Host "Start Docker Desktop, wait until it says it is running, and re-run."
        Exit-Build 1
    }

    & "$PSScriptRoot/build-images.ps1"
    if ($LASTEXITCODE -ne 0) { Exit-Build $LASTEXITCODE }

    Write-Host ""
    Write-Host "=== Ensuring the desktops can reach Ollama ==="

    # Everything below writes .env, which docker/Dockerfile copies into the
    # control plane image - hence running before "docker compose up --build".
    # A fresh clone has no .env at all, and without one that build fails.
    if (-not (Test-Path $script:EnvFile)) {
        if (Test-Path ".env.example") {
            Write-Host "No .env yet - creating it from .env.example."
            Copy-Item ".env.example" $script:EnvFile
        } else {
            New-Item -ItemType File -Path $script:EnvFile | Out-Null
        }
    }

    docker network inspect flowera-network *>$null
    if ($LASTEXITCODE -ne 0) {
        docker network create flowera-network | Out-Null
    }

    $ollamaUrl = ""

    # Whatever is already in .env wins, as long as it actually works.
    $configuredUrl = Read-EnvVar "OLLAMA_URL"

    if ($configuredUrl) {
        if (Test-OllamaReachable $configuredUrl) {
            Write-Host "Using OLLAMA_URL from .env: $configuredUrl"
            $ollamaUrl = $configuredUrl
        } else {
            Write-Host "OLLAMA_URL in .env is not reachable from a container:"
            Write-Host "  $configuredUrl"
            Write-Host "Looking for one that is."
        }
    }

    # The host running the control plane, over the same host-gateway mapping
    # every desktop already gets for CONTROL_PLANE_URL.
    if (-not $ollamaUrl -and (Test-OllamaReachable $script:DefaultOllamaUrl)) {
        Write-Host "Host Ollama is reachable from containers."
        $ollamaUrl = $script:DefaultOllamaUrl
    }

    # Ollama is running right here, but no container can reach it.
    if (-not $ollamaUrl) {
        $hostOllama = $false
        try {
            Invoke-WebRequest -Uri "http://127.0.0.1:11434/api/tags" `
                -UseBasicParsing -TimeoutSec 5 -ErrorAction Stop | Out-Null
            $hostOllama = $true
        } catch {
            # No Ollama on this machine.
        }

        if ($hostOllama) {
            Write-Host ""
            Write-Host "Ollama is running on this machine but no desktop container"
            Write-Host "can reach it."

            if (Enable-OllamaListenAll) {
                Write-Host "Host Ollama is now reachable from containers."
                $ollamaUrl = $script:DefaultOllamaUrl
            } else {
                Write-Host "Could not make the host's Ollama reachable."
            }
        }
    }

    # Last resort: the container Flowera manages itself. Small model, but it
    # needs nothing from the host and always works, which beats handing every
    # desktop a URL that answers nowhere. qwen2.5-coder:3b, not qwen3:0.6b -
    # the 0.6b model was too weak to reliably follow the agent's own
    # COMMAND/DONE/HELP contract (see internal/ollama.Model's comment).
    if (-not $ollamaUrl) {
        Write-Host ""
        Write-Host "Falling back to the bundled flowera-ollama container."
        $ollamaUrl = $BundledOllamaUrl
        Set-EnvVar "OLLAMA_MODEL" "qwen2.5-coder:3b"
    }

    Set-EnvVar "OLLAMA_URL" $ollamaUrl

    # The bundled container gets both models from the control plane itself
    # (internal/ollama.EnsureReady). A host or remote server does not.
    if ($ollamaUrl -ne $BundledOllamaUrl) {
        # The model named in .env is a choice, so it is pulled rather than
        # swapped out; only if that pull fails does OLLAMA_MODEL fall back
        # to a model the server already has.
        $wantedModel = Read-EnvVar "OLLAMA_MODEL"
        if (-not $wantedModel) { $wantedModel = "qwen2.5-coder:3b" }
        # @(): a function's output is unrolled, so one model would come back
        # as a bare string and [0] would be its first character.
        $availableModels = @(Get-OllamaModels $ollamaUrl)

        if (Test-ModelPresent $availableModels $wantedModel) {
            Write-Host "Chat model $wantedModel is present."
            Set-EnvVar "OLLAMA_MODEL" $wantedModel
        } else {
            Write-Host ""
            Write-Host "Pulling the chat model $wantedModel (once; a 3B model is ~2GB)."

            if (Invoke-OllamaPull $ollamaUrl $wantedModel) {
                Write-Host "Chat model $wantedModel is ready."
                Set-EnvVar "OLLAMA_MODEL" $wantedModel
            } elseif ($availableModels.Count -gt 0) {
                $firstModel = $availableModels[0]
                Write-Host "WARNING: could not pull $wantedModel; using $firstModel."
                Set-EnvVar "OLLAMA_MODEL" $firstModel
            } else {
                Write-Host "WARNING: could not pull $wantedModel, and that server has"
                Write-Host "no models at all. Desktops will not be able to reason until"
                Write-Host "you run: ollama pull $wantedModel"
                Set-EnvVar "OLLAMA_MODEL" $wantedModel
            }
        }

        if (Test-ModelPresent @(Get-OllamaModels $ollamaUrl) $EmbedModel) {
            Write-Host "Embedding model $EmbedModel is present."
        } else {
            Write-Host ""
            Write-Host "Pulling the embedding model $EmbedModel (~270MB, once)."

            # Not fatal. A desktop on the ollama or claude harness never
            # embeds anything, so a failed pull here must not stop the whole
            # stack coming up - it only costs TheBot its memory.
            if (Invoke-OllamaPull $ollamaUrl $EmbedModel) {
                Write-Host "Embedding model $EmbedModel is ready."
            } else {
                Write-Host "WARNING: could not pull $EmbedModel."
                Write-Host "The thebot harness will answer, but will not be able to"
                Write-Host "remember anything it learns. The other harnesses are"
                Write-Host "unaffected."
            }
        }
    }

    Write-Host ""
    Write-Host "Desktops will use OLLAMA_URL=$(Read-EnvVar 'OLLAMA_URL') OLLAMA_MODEL=$(Read-EnvVar 'OLLAMA_MODEL')"

    Write-Host ""
    Write-Host "=== Stopping Flowera ==="
    docker compose down
    if ($LASTEXITCODE -ne 0) { Exit-Build $LASTEXITCODE }

    Write-Host ""
    Write-Host "=== Removing old desktop containers ==="
    $containers = docker ps -a --filter "name=desktop_" --format "{{.ID}}"
    if ($containers) {
        docker rm -f $containers
        if ($LASTEXITCODE -ne 0) { Exit-Build $LASTEXITCODE }
    }

    Write-Host ""
    Write-Host "=== Removing old desktop volumes ==="
    $volumes = docker volume ls --filter "name=desktop_" --format "{{.Name}}"
    if ($volumes) {
        docker volume rm $volumes
        if ($LASTEXITCODE -ne 0) { Exit-Build $LASTEXITCODE }
    }

    Write-Host ""
    Write-Host "=== Starting Flowera ==="
    docker compose up --build -d
    if ($LASTEXITCODE -ne 0) { Exit-Build $LASTEXITCODE }

    Write-Host ""
    Write-Host "=== Waiting for dashboard ==="

    if (-not (Wait-ForDashboard -MaxAttempts 60 -DelaySeconds 2)) {
        Write-Host "ERROR: Flowera dashboard is not responding on port 8080."
        docker compose logs --tail=100
        Exit-Build 1
    }

    Write-Host ""
    Write-Host "========================================"
    Write-Host " Flowera is running"
    Write-Host "========================================"
    Write-Host ""
    Write-Host "Dashboard: http://localhost:8080"

    Write-Host ""
    Write-Host "=== Starting Cloudflare Tunnel ==="
    if (-not (Test-CommandAvailable "cloudflared")) {
        Write-Host "ERROR: cloudflared is not installed. Flowera is still running"
        Write-Host "locally; install cloudflared (.\scripts\windows\setup.ps1) for"
        Write-Host "access from other devices."
        Exit-Build 1
    }
    Write-Host ""

    # cloudflared logs to stderr, hence 2>&1. Echo every line while watching
    # for the trycloudflare.com URL, and report it to the control plane for
    # display in the dashboard's Mobile dropdown. "$_" rather than $_: in
    # Windows PowerShell 5.1 a redirected stderr line arrives as an
    # ErrorRecord, and only its string form is the line itself.
    cloudflared tunnel --protocol http2 --url http://127.0.0.1:8080 2>&1 | ForEach-Object {
        $line = "$_"
        Write-Host $line

        if ($line -match 'https://[a-zA-Z0-9.-]+\.trycloudflare\.com') {
            try {
                Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:8080/api/tunnel" `
                    -ContentType "application/json" `
                    -Body (@{ url = $Matches[0] } | ConvertTo-Json -Compress) `
                    -TimeoutSec 5 | Out-Null
            } catch {
                # Best effort: the tunnel works whether or not the dashboard
                # shows it.
            }
        }
    }
}
finally {
    if (-not $script:IntentionalExit) {
        Invoke-Cleanup
    }
}
#endregion

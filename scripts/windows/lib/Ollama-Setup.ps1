<#
.SYNOPSIS
    Shared by scripts/windows/build.ps1 and scripts/windows/rebuild.ps1.
.DESCRIPTION
    Dot-sourced, not executed directly: defines Invoke-EnsureOllamaReady,
    which finds an Ollama the desktop containers can actually reach, pulls
    the chat and embedding models onto it, and records OLLAMA_URL/
    OLLAMA_MODEL in .env.

    Requires: flowera-desktop:basic already built (the probes run inside
    it), flowera-network already created, and the current directory being
    the repo root. The caller is responsible for all three - this file only
    does the Ollama part, so build.ps1 and rebuild.ps1 don't each carry
    their own copy of it.
#>

# .env is read by the control plane (docker/Dockerfile copies it into the
# image) and by docker compose. Written as UTF-8 *without* a BOM and with LF
# endings: Windows PowerShell 5.1's own Set-Content/Out-File would add a BOM
# (or write UTF-16), and a BOM glued to the first key turns DEFAULT_PASSWORD
# into a variable nobody reads.
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
    data=json.dumps({'model': sys.argv[2], 'stream': True}).encode(),
    headers={'Content-Type': 'application/json'},
)
try:
    with urllib.request.urlopen(req) as resp:
        for line in resp:
            try:
                data = json.loads(line.decode().strip())
                status = data.get('status', '')
                total = data.get('total', 0)
                completed = data.get('completed', 0)
                if total > 0:
                    pct = int(completed * 100 / total)
                    print(f'\r  {status}: {pct}% ({completed // 1048576}MB / {total // 1048576}MB)', end='', flush=True)
                elif status:
                    print(f'\r  {status}...', end='', flush=True)
            except Exception:
                pass
        print('')
except Exception as exc:
    print(exc, file=sys.stderr)
    sys.exit(1)
'@

    # Out-Host, so nothing the pull prints becomes part of the return value.
    Invoke-InDesktopImage -Code $code -Arguments @($Url, $Model) | Out-Host
    return ($LASTEXITCODE -eq 0)
}

# The Windows counterpart of the Linux systemd step. The Ollama app for
# Windows listens on 127.0.0.1 only unless OLLAMA_HOST says otherwise, and it
# reads that from the user's environment when it starts - so set it there
# and restart the app. Docker Desktop normally forwards host.docker.internal
# to the host's loopback, which is why this rarely fires; when it does,
# nothing short of listening on more than loopback makes Ollama reachable.
function Enable-OllamaListenAll {
    param([string]$DefaultOllamaUrl)

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
        if (Test-OllamaReachable $DefaultOllamaUrl) {
            return $true
        }
        Start-Sleep -Seconds 2
    }

    return $false
}

# Ensures OLLAMA_URL/OLLAMA_MODEL in .env name a server the desktop
# containers can reach, with the chat and embedding models pulled onto it.
# Everything here writes .env, which docker/Dockerfile copies into the
# control plane image - so callers must run this before "docker compose
# build"/"up --build", not after. Assumes the current directory is the repo
# root (the caller has already Set-Location'd there).
function Invoke-EnsureOllamaReady {
    $script:EnvFile = Join-Path (Get-Location).Path ".env"
    $defaultOllamaUrl = "http://host.docker.internal:11434"
    $bundledOllamaUrl = "http://flowera-ollama:11434"
    # The chat model is not the only model. TheBot's harness embeds every
    # fact it learns, every document it indexes and every failure it
    # records, and Ollama refuses to embed with a chat model. See
    # internal/ollama.EmbedModel.
    $embedModel = "nomic-embed-text"

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
    if (-not $ollamaUrl -and (Test-OllamaReachable $defaultOllamaUrl)) {
        Write-Host "Host Ollama is reachable from containers."
        $ollamaUrl = $defaultOllamaUrl
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

            if (Enable-OllamaListenAll -DefaultOllamaUrl $defaultOllamaUrl) {
                Write-Host "Host Ollama is now reachable from containers."
                $ollamaUrl = $defaultOllamaUrl
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
        $ollamaUrl = $bundledOllamaUrl
        # A pinned model is replaced by a small one that always works; "auto"
        # stays auto - the control plane pulls what fits into the container.
        $pinned = Read-EnvVar "OLLAMA_MODEL"
        if ($pinned -and ($pinned -ne "auto")) { Set-EnvVar "OLLAMA_MODEL" "qwen2.5-coder:3b" }
    }

    Set-EnvVar "OLLAMA_URL" $ollamaUrl

    # The bundled container gets both models from the control plane itself
    # (internal/ollama.EnsureReady). A host or remote server does not.
    if ($ollamaUrl -ne $bundledOllamaUrl) {
        # The model named in .env is a choice, so it is pulled rather than
        # swapped out; only if that pull fails does OLLAMA_MODEL fall back
        # to a model the server already has.
        $wantedModel = Read-EnvVar "OLLAMA_MODEL"
        # @(): a function's output is unrolled, so one model would come back
        # as a bare string and [0] would be its first character.
        $availableModels = @(Get-OllamaModels $ollamaUrl)

        if ((-not $wantedModel) -or ($wantedModel -eq "auto")) {
            # "auto" (or unset): the control plane chooses the chat model for
            # this machine from what the server has (internal/ollama,
            # SelectModel), so .env is left alone rather than pinned. A server
            # with no chat model at all gets a small one to start from.
            $chatModels = @($availableModels | Where-Object { $_ -and -not $_.StartsWith($embedModel) })
            if ($chatModels.Count -gt 0) {
                Write-Host "Chat model: chosen automatically for this machine from what the server has."
            } else {
                Write-Host ""
                Write-Host "That server has no chat model; pulling qwen2.5-coder:3b (~2GB) as a starting point."
                if (-not (Invoke-OllamaPull $ollamaUrl "qwen2.5-coder:3b")) {
                    Write-Host "WARNING: could not pull it. Run: ollama pull qwen2.5-coder:3b"
                }
            }
        } elseif (Test-ModelPresent $availableModels $wantedModel) {
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

        if (Test-ModelPresent @(Get-OllamaModels $ollamaUrl) $embedModel) {
            Write-Host "Embedding model $embedModel is present."
        } else {
            Write-Host ""
            Write-Host "Pulling the embedding model $embedModel (~270MB, once)."

            # Not fatal. A desktop on the ollama or claude harness never
            # embeds anything, so a failed pull here must not stop the whole
            # stack coming up - it only costs TheBot its memory.
            if (Invoke-OllamaPull $ollamaUrl $embedModel) {
                Write-Host "Embedding model $embedModel is ready."
            } else {
                Write-Host "WARNING: could not pull $embedModel."
                Write-Host "The thebot harness will answer, but will not be able to"
                Write-Host "remember anything it learns. The other harnesses are"
                Write-Host "unaffected."
            }
        }
    }

    Write-Host ""
    Write-Host "Desktops will use OLLAMA_URL=$(Read-EnvVar 'OLLAMA_URL') OLLAMA_MODEL=$(Read-EnvVar 'OLLAMA_MODEL')"
}

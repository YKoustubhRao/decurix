<#
.SYNOPSIS
    Shared by scripts/windows/build.ps1 and scripts/windows/rebuild.ps1.
.DESCRIPTION
    Dot-sourced, not executed directly: defines Invoke-EnsureOllamaReady,
    which finds an Ollama the desktop containers can actually reach, pulls
    the chat and embedding models onto it, and records OLLAMA_URL/
    OLLAMA_MODEL in .env.

    Requires: decurix-desktop:basic already built (the probes run inside
    it), decurix-network already created, and the current directory being
    the repo root. The caller is responsible for all three - this file only
    does the Ollama part, so build.ps1 and rebuild.ps1 don't each carry
    their own copy of it.
#>

# .env is given to the control plane by docker compose (env_file) when it
# starts. Read and written as UTF-8 *without* a BOM and with LF endings:
# Windows PowerShell 5.1's own Get-Content reads it in the ANSI code page
# (so every run mangled a non-ASCII value a little more), and its
# Set-Content/Out-File would add a BOM (or write UTF-16) - a BOM glued to the
# first key turns DEFAULT_PASSWORD into a variable nobody reads.
function Read-EnvLines {
    if (-not (Test-Path -LiteralPath $script:EnvFile)) { return @() }
    $utf8 = New-Object System.Text.UTF8Encoding $false
    return @([System.IO.File]::ReadAllLines($script:EnvFile, $utf8) | ForEach-Object { $_.TrimEnd("`r") })
}

# Read-EnvVar returns Name's value as compose reads it: the last line that
# sets it, without one pair of surrounding quotes, and without a trailing
# " # comment" on an unquoted value.
function Read-EnvVar {
    param([string]$Name)

    $line = Read-EnvLines |
        Where-Object { $_ -match "^$([regex]::Escape($Name))=" } |
        Select-Object -Last 1
    if (-not $line) { return "" }

    $value = $line.Substring($Name.Length + 1)
    if ($value.Length -ge 2 -and ($value[0] -eq '"' -or $value[0] -eq "'") -and $value[-1] -eq $value[0]) {
        return $value.Substring(1, $value.Length - 2)
    }
    return ($value -replace '\s+#.*$', '').TrimEnd()
}

function Set-EnvVar {
    param([string]$Name, [string]$Value)

    $lines = @(Read-EnvLines)

    $pattern = "^$([regex]::Escape($Name))="
    $found = $false
    $kept = @()

    foreach ($line in $lines) {
        if ($line -match $pattern) {
            if (-not $found) { $kept += "$Name=$Value" }
            $found = $true
        } else {
            $kept += $line
        }
    }

    if (-not $found) {
        $kept += "$Name=$Value"
    }

    $utf8NoBom = New-Object System.Text.UTF8Encoding $false
    [System.IO.File]::WriteAllText(
        $script:EnvFile,
        (($kept -join "`n") + "`n"),
        $utf8NoBom
    )
}

# A setting this run needs but the person did not choose (a fallback server or
# model) is used for this run only: it goes into a compose override file,
# never into .env, so the next run asks again and the person's own choice
# survives a moment when their server was down. Callers start compose with
# the files in $script:ComposeFiles.
$script:RunOnly = [ordered]@{}
$script:ComposeFiles = @('-f', 'docker-compose.yaml')

function Set-RunOnly {
    param([string]$Name, [string]$Value)

    if ($Value -match '["\\\r\n]') {
        Write-Host "WARNING: not using $Name=$Value for this run (it holds a quote, a backslash or a line break)."
        return
    }
    $script:RunOnly[$Name] = $Value
    Write-Host ""
    Write-Host "!! THIS RUN ONLY: $Name=$Value"
    Write-Host "!! .env is left as you set it; the next run checks again."
}

# Get-EffectiveEnvVar is the value this run uses: a run-only one, else .env's.
function Get-EffectiveEnvVar {
    param([string]$Name)

    if ($script:RunOnly.Contains($Name)) { return $script:RunOnly[$Name] }
    return Read-EnvVar $Name
}

# Write-RunOverride writes the run-only settings as a compose override and
# adds it to $script:ComposeFiles.
function Write-RunOverride {
    $script:ComposeFiles = @('-f', 'docker-compose.yaml')
    if ($script:RunOnly.Count -eq 0) { return }
    $file = Join-Path ([System.IO.Path]::GetTempPath()) ("decurix-run-" + [guid]::NewGuid().ToString('N') + ".yaml")
    $text = "services:`n  desktop-manager:`n    environment:`n"
    foreach ($key in $script:RunOnly.Keys) { $text += "      ${key}: `"$($script:RunOnly[$key])`"`n" }
    [System.IO.File]::WriteAllText($file, $text, (New-Object System.Text.UTF8Encoding $false))
    $script:ComposeFiles += @('-f', $file)
}

# Invoke-InDesktopImage runs a Python snippet inside decurix-desktop:basic, on
# decurix-network, with the same host.docker.internal mapping every desktop
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
        --network decurix-network `
        --add-host host.docker.internal:host-gateway `
        --entrypoint python3 `
        decurix-desktop:basic `
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

# Docker Desktop forwards host.docker.internal to this computer's 127.0.0.1,
# where the Ollama app listens by default, so Ollama never needs to be on
# every address - and with no login, on every address it hands model
# management to the whole network. Restore-OllamaLoopback takes back what an
# earlier version of these scripts did (OLLAMA_HOST set to 0.0.0.0 for the user),
# and restarts the app so it listens on 127.0.0.1 again.
function Restore-OllamaLoopback {
    if ([Environment]::GetEnvironmentVariable("OLLAMA_HOST", "User") -ne "0.0.0.0") { return }

    Write-Host "An earlier Decurix put Ollama on every address (OLLAMA_HOST set to 0.0.0.0 for your"
    Write-Host "user). Removing that, so Ollama listens on 127.0.0.1 only."
    [Environment]::SetEnvironmentVariable("OLLAMA_HOST", $null, "User")
    Remove-Item Env:OLLAMA_HOST -ErrorAction SilentlyContinue

    $app = Join-Path $env:LOCALAPPDATA "Programs\Ollama\ollama app.exe"
    if (Test-Path -LiteralPath $app) {
        Get-Process -Name "ollama app", "ollama" -ErrorAction SilentlyContinue |
            Stop-Process -Force -ErrorAction SilentlyContinue
        Start-Sleep -Seconds 2
        Start-Process -FilePath $app
        Start-Sleep -Seconds 3
    } else {
        Write-Host "Restart Ollama yourself for this to take effect."
    }
}

# Ensures OLLAMA_URL/OLLAMA_MODEL name a server the desktop containers can
# reach, with the chat and embedding models pulled onto it. A fallback is
# used for this run only (Set-RunOnly); callers start compose with
# $script:ComposeFiles. Assumes the current directory is the repo root (the
# caller has already Set-Location'd there).
function Invoke-EnsureOllamaReady {
    $script:EnvFile = Join-Path (Get-Location).Path ".env"
    $defaultOllamaUrl = "http://host.docker.internal:11434"
    $bundledOllamaUrl = "http://decurix-ollama:11434"
    # The chat model is not the only model. TheBot's harness embeds every
    # fact it learns, every document it indexes and every failure it
    # records, and Ollama refuses to embed with a chat model. See
    # internal/ollama.EmbedModel.
    $embedModel = "nomic-embed-text"

    docker network inspect decurix-network *>$null
    if ($LASTEXITCODE -ne 0) {
        docker network create decurix-network | Out-Null
    }

    Restore-OllamaLoopback
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
            Write-Host "Ollama answers on this computer but not from a desktop container."
            Write-Host "Docker Desktop normally forwards host.docker.internal to this computer;"
            Write-Host "check that Docker Desktop (not another engine) is running, then re-run."
        }
    }

    # Last resort: the container Decurix manages itself. Small model, but it
    # needs nothing from the host and always works, which beats handing every
    # desktop a URL that answers nowhere. qwen2.5-coder:3b, not qwen3:0.6b -
    # the 0.6b model was too weak to reliably follow the agent's own
    # COMMAND/DONE/HELP contract (see internal/ollama.Model's comment).
    # For this run only (Set-RunOnly): a server that was down a moment must
    # not cost the person their own server and model for good.
    if (-not $ollamaUrl) {
        Write-Host ""
        Write-Host "WARNING: no Ollama the desktops can reach. Falling back to the bundled"
        Write-Host "decurix-ollama container for this run."
        $ollamaUrl = $bundledOllamaUrl
        # A pinned model is replaced by a small one that always works; "auto"
        # stays auto - the control plane pulls what fits into the container.
        $pinned = Read-EnvVar "OLLAMA_MODEL"
        if ($pinned -and ($pinned -ne "auto")) { Set-RunOnly "OLLAMA_MODEL" "qwen2.5-coder:3b" }
    }

    # .env keeps the person's server. A URL is written only when .env named
    # none and the one found is not the fallback; anything else is this run's.
    if ($ollamaUrl -ne $configuredUrl) {
        if ((-not $configuredUrl) -and ($ollamaUrl -ne $bundledOllamaUrl)) {
            Set-EnvVar "OLLAMA_URL" $ollamaUrl
        } else {
            Set-RunOnly "OLLAMA_URL" $ollamaUrl
        }
    }

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
        } else {
            Write-Host ""
            Write-Host "Pulling the chat model $wantedModel (once; a 3B model is ~2GB)."

            if (Invoke-OllamaPull $ollamaUrl $wantedModel) {
                Write-Host "Chat model $wantedModel is ready."
            } elseif ($availableModels.Count -gt 0) {
                $firstModel = $availableModels[0]
                Write-Host "WARNING: could not pull $wantedModel; using $firstModel for this run."
                Set-RunOnly "OLLAMA_MODEL" $firstModel
            } else {
                Write-Host "WARNING: could not pull $wantedModel, and that server has"
                Write-Host "no models at all. Desktops will not be able to reason until"
                Write-Host "you run: ollama pull $wantedModel"
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

    Write-RunOverride
    Write-Host ""
    Write-Host "Desktops will use OLLAMA_URL=$(Get-EffectiveEnvVar 'OLLAMA_URL') OLLAMA_MODEL=$(Get-EffectiveEnvVar 'OLLAMA_MODEL')"
    if ($script:RunOnly.Count -gt 0) {
        Write-Host "!! Some of that is for this run only (see above); .env is unchanged there."
    }
}

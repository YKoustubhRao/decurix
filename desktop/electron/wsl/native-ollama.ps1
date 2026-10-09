# Runs the shared model server on Windows, outside WSL's resource limits.
# ensure starts a hidden, independent serve process so scheduled workers still
# work after the Electron window closes. A separate port leaves user Ollama alone.
param(
    [ValidateSet('ensure', 'serve', 'stop', 'prepare', 'install')][string]$Action = 'ensure',
    [Parameter(Mandatory=$true)][string]$BindAddress,
    [Parameter(Mandatory=$true)][string]$SettingsFile,
    [string]$Store = (Join-Path $env:LOCALAPPDATA 'Decurix\ollama')
)
$ErrorActionPreference = 'Stop'
$runtime = Join-Path $Store 'runtime-v0.40.1'
$exe = Join-Path $runtime 'ollama.exe'
$complete = Join-Path $runtime '.complete'
$url = "http://" + $BindAddress + ':11435'

# Read only Ollama's server settings, never execute .env as PowerShell.
$settings = @{}
foreach ($line in [IO.File]::ReadAllLines($SettingsFile)) {
    if ($line -match '^\s*([A-Z][A-Z0-9_]*)\s*=\s*(.*?)\s*$') {
        $key = $Matches[1]; $value = $Matches[2]
        if ($value -match '^(["''])') { $value = $value.Trim([char]34, [char]39) }
        else { $value = ($value -replace '\s+#.*$', '').Trim() }
        $settings[$key] = $value
    }
}
function Ready {
    try { $null = Invoke-RestMethod -Uri ($url + '/api/tags') -TimeoutSec 3 -Proxy $null; return $true }
    catch { return $false }
}
# Only this application's managed model directory is disposable.
function Model-Directory {
    if ($settings['OLLAMA_MODELS']) { return [IO.Path]::GetFullPath((Join-Path $settings['OLLAMA_MODELS'] 'decurix-managed')) }
    return [IO.Path]::GetFullPath((Join-Path $Store 'models'))
}
if ($Action -eq 'stop') {
    Write-Output 'Unloading local models and stopping inference...'
    if (Ready) {
        try {
            $loaded = Invoke-RestMethod -Uri ($url + '/api/ps') -TimeoutSec 5 -Proxy $null
            foreach ($loadedModel in $loaded.models) {
                $body = @{model=$loadedModel.name; keep_alive=0; stream=$false} | ConvertTo-Json -Compress
                $null = Invoke-RestMethod -Method Post -Uri ($url + '/api/generate') -ContentType 'application/json' -Body $body -TimeoutSec 10 -Proxy $null
            }
        } catch { Write-Output 'Stopping the server to release remaining model memory.' }
    }
    $prefix = [IO.Path]::GetFullPath($Store).TrimEnd('\') + '\'
    $owned = @(Get-CimInstance Win32_Process | Where-Object {
        $_.ExecutablePath -and $_.ExecutablePath.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)
    })
    foreach ($process in $owned) {
        Stop-Process -Id $process.ProcessId -Force -ErrorAction Stop
        Wait-Process -Id $process.ProcessId -Timeout 10 -ErrorAction SilentlyContinue
    }
    $models = Model-Directory
    $expected = if ($settings['OLLAMA_MODELS']) { [IO.Path]::GetFullPath($settings['OLLAMA_MODELS']).TrimEnd('\') + '\decurix-managed' } else { [IO.Path]::GetFullPath($Store).TrimEnd('\') + '\models' }
    if (-not $models.Equals($expected, [StringComparison]::OrdinalIgnoreCase)) { throw 'Refusing to delete a model directory outside the managed store.' }
    if (Test-Path -LiteralPath $models) {
        # Do not follow junctions or symlinks into another model store.
        $items = @((Get-Item -LiteralPath $models -Force)) + @(Get-ChildItem -LiteralPath $models -Recurse -Force)
        if ($items | Where-Object { $_.Attributes -band [IO.FileAttributes]::ReparsePoint }) { throw 'Refusing to delete a model directory containing a junction or symbolic link.' }
        Write-Output 'Deleting downloaded local model files...'
        Remove-Item -LiteralPath $models -Recurse -Force
    }
    Write-Output 'Local models deleted and memory released. Ollama runtime is kept.'
    exit 0
}
if ($Action -eq 'prepare') {
    if (-not (Ready)) { throw 'Start local hosting before preparing models.' }
    Write-Output 'Scanning Windows RAM, GPU and CPU capacity...'
    $pick = Invoke-RestMethod -Uri ('http://' + $BindAddress + ':11436/selection?can_pull=1') -TimeoutSec 40 -Proxy $null
    $model = $settings['OLLAMA_MODEL']
    if (-not $model -or $model -eq 'auto') {
        if (-not $pick.selection.fits) { throw 'No recommended model fits the current free resources. Free memory before enabling local hosting.' }
        $model = $pick.selection.model.name
    }
    $state = @{model=$model; resources=$pick.resources; detail=('Selected ' + $model + ' for this computer.')} | ConvertTo-Json -Depth 6 -Compress
    Write-Output ('DECURIX_STATE ' + $state)
    if (-not $model -or $model.StartsWith('-')) { throw 'Invalid model name.' }
    $env:OLLAMA_HOST = $url
    foreach ($name in @($model, 'nomic-embed-text')) {
        Write-Output ('Preparing model ' + $name + ' (cached layers are reused)...')
        & $exe pull $name
        if ($LASTEXITCODE -ne 0) { throw ('Could not prepare model ' + $name) }
    }
    # Copying the manifest shares the downloaded weights; workers keep a stable
    # model name across off/on cycles and new hardware-based selections.
    $aliasBody = @{source=$model; destination='decurix-local'} | ConvertTo-Json -Compress
    $null = Invoke-RestMethod -Method Post -Uri ($url + '/api/copy') -ContentType 'application/json' -Body $aliasBody -TimeoutSec 30 -Proxy $null
    Write-Output ('Loading ' + $model + ' into memory...')
    $keepAlive = if ($settings['OLLAMA_KEEP_ALIVE']) { $settings['OLLAMA_KEEP_ALIVE'] } else { -1 }
    $body = @{model='decurix-local'; keep_alive=$keepAlive; stream=$false; options=@{num_ctx=8192}} | ConvertTo-Json -Depth 4 -Compress
    $null = Invoke-RestMethod -Method Post -Uri ($url + '/api/generate') -ContentType 'application/json' -Body $body -TimeoutSec 300 -Proxy $null
    Write-Output ('Local inference is ready with ' + $model + '.')
    exit 0
}
if ($Action -eq 'serve') {
    $env:OLLAMA_HOST = $BindAddress + ':11435'
    $env:OLLAMA_MODELS = Model-Directory
    $env:OLLAMA_NUM_PARALLEL = '1'
    $env:OLLAMA_MAX_LOADED_MODELS = '1'
    $env:OLLAMA_FLASH_ATTENTION = '1'
    $env:OLLAMA_KV_CACHE_TYPE = 'q8_0'
    $env:OLLAMA_KEEP_ALIVE = '-1'
    foreach ($key in @('OLLAMA_NUM_PARALLEL','OLLAMA_MAX_LOADED_MODELS','OLLAMA_FLASH_ATTENTION','OLLAMA_KV_CACHE_TYPE','OLLAMA_KEEP_ALIVE','OLLAMA_MAX_QUEUE','OLLAMA_CONTEXT_LENGTH')) {
        if ($settings[$key]) { [Environment]::SetEnvironmentVariable($key, $settings[$key], 'Process') }
    }
    # Ollama writes normal startup messages to stderr. PowerShell 5 turns
    # native stderr redirected with *>> into a terminating NativeCommandError
    # under ErrorActionPreference=Stop, killing an otherwise healthy server.
    # Redirect at the process boundary instead and preserve its real exit code.
    $server = Start-Process -FilePath $exe -ArgumentList 'serve' -WorkingDirectory $runtime -WindowStyle Hidden -Wait -PassThru `
        -RedirectStandardOutput (Join-Path $Store 'server.stdout.log') -RedirectStandardError (Join-Path $Store 'server.log')
    exit $server.ExitCode
}

# Keep selection alongside native inference. The helper uses the existing Go
# selector and real Windows memory/CPU/GPU probes on every request.
function Ensure-Selector {
    $shipped = Join-Path $PSScriptRoot 'decurix-model-selector.exe'
    if (-not (Test-Path -LiteralPath $shipped)) { throw 'Windows model selector is missing. Rebuild the Windows app.' }
    New-Item -ItemType Directory -Force -Path $Store | Out-Null
    $digest = (Get-FileHash -LiteralPath $shipped -Algorithm SHA256).Hash.ToLowerInvariant()
    $selector = Join-Path $Store ('selector-' + $digest + '.exe')
    $health = 'http://' + $BindAddress + ':11436/health'
    $stamp = Join-Path $Store 'selector-settings.sha256'
    $settingsDigest = (Get-FileHash -LiteralPath $SettingsFile -Algorithm SHA256).Hash + '|' + $BindAddress
    function Selector-Ready {
        try { $status = Invoke-RestMethod -Uri $health -TimeoutSec 3 -Proxy $null; return $status.service -eq 'decurix-model-selector' }
        catch { return $false }
    }
    if ((Test-Path -LiteralPath $selector) -and (Test-Path -LiteralPath $stamp) -and
        [IO.File]::ReadAllText($stamp) -eq $settingsDigest -and (Selector-Ready)) { return }
    # Only app-owned selector processes are replaced on an app upgrade.
    $prefix = [IO.Path]::GetFullPath($Store).TrimEnd('\') + '\selector-'
    Get-CimInstance Win32_Process | Where-Object {
        $_.ExecutablePath -and $_.ExecutablePath.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)
    } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
    if (-not (Test-Path -LiteralPath $selector)) { Copy-Item -LiteralPath $shipped -Destination $selector }
    function Literal([string]$value) { return "'" + $value.Replace("'", "''") + "'" }
    $script = '& ' + (Literal $selector) + ' -listen ' + (Literal ($BindAddress + ':11436')) + ' -ollama ' + (Literal $url) + ' -settings ' + (Literal $SettingsFile) + ' *>> ' + (Literal (Join-Path $Store 'selector.log'))
    $encoded = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($script))
    $startup = New-CimInstance -ClassName Win32_ProcessStartup -ClientOnly -Property @{ShowWindow=[uint16]0}
    $result = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{
        CommandLine=('powershell.exe -NoProfile -NonInteractive -EncodedCommand ' + $encoded)
        ProcessStartupInformation=$startup
    }
    if ($result.ReturnValue -ne 0) { throw 'Could not start Windows model selection.' }
    for ($attempt = 0; $attempt -lt 30; $attempt++) {
        if (Selector-Ready) {
            [IO.File]::WriteAllText($stamp, $settingsDigest)
            Write-Output 'Windows resource-based model selector is ready.'; return
        }
        Start-Sleep -Seconds 1
    }
    throw "Windows model selector did not start. Check $Store\selector.log."
}

New-Item -ItemType Directory -Force -Path $Store | Out-Null
if (-not (Test-Path -LiteralPath $complete) -or -not (Test-Path -LiteralPath $exe)) {
    # Standalone CLI includes CPU and NVIDIA libraries. Pin both URL and hash.
    $download = 'https://github.com/ollama/ollama/releases/download/v0.40.1/ollama-windows-amd64.zip'
    $expected = 'b394d14436d38032f23190e3f14eb2c6dad5ebbe4e192414f74c8fdca01703ab'
    $zip = Join-Path $Store 'ollama-windows-amd64.zip'
    Write-Output 'Downloading native Windows Ollama (first start only)...'
    $ProgressPreference = 'SilentlyContinue'
    if (-not (Test-Path -LiteralPath $zip) -or (Get-FileHash -LiteralPath $zip -Algorithm SHA256).Hash -ine $expected) {
        $curl = Get-Command curl.exe -ErrorAction SilentlyContinue
        if ($curl) {
            # Keep partial transfers for Retry and print curl's byte/rate progress.
            $partial = $zip + '.partial'
            $alreadyComplete = (Test-Path -LiteralPath $partial) -and (Get-FileHash -LiteralPath $partial -Algorithm SHA256).Hash -ieq $expected
            if (-not $alreadyComplete) {
                & $curl.Source --location --fail --retry 3 --connect-timeout 20 --speed-limit 1024 --speed-time 60 --continue-at - --output $partial $download
                if ($LASTEXITCODE -ne 0) {
                    if ($LASTEXITCODE -in @(22,33)) {
                        # A rejected range or HTTP error must not trap Retry on a bad partial file.
                        if (Test-Path -LiteralPath $partial) { Remove-Item -LiteralPath $partial }
                        throw 'Ollama download failed. Retry starts a fresh transfer.'
                    }
                    throw 'Ollama download interrupted. Retry resumes the partial download.'
                }
            }
            if ((Get-FileHash -LiteralPath $partial -Algorithm SHA256).Hash -ine $expected) {
                Remove-Item -LiteralPath $partial
                throw 'Native Ollama download failed SHA-256 verification. Retry downloads a fresh copy.'
            }
            Move-Item -LiteralPath $partial -Destination $zip -Force
        } else {
            Invoke-WebRequest -Uri $download -OutFile $zip -UseBasicParsing -TimeoutSec 900
        }
    }
    if ((Get-FileHash -LiteralPath $zip -Algorithm SHA256).Hash -ine $expected) { throw 'Native Ollama download failed SHA-256 verification.' }
    Expand-Archive -LiteralPath $zip -DestinationPath $runtime -Force
    if (-not (Test-Path -LiteralPath $exe)) { throw 'Native Ollama archive did not contain ollama.exe.' }
    [IO.File]::WriteAllText($complete, 'v0.40.1')
    Remove-Item -LiteralPath $zip
}
if (-not (Test-Path -LiteralPath $exe)) { throw 'Native Ollama archive did not contain ollama.exe.' }
if ($Action -eq 'install') { Write-Output 'Ollama runtime installed. Hosting is off until enabled.'; exit 0 }
if (Ready) { Ensure-Selector; Write-Output 'Windows Ollama is ready.'; exit 0 }
# Only old app-owned servers are stopped, never an installed user Ollama.
$prefix = [IO.Path]::GetFullPath($Store).TrimEnd('\') + '\'
Get-CimInstance Win32_Process | Where-Object {
    $_.ExecutablePath -and $_.ExecutablePath.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)
} | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }

# WMI avoids Electron's child-process job and Windows Terminal windows.
# Quote literal strings in a script block rather than building shell arguments.
function Quote([string]$value) { return "'" + $value.Replace("'", "''") + "'" }
$script = '& ' + (Quote $PSCommandPath) + ' -Action serve -BindAddress ' + (Quote $BindAddress) + ' -SettingsFile ' + (Quote $SettingsFile) + ' -Store ' + (Quote $Store)
$script = 'try { ' + $script + ' } catch { $_ | Out-String | Add-Content -LiteralPath ' + (Quote (Join-Path $Store 'launcher.log')) + '; exit 1 }'
$encoded = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($script))
$command = 'powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand ' + $encoded
$startup = New-CimInstance -ClassName Win32_ProcessStartup -ClientOnly -Property @{ShowWindow=[uint16]0}
$result = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{CommandLine=$command; ProcessStartupInformation=$startup}
if ($result.ReturnValue -ne 0) { throw 'Could not start native Windows Ollama.' }
for ($attempt = 0; $attempt -lt 60; $attempt++) {
    if (Ready) { Ensure-Selector; Write-Output 'Windows Ollama is ready.'; exit 0 }
    if (-not (Get-Process -Id $result.ProcessId -ErrorAction SilentlyContinue)) {
        Get-Content -LiteralPath (Join-Path $Store 'launcher.log') -Tail 8 -ErrorAction SilentlyContinue
        Get-Content -LiteralPath (Join-Path $Store 'server.log') -Tail 8 -ErrorAction SilentlyContinue
        throw "Windows Ollama exited during startup. Check $Store\server.log and launcher.log."
    }
    Start-Sleep -Seconds 1
}
throw "Windows Ollama did not start. Check $Store\server.log and launcher.log."

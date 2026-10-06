<#
.SYNOPSIS
    Build the Decurix Windows Electron installer.
.DESCRIPTION
    Run in Windows PowerShell from any directory. Requires Go 1.26+, Node.js
    and npm, and an internet connection the first time (Electron, and the
    pinned downloads below). The resulting installer is under dist/electron/.

    The app runs the same stack as on a Mac or Linux - the Linux control plane
    in Docker, and its workers as Linux desktops (XFCE, shown through noVNC) -
    in a WSL distro of its own. Nothing else is installed by the person: the
    app imports the distro, installs Docker Engine in it and turns WSL on when
    Windows lacks it (desktop/electron/main.js). The installer carries:

        resources\stack\                the Linux control plane, its Dockerfile,
                                        the compose file, VERSION, and the
                                        phone build (decurix-android-arm64)
        resources\wsl\                  the distro: Alpine's minirootfs, and
                                        setup.sh / dockerd.sh run inside it
        resources\cloudflared.exe       the phone link's tunnel
.PARAMETER MinFreeMemoryMB
    Free RAM the build insists on before it starts (default 3072). On a small
    machine a lower value still builds, slowly: Go and Node are capped below.
#>
param(
    [int]$MinFreeMemoryMB = 3072
)

$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$electron = Join-Path $repo 'desktop/electron'
$stage = Join-Path $electron 'build'
$stack = Join-Path $stage 'stack'
$wsl = Join-Path $stage 'wsl'

# What the installer carries besides our own code, pinned by SHA-256 so a
# changed download is refused rather than shipped. To move to a newer release,
# change the URL and the hash together (cloudflared: the "digest" of the asset
# in GitHub's release API; Alpine: latest-releases.yaml beside the file).
$downloads = @(
    @{
        Name   = 'cloudflared.exe'
        Url    = 'https://github.com/cloudflare/cloudflared/releases/download/2026.9.3/cloudflared-windows-amd64.exe'
        Sha256 = 'f096265ec2fcbe9bb6e2d64268db167ced3fcbb83d894bdb9e2fcdb26f2ea7e2'
    },
    @{
        Name   = 'alpine-minirootfs.tar.gz'
        Url    = 'https://dl-cdn.alpinelinux.org/alpine/v3.24/releases/x86_64/alpine-minirootfs-3.24.2-x86_64.tar.gz'
        Sha256 = 'c5ca053cfe1d85c5b96dff8b9bc57045f7f184a30ffb6b65776409ca90388677'
    }
)

# npm.cmd uses cmd.exe for lifecycle scripts, and cmd.exe cannot use a WSL UNC
# path as its working directory. Build from a Windows drive checkout.
if ($repo.StartsWith('\\')) {
    throw 'Copy the repository to a Windows drive (for example C:\src\decurix) before building. npm cannot build from a WSL UNC path.'
}

$os = Get-CimInstance Win32_OperatingSystem
$freeMemoryMB = [int]($os.FreePhysicalMemory / 1024)
if ($freeMemoryMB -lt $MinFreeMemoryMB) {
    throw "Only $freeMemoryMB MB of RAM is free. Close other apps until at least $MinFreeMemoryMB MB is free before building, or pass -MinFreeMemoryMB with a lower value."
}

foreach ($tool in @('go', 'node', 'npm')) {
    if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) {
        throw "$tool is required to build the Windows app."
    }
}

# Everything the control plane embeds is built into Linux images, so a script
# with CRLF endings would fail inside a desktop ("required file not found").
# .gitattributes keeps them LF; this catches a checkout made before it.
$crlf = @(Get-ChildItem (Join-Path $repo 'agent'), (Join-Path $electron 'wsl') -Recurse -File |
    Where-Object { $_.FullName -notmatch '\\(node_modules|__pycache__|thebot)\\' } |
    Where-Object {
        $bytes = [IO.File]::ReadAllBytes($_.FullName) | Select-Object -First 512
        ($bytes.Count -gt 2 -and $bytes[0] -eq 0x23 -and $bytes[1] -eq 0x21) -and ([Text.Encoding]::ASCII.GetString($bytes) -match "`r`n")
    })
if ($crlf.Count -gt 0) {
    throw "These scripts have Windows (CRLF) line endings and would not run in Linux: $($crlf.FullName -join ', '). Check them out again with LF endings (see .gitattributes)."
}

Write-Host 'Checking Electron launcher...'
& node --test (Join-Path $electron 'main.test.js') (Join-Path $electron 'mobile-access.test.js')
if ($LASTEXITCODE -ne 0) { throw 'Electron launcher tests failed.' }

$version = 'dev'
if (Get-Command git -ErrorAction SilentlyContinue) {
    $description = git -C $repo describe --tags --always --dirty 2>$null
    if ($LASTEXITCODE -eq 0 -and $description) { $version = $description.Trim() }
}

New-Item -ItemType Directory -Force -Path $stage | Out-Null
# What earlier versions of this script staged here and the installer no longer carries.
foreach ($old in @('runtime', 'decurix.exe', 'decurix', 'cloudflared', 'python-embed.zip', '*.whl')) {
    Remove-Item (Join-Path $stage $old) -Recurse -Force -ErrorAction SilentlyContinue
}

foreach ($download in $downloads) {
    $target = Join-Path $stage $download.Name
    if ((Test-Path $target) -and (Get-FileHash $target -Algorithm SHA256).Hash -eq $download.Sha256) { continue }
    Write-Host "Downloading $($download.Name)..."
    $oldProgress = $ProgressPreference
    $ProgressPreference = 'SilentlyContinue'   # the progress bar slows a download several times over
    try { Invoke-WebRequest -Uri $download.Url -OutFile $target -UseBasicParsing } finally { $ProgressPreference = $oldProgress }
    $hash = (Get-FileHash $target -Algorithm SHA256).Hash
    if ($hash -ne $download.Sha256) {
        Remove-Item $target
        throw "$($download.Name) is not the file this build was pinned to (SHA-256 $hash, expected $($download.Sha256))."
    }
}

# The distro, staged from scratch.
if (Test-Path $wsl) { Remove-Item $wsl -Recurse -Force }
New-Item -ItemType Directory -Force -Path $wsl | Out-Null
Copy-Item (Join-Path $stage 'alpine-minirootfs.tar.gz') $wsl
Copy-Item (Join-Path $electron 'wsl/setup.sh'), (Join-Path $electron 'wsl/dockerd.sh') $wsl

if (Test-Path $stack) { Remove-Item $stack -Recurse -Force }
New-Item -ItemType Directory -Force -Path $stack | Out-Null

$oldGoos = $env:GOOS
$oldGoarch = $env:GOARCH
$oldCgo = $env:CGO_ENABLED
$oldGoFlags = $env:GOFLAGS
$oldGoMaxProcs = $env:GOMAXPROCS
$oldGoMemLimit = $env:GOMEMLIMIT
Push-Location $repo
try {
    $env:CGO_ENABLED = '0'
    $env:GOFLAGS = '-p=1'
    $env:GOMAXPROCS = '2'
    $env:GOMEMLIMIT = '512MiB'

    # TheBot is a Go module of its own, which go:embed skips: pack it into the
    # build context the control plane carries (embed_gen.go). On this machine,
    # before GOOS is set.
    Remove-Item Env:GOOS -ErrorAction SilentlyContinue
    Remove-Item Env:GOARCH -ErrorAction SilentlyContinue
    Write-Host 'Packing the desktop build context...'
    & go generate .
    if ($LASTEXITCODE -ne 0) { throw 'go generate failed.' }

    $env:GOOS = 'linux'
    $env:GOARCH = 'amd64'
    Write-Host 'Building the Linux control plane...'
    & go build -trimpath -ldflags "-s -w -X main.version=$version" -o (Join-Path $stack 'decurix') ./cmd/decurix
    if ($LASTEXITCODE -ne 0) { throw 'Linux Go build failed.' }

    $env:GOOS = 'android'
    $env:GOARCH = 'arm64'
    Write-Host 'Building Android phone helper...'
    & go build -trimpath -ldflags "-s -w -X main.version=$version" -o (Join-Path $stack 'decurix-android-arm64') ./cmd/decurix
    if ($LASTEXITCODE -ne 0) { throw 'Android Go build failed.' }
} finally {
    Pop-Location
    $env:GOOS = $oldGoos
    $env:GOARCH = $oldGoarch
    $env:CGO_ENABLED = $oldCgo
    $env:GOFLAGS = $oldGoFlags
    $env:GOMAXPROCS = $oldGoMaxProcs
    $env:GOMEMLIMIT = $oldGoMemLimit
}

# The control plane's image and compose file, as scripts/linux/build_app.sh
# stages them: the compose file builds from the Dockerfile beside it.
Copy-Item (Join-Path $electron 'stack/Dockerfile') (Join-Path $stack 'Dockerfile')
$compose = [IO.File]::ReadAllText((Join-Path $repo 'docker-compose.yaml'))
if ($compose -notmatch 'dockerfile: docker/Dockerfile') {
    throw 'docker-compose.yaml no longer names docker/Dockerfile; update this script.'
}
$utf8 = New-Object System.Text.UTF8Encoding $false
[IO.File]::WriteAllText((Join-Path $stack 'docker-compose.yaml'), ($compose -replace 'dockerfile: docker/Dockerfile', 'dockerfile: Dockerfile' -replace "`r`n", "`n"), $utf8)
[IO.File]::WriteAllText((Join-Path $stack 'VERSION'), "$version $((Get-Date).ToUniversalTime().ToString('yyyyMMddTHHmmssZ'))`n", $utf8)

Push-Location $electron
$oldNodeOptions = $env:NODE_OPTIONS
$oldNpmSockets = $env:npm_config_maxsockets
try {
    if (-not $env:NODE_OPTIONS) { $env:NODE_OPTIONS = '--max-old-space-size=1024' }
    $env:npm_config_maxsockets = '2'
    Write-Host 'Installing pinned Electron dependencies...'
    & npm ci
    if ($LASTEXITCODE -ne 0) { throw 'npm ci failed.' }
    Write-Host 'Building Windows installer...'
    & npm run dist:win
    if ($LASTEXITCODE -ne 0) { throw 'Electron installer build failed.' }
} finally {
    $env:NODE_OPTIONS = $oldNodeOptions
    $env:npm_config_maxsockets = $oldNpmSockets
    Pop-Location
}

Write-Host "Installer: $(Join-Path $repo 'dist/electron')"

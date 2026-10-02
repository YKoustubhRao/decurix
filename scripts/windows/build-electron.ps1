<#
.SYNOPSIS
    Build the Decurix Windows Electron installer.
.DESCRIPTION
    Run in Windows PowerShell from any directory. Requires Go 1.26+ (the Go
    toolchain fetches 1.27 for TheBot's engine by itself), Node.js and npm,
    and an internet connection the first time (Electron, and the two pinned
    downloads below). The resulting installer is under dist/electron/.

    The installed app needs nothing else on the machine: no Docker, no WSL,
    no Python. Its workers are native - each a private desktop of the person's
    own computer - and everything one runs ships in the installer:

        resources\decurix.exe           the Windows control plane
        resources\runtime\python\       the Python that runs a worker's agent, with the
                                        two packages its document tools need
        resources\runtime\agent\        the agent (agent\native_worker.py + decurix_agent)
        resources\runtime\soul.exe      TheBot's engine
        resources\runtime\decurix-ui.exe  how the agent sees and works its desktop
        resources\cloudflared.exe       the phone link's tunnel

    The local model runtime (Ollama) and a model are fetched by the control
    plane itself the first time a worker needs them.
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
$runtime = Join-Path $stage 'runtime'

# What the installer carries besides our own code, pinned by SHA-256 so a
# changed download is refused rather than shipped. To move to a newer release,
# change the URL and the hash together (cloudflared: the "digest" of the asset
# in GitHub's release API; Python: hash the file from python.org).
$downloads = @(
    @{
        Name   = 'python-embed.zip'
        Url    = 'https://www.python.org/ftp/python/3.12.10/python-3.12.10-embed-amd64.zip'
        Sha256 = '4acbed6dd1c744b0376e3b1cf57ce906f9dc9e95e68824584c8099a63025a3c3'
    },
    @{
        Name   = 'cloudflared.exe'
        Url    = 'https://github.com/cloudflare/cloudflared/releases/download/2026.9.3/cloudflared-windows-amd64.exe'
        Sha256 = 'f096265ec2fcbe9bb6e2d64268db167ced3fcbb83d894bdb9e2fcdb26f2ea7e2'
    },
    # The agent's document tools (tools/office.py): PDFs and workbooks. Pure
    # Python, so a wheel is only a zip to unpack - the embeddable Python has no
    # pip, and the build needs none.
    @{
        Name   = 'pypdf-6.19.0-py3-none-any.whl'
        Url    = 'https://files.pythonhosted.org/packages/3c/2c/c43c03eaf630435f023f1dc61ec4a4a78951ad5530a62c71cc89bde307b7/pypdf-6.19.0-py3-none-any.whl'
        Sha256 = '7e5d6e730e7dae87d560a2cee218b852f6498c8be61966f3cd02ead971e48d14'
    },
    @{
        Name   = 'openpyxl-3.1.5-py2.py3-none-any.whl'
        Url    = 'https://files.pythonhosted.org/packages/c0/da/977ded879c29cbd04de313843e76868e6e13408a94ed6b987245dc7c8506/openpyxl-3.1.5-py2.py3-none-any.whl'
        Sha256 = '5282c12b107bffeef825f4617dc029afaf41d0ea60823bbb665ef3079dc79de2'
    },
    @{
        Name   = 'et_xmlfile-2.0.0-py3-none-any.whl'   # openpyxl's one dependency
        Url    = 'https://files.pythonhosted.org/packages/c1/8b/5fe2cc11fee489817272089c4203e679c63b570a5aaeb18d852ae3cbba6a/et_xmlfile-2.0.0-py3-none-any.whl'
        Sha256 = '7a91720bc756843502c3b7504c77b8fe44217c85c537d85037f0f536151b2caa'
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
foreach ($old in @('decurix', 'alpine-minirootfs.tar.gz', 'cloudflared')) {
    Remove-Item (Join-Path $stage $old) -ErrorAction SilentlyContinue
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

# The worker runtime, staged from scratch so nothing a previous build left
# behind (a renamed module, a stale .pyc) ships.
Write-Host 'Staging the worker runtime...'
if (Test-Path $runtime) { Remove-Item $runtime -Recurse -Force }
New-Item -ItemType Directory -Force -Path (Join-Path $runtime 'agent') | Out-Null
Expand-Archive -Path (Join-Path $stage 'python-embed.zip') -DestinationPath (Join-Path $runtime 'python')
Copy-Item (Join-Path $repo 'agent/native_worker.py') (Join-Path $runtime 'agent')
& robocopy (Join-Path $repo 'agent/decurix_agent') (Join-Path $runtime 'agent/decurix_agent') /E /XD __pycache__ /XF *.pyc /NFL /NDL /NJH /NJS /NP | Out-Null
if ($LASTEXITCODE -ge 8) { throw 'Copying the agent failed.' }

# The embeddable Python takes its module path from python*._pth alone - it
# ignores PYTHONPATH - so the agent's folder is named there: `python -m
# decurix_agent.computer` (the worker's gui command) finds the agent.
$pth = Get-ChildItem (Join-Path $runtime 'python') -Filter 'python*._pth' | Select-Object -First 1
if (-not $pth) { throw 'The embeddable Python has no ._pth file to name the agent folder in.' }
Add-Content -Path $pth.FullName -Value '..\agent' -Encoding ASCII

# The packages the agent's document tools import, unpacked where that Python
# looks for them (the same file names it).
$sitePackages = Join-Path $runtime 'python\Lib\site-packages'
New-Item -ItemType Directory -Force -Path $sitePackages | Out-Null
Add-Type -AssemblyName System.IO.Compression.FileSystem
foreach ($wheel in Get-ChildItem $stage -Filter '*.whl') {
    [System.IO.Compression.ZipFile]::ExtractToDirectory($wheel.FullName, $sitePackages)
}
Add-Content -Path $pth.FullName -Value 'Lib\site-packages' -Encoding ASCII
& (Join-Path $runtime 'python\python.exe') -c "import pypdf, openpyxl"
if ($LASTEXITCODE -ne 0) { throw "The worker's Python cannot import its document packages." }

# decurix-ui.exe: how a worker's agent sees and works its own desktop. Built
# with the C# compiler that is part of Windows itself (.NET Framework), so the
# build needs no SDK - and the program runs on that same framework, which
# every Windows 10 and 11 has.
Write-Host "Building the worker's screen helper..."
$framework = [Runtime.InteropServices.RuntimeEnvironment]::GetRuntimeDirectory()
$csc = Join-Path $framework 'csc.exe'
if (-not (Test-Path $csc)) { throw "The .NET Framework C# compiler was not found at $csc." }
& $csc /nologo /target:exe /platform:x64 /optimize+ "/out:$(Join-Path $runtime 'decurix-ui.exe')" "/lib:$(Join-Path $framework 'WPF')" `
    /r:UIAutomationClient.dll /r:UIAutomationTypes.dll /r:WindowsBase.dll /r:System.Drawing.dll /r:System.Web.Extensions.dll `
    (Join-Path $repo 'agent/windows/decurix-ui.cs')
if ($LASTEXITCODE -ne 0) { throw 'Building the screen helper failed.' }

$oldGoos = $env:GOOS
$oldGoarch = $env:GOARCH
$oldCgo = $env:CGO_ENABLED
$oldGoFlags = $env:GOFLAGS
$oldGoMaxProcs = $env:GOMAXPROCS
$oldGoMemLimit = $env:GOMEMLIMIT
try {
    $env:CGO_ENABLED = '0'
    $env:GOFLAGS = '-p=1'
    $env:GOMAXPROCS = '2'
    $env:GOMEMLIMIT = '512MiB'
    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'

    Push-Location $repo
    try {
        Write-Host 'Building the Windows control plane...'
        & go build -trimpath -ldflags "-s -w -X main.version=$version" -o (Join-Path $stage 'decurix.exe') ./cmd/decurix
        if ($LASTEXITCODE -ne 0) { throw 'Windows Go build failed.' }

        $env:GOOS = 'android'
        $env:GOARCH = 'arm64'
        Write-Host 'Building Android phone helper...'
        & go build -trimpath -ldflags "-s -w -X main.version=$version" -o (Join-Path $stage 'decurix-android-arm64') ./cmd/decurix
        if ($LASTEXITCODE -ne 0) { throw 'Android Go build failed.' }
    } finally { Pop-Location }

    # TheBot is a Go module of its own (agent/thebot), built from its folder.
    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'
    Push-Location (Join-Path $repo 'agent/thebot')
    try {
        Write-Host "Building TheBot's engine..."
        & go build -trimpath -ldflags '-s -w' -o (Join-Path $runtime 'soul.exe') ./cmd/soul
        if ($LASTEXITCODE -ne 0) { throw 'TheBot engine build failed.' }
    } finally { Pop-Location }
} finally {
    $env:GOOS = $oldGoos
    $env:GOARCH = $oldGoarch
    $env:CGO_ENABLED = $oldCgo
    $env:GOFLAGS = $oldGoFlags
    $env:GOMAXPROCS = $oldGoMaxProcs
    $env:GOMEMLIMIT = $oldGoMemLimit
}

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

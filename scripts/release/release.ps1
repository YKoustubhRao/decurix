<#
.SYNOPSIS
    Cross-compiles the Decurix control plane into standalone executables and
    packages each one as a downloadable archive.
.DESCRIPTION
    The web templates (web/embed.go) and the desktop image build context
    (embed.go) are compiled into the binary, so each executable is fully
    self-contained: it needs only Docker on the host, and builds its own
    desktop images via `decurix --build-images`. The archives add nothing it
    depends on, just a README and an example config.
.PARAMETER Version
    Version string stamped into the binary. Defaults to `git describe`.
.PARAMETER Output
    Directory to write binaries and archives into. Wiped on each run.
.PARAMETER Targets
    os/arch pairs to build, e.g. windows/amd64. Defaults to all six.
.PARAMETER BinariesOnly
    Build the executables but skip the archives.
.EXAMPLE
    .\scripts\release\release.ps1
.EXAMPLE
    .\scripts\release\release.ps1 -Version v0.2.0 -Targets windows/amd64,linux/amd64
#>

param(
    [string]$Version = "",
    [string]$Output = "dist",
    [string[]]$Targets = @(
        "linux/amd64",
        "linux/arm64",
        "windows/amd64",
        "windows/arm64",
        "darwin/amd64",
        "darwin/arm64"
    ),
    [switch]$BinariesOnly
)

$ErrorActionPreference = "Stop"

# Every script runs from the repo root, two levels up from scripts/<group>/.
Set-Location (Split-Path -Parent (Split-Path -Parent $PSScriptRoot))

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    Write-Host "ERROR: Go is not installed (needed to build the executables)."
    exit 1
}

# Version defaults to the git description, so a binary can always be traced
# back to the commit it came from. Falls back to "dev" outside a checkout.
if ([string]::IsNullOrWhiteSpace($Version)) {
    $Version = (git describe --tags --always --dirty 2>$null)

    if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($Version)) {
        $Version = "dev"
    }

    $Version = $Version.Trim()
}

if ([string]::IsNullOrWhiteSpace($Output)) {
    Write-Host "ERROR: -Output needs a directory."
    exit 1
}

Write-Host "========================================"
Write-Host " Building Decurix $Version"
Write-Host "========================================"
Write-Host ""
Write-Host "Go:      $(go version)"
Write-Host "Output:  $Output/"
Write-Host "Targets: $($Targets -join ' ')"

if (Test-Path $Output) {
    Remove-Item -Recurse -Force $Output
}

$outDir = (New-Item -ItemType Directory -Force -Path $Output).FullName
$stageRoot = (New-Item -ItemType Directory -Force -Path (Join-Path ([System.IO.Path]::GetTempPath()) ("decurix-release-" + [guid]::NewGuid()))).FullName

# Writes the human-facing extras that sit next to the binary in an archive: an
# example config and a README for this OS ($Goos). The binary needs neither --
# it embeds the web templates and the desktop build context -- so there is
# deliberately no docker/, agent/ or scripts/ in a bundle.
function Copy-Bundle {
    param([string]$Dest, [string]$Goos)

    if ($Goos -eq "windows") {
        $run = ".\decurix.exe"
        $copyEnv = "copy .env.example .env"
        $dataDir = "%AppData%\decurix"
    } else {
        $run = "./decurix"
        $copyEnv = "cp .env.example .env"
        $dataDir = "~/.config/decurix"
    }

    if ($Goos -eq "darwin") {
        $dataDir = "~/Library/Application Support/decurix"
    }

    Copy-Item ".env.example" (Join-Path $Dest ".env.example")

    $readme = @"
Decurix $Version
================

This bundle is the Decurix control plane as a single self-contained
executable. It needs no runtime, no repo checkout and no files beside it --
the dashboard and the desktop image build context are compiled in.

Requirements
------------
  * Docker (running, and reachable by the user running Decurix)
  * ~15 GB of disk for the three desktop images

Setup
-----
  1. Build the desktop images (one time, and again whenever you upgrade):

         $run --build-images

     This builds decurix-desktop:basic, :developer and :ai straight from the
     executable. It takes a while the first time. Add --rebuild to rebuild
     images that already exist.

  2. Configure (optional -- every setting has a working default):

         $copyEnv

     Edit .env -- set DEFAULT_PASSWORD (the VNC password for new desktops).
     Decurix reads .env from the working directory, or from the folder the
     executable lives in. Plain environment variables work too. The Claude
     harness needs no .env entry -- log in from the dashboard instead, next
     to the "Claude Code" option when creating a desktop.

     Workers think with a model served by Ollama (https://ollama.com).
     OLLAMA_URL picks the server (default: the Ollama on this machine) and
     OLLAMA_MODEL the model (default: qwen2.5-coder:3b). Pull it first:

         ollama pull qwen2.5-coder:3b
         ollama pull nomic-embed-text   (only the thebot harness needs this)

     On Linux, Ollama must listen on more than 127.0.0.1 for desktop
     containers to reach it: set OLLAMA_HOST=0.0.0.0 on its service.

  3. Run:

         $run

  4. Open http://localhost:8080

Notes
-----
  * Decurix listens on port 8080 and talks to Docker through the local Docker
    socket / named pipe. On Linux the user must be able to reach
    /var/run/docker.sock (usually: be in the "docker" group).
  * Task records, chats, queries, worker memory and action screenshots
    are kept in decurix.db under $dataDir, so they survive a
    restart. Set DECURIX_DATA_DIR to keep them somewhere else.
  * $run --version prints the build this bundle was cut from.
  * Full docs: https://github.com/YKoustubhRao/decurix
"@

    Set-Content -Path (Join-Path $Dest "README.txt") -Value $readme
}

foreach ($target in $Targets) {
    $parts = $target.Split("/")

    if ($parts.Count -ne 2 -or -not $parts[0] -or -not $parts[1]) {
        Write-Host "ERROR: target '$target' is not in os/arch form (e.g. linux/amd64)."
        exit 1
    }

    $goos = $parts[0]
    $goarch = $parts[1]
    $name = "decurix_${Version}_${goos}_${goarch}"
    $ext = if ($goos -eq "windows") { ".exe" } else { "" }
    $binaryPath = Join-Path $outDir "$name$ext"

    Write-Host ""
    Write-Host "=== $goos/$goarch ==="

    # CGO off keeps the binary static, so it runs on any machine of that
    # os/arch without matching system libraries. -s -w drops the debug
    # symbols; -X stamps main.version.
    $env:CGO_ENABLED = "0"
    $env:GOOS = $goos
    $env:GOARCH = $goarch

    go build -trimpath -ldflags "-s -w -X main.version=$Version" -o $binaryPath ./cmd/decurix

    Remove-Item Env:CGO_ENABLED, Env:GOOS, Env:GOARCH

    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

    Write-Host "  binary:  $Output/$name$ext"

    if ($BinariesOnly) { continue }

    $stage = (New-Item -ItemType Directory -Force -Path (Join-Path $stageRoot $name)).FullName

    Copy-Item $binaryPath (Join-Path $stage "decurix$ext")
    Copy-Bundle -Dest $stage -Goos $goos

    # Zip everywhere: it is what Compress-Archive gives us natively, and
    # Windows is the platform most likely to be unpacking these.
    $archive = Join-Path $outDir "$name.zip"
    Compress-Archive -Path $stage -DestinationPath $archive -Force

    Write-Host "  archive: $Output/$name.zip"
}

Write-Host ""
Write-Host "=== Checksums ==="

$sums = Get-ChildItem -Path $outDir -File -Filter "decurix_*" | ForEach-Object {
    $hash = (Get-FileHash -Algorithm SHA256 $_.FullName).Hash.ToLower()
    "$hash  $($_.Name)"
}

Set-Content -Path (Join-Path $outDir "SHA256SUMS") -Value $sums

Remove-Item -Recurse -Force $stageRoot

Write-Host ""
Write-Host "========================================"
Write-Host " Decurix $Version built"
Write-Host "========================================"
Write-Host ""

Get-ChildItem -Path $outDir

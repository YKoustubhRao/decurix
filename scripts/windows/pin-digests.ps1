<#
.SYNOPSIS
    Prints the digest of every image Decurix still takes by tag alone (B-24),
    as the lines to paste. Changes no file.
.DESCRIPTION
    Each Dockerfile FROM, and the images the control plane runs itself (the
    bundled Ollama, the host hop's alpine). A digest is read with
    `docker buildx imagetools inspect` (the multi-platform index, nothing
    pulled), or, without buildx, by pulling the image and reading its
    RepoDigests. A reference already pinned (@sha256:), a local image
    (decurix-*), scratch and a build stage's own name are skipped. Exits 1 if
    any digest could not be read. A pin is moved on purpose, by a person who
    read what it now points at.

    Twin: scripts/linux/pin-digests.sh.
#>

$root = $PSScriptRoot
while ($root -and -not (Test-Path (Join-Path $root "docker/desktop.basic.Dockerfile"))) {
    $root = Split-Path -Parent $root
}
if (-not $root) {
    Write-Host "ERROR: no docker/desktop.basic.Dockerfile found above $PSScriptRoot."
    exit 1
}
Set-Location $root

docker version *>$null
if ($LASTEXITCODE -ne 0) {
    Write-Host "ERROR: docker is not reachable; start Docker first."
    exit 1
}

$digests = @{}
$script:failed = 0

# The digest (sha256:...) of a reference, or "" when it cannot be read; each reference is asked once.
function Resolve-Digest([string]$Ref) {
    if ($digests.ContainsKey($Ref)) { return $digests[$Ref] }
    $d = (docker buildx imagetools inspect --format '{{.Manifest.Digest}}' $Ref 2>$null | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or -not $d.StartsWith("sha256:")) {
        $d = ""
        docker pull -q $Ref *>$null
        if ($LASTEXITCODE -eq 0) {
            $repo = (docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' $Ref 2>$null | Select-Object -First 1)
            if ($repo -and $repo.Contains("@")) { $d = $repo.Substring($repo.IndexOf("@") + 1).Trim() }
            if (-not $d.StartsWith("sha256:")) { $d = "" }
        }
    }
    $digests[$Ref] = $d
    return $d
}

# Prints where, and Text with Old replaced by Ref@digest (Ref is Old unless given; -Quote for a Go string).
function Write-Pin([string]$File, [int]$Line, [string]$Text, [string]$Old, [string]$Ref = "", [switch]$Quote) {
    if (-not $Ref) { $Ref = $Old }
    $d = Resolve-Digest $Ref
    Write-Output "# ${File}:$Line"
    if (-not $d) {
        Write-Output "#   could not read the digest of $Ref"
        $script:failed = 1
        return
    }
    $new = "$Ref@$d"
    if ($Quote) { $new = '"' + $new + '"' }
    $at = $Text.IndexOf($Old)
    Write-Output ($Text.Substring(0, $at) + $new + $Text.Substring($at + $Old.Length))
}

Write-Output ("# Digests read " + (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ") + ". Paste each line over the one named above it.")
Write-Output ""

# Every Dockerfile FROM: the image, its flags (--platform=...) skipped, and not a stage defined above it.
$files = @("docker/Dockerfile") + @(Get-ChildItem -Path docker -Filter *.Dockerfile | ForEach-Object { "docker/" + $_.Name }) +
    @("desktop/electron/stack/Dockerfile") +
    @(Get-ChildItem -Path bench -Directory | ForEach-Object { Get-ChildItem -Path $_.FullName -Filter *.Dockerfile } | ForEach-Object { "bench/" + $_.Directory.Name + "/" + $_.Name })
foreach ($f in $files) {
    if (-not (Test-Path $f)) { continue }
    $stages = @{}
    $n = 0
    foreach ($text in [System.IO.File]::ReadAllLines((Join-Path $root $f))) {
        $n++
        $words = @($text.Trim() -split '\s+' | Where-Object { $_ -ne "" })
        if ($words.Count -lt 2 -or $words[0] -ne "FROM") { continue }
        $rest = @($words | Select-Object -Skip 1 | Where-Object { -not $_.StartsWith("--") })
        if ($rest.Count -lt 1) { continue }
        $ref = $rest[0]
        if ($rest.Count -ge 3 -and $rest[1] -eq "AS") { $stages[$rest[2]] = $true }
        if ($ref.Contains("@sha256:") -or $ref -eq "scratch" -or $ref.StartsWith("decurix-") -or $stages.ContainsKey($ref)) { continue }
        Write-Pin $f $n $text $ref
    }
}

# The images the control plane runs itself.
$n = 0
foreach ($text in [System.IO.File]::ReadAllLines((Join-Path $root "internal/ollama/ollama.go"))) {
    $n++
    if ($text -match 'containerImage\s*=\s*"([^"@]+)"') { Write-Pin "internal/ollama/ollama.go" $n $text $Matches[1] }
}
# The host hop names "alpine" with no tag at all (latest); its pin takes the tag the control plane's own image uses.
$alpine = "alpine:3.24"
foreach ($text in [System.IO.File]::ReadAllLines((Join-Path $root "docker/Dockerfile"))) {
    if ($text -match '^FROM (alpine:[^ @]+)') { $alpine = $Matches[1]; break }
}
$n = 0
foreach ($text in [System.IO.File]::ReadAllLines((Join-Path $root "internal/desktop/hop.go"))) {
    $n++
    if ($text.Contains('"--pid=host", "alpine"')) { Write-Pin "internal/desktop/hop.go" $n $text '"alpine"' $alpine -Quote }
}

exit $script:failed

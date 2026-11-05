# Checks the .env handling of scripts/windows/lib/Ollama-Setup.ps1: values are
# read as compose reads them and as UTF-8 (Windows PowerShell 5.1's
# Get-Content would read them in the ANSI code page), written without a BOM
# or a duplicate line, and a fallback is used for one run only - it never
# reaches .env. Runs in Windows PowerShell 5.1 and in pwsh:
#
#   powershell -NoProfile -ExecutionPolicy Bypass -File scripts/testing/ollama-env.test.ps1

$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot '../windows/lib/Ollama-Setup.ps1')

$failures = 0
function Check([string]$Name, $Got, $Want) {
    if ("$Got" -ceq "$Want") { Write-Host "ok   $Name" }
    else { Write-Host "FAIL ${Name}: got [$Got], want [$Want]"; $script:failures++ }
}

$dir = Join-Path ([System.IO.Path]::GetTempPath()) ('decurix-env-test-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $dir | Out-Null
try {
    $script:EnvFile = Join-Path $dir '.env'
    $utf8 = New-Object System.Text.UTF8Encoding $false
    # A non-ASCII value, written from code points so this file stays ASCII.
    $name = 'Zo' + [char]0x00EB + ' M' + [char]0x00FC + 'ller'
    [System.IO.File]::WriteAllText($script:EnvFile, "NAME=$name`r`nOLLAMA_URL=`"http://mine:11434`"`r`nOLLAMA_MODEL=qwen3:4b # my pick`r`nQ='quoted'`r`n", $utf8)
    $before = [System.IO.File]::ReadAllText($script:EnvFile, $utf8)

    Check 'quoted value, CRLF' (Read-EnvVar 'OLLAMA_URL') 'http://mine:11434'
    Check 'trailing comment' (Read-EnvVar 'OLLAMA_MODEL') 'qwen3:4b'
    Check 'single quotes' (Read-EnvVar 'Q') 'quoted'
    Check 'non-ASCII read as UTF-8' (Read-EnvVar 'NAME') $name

    Set-RunOnly 'OLLAMA_MODEL' 'qwen2.5-coder:3b' | Out-Null
    Set-RunOnly 'BAD' 'a"b' | Out-Null
    Write-RunOverride
    Check 'a fallback leaves .env as it was' ([System.IO.File]::ReadAllText($script:EnvFile, $utf8)) $before
    Check 'the run uses the fallback' (Get-EffectiveEnvVar 'OLLAMA_MODEL') 'qwen2.5-coder:3b'
    Check 'compose gets the override file' $script:ComposeFiles.Count 4
    $override = Get-Content -LiteralPath $script:ComposeFiles[3] -Raw
    Check 'the override names the run-only value' ($override -match 'OLLAMA_MODEL: "qwen2\.5-coder:3b"') $true
    Check 'a value with a quote is not used' ($override -match 'BAD') $false
    Remove-Item -LiteralPath $script:ComposeFiles[3]

    Set-EnvVar 'OLLAMA_URL' 'http://other:11434'
    Set-EnvVar 'OLLAMA_URL' 'http://other:11434'
    $bytes = [System.IO.File]::ReadAllBytes($script:EnvFile)
    $after = [System.IO.File]::ReadAllText($script:EnvFile, $utf8)
    Check 'no BOM written' ($bytes[0] -eq 0xEF) $false
    Check 'one line per name' ([regex]::Matches($after, '(?m)^OLLAMA_URL=').Count) 1
    Check 'non-ASCII survives a write' (Read-EnvVar 'NAME') $name
} finally {
    Remove-Item -LiteralPath $dir -Recurse -Force
}

if ($failures -gt 0) { Write-Host "$failures check(s) failed"; exit 1 }
Write-Host 'all checks passed'

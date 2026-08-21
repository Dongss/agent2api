<#
.SYNOPSIS
    Install agent2api on Windows.

.DESCRIPTION
    irm https://raw.githubusercontent.com/Dongss/agent2api/main/scripts/install.ps1 | iex

    Installs into %LOCALAPPDATA%\Programs\agent2api, which needs no administrator
    rights, and adds that directory to the user PATH if it is not there already.

    Environment:
      AGENT2API_VERSION      install this tag instead of the latest (e.g. v0.1.0)
      AGENT2API_INSTALL_DIR  install here instead of the default

.NOTES
    Windows support is experimental: agent2api cannot yet reclaim the whole
    process tree of an agent CLI it starts there, so a run that times out can
    leave a child process behind. See the README.
#>

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$Repo = 'Dongss/agent2api'
$Bin = 'agent2api'

# Windows PowerShell 5.1 still defaults to TLS 1.0, which github.com refuses.
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

function Fail($message) {
    # throw rather than exit: this script is meant to be run through `iex`, where
    # exit takes the whole session with it.
    throw "install.ps1: $message"
}

# --- what to download ------------------------------------------------------

$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { 'amd64' }
    'ARM64' { 'arm64' }
    default { Fail "unsupported architecture: $($env:PROCESSOR_ARCHITECTURE) (releases cover amd64 and arm64)" }
}

$version = $env:AGENT2API_VERSION
if (-not $version) {
    # Follow the /releases/latest redirect and read the tag off the final URL:
    # the API would rate-limit an unauthenticated install script.
    try {
        $resp = Invoke-WebRequest -Uri "https://github.com/$Repo/releases/latest" -UseBasicParsing
    } catch {
        Fail "cannot find the latest release of $Repo — the network may be down, or the project may have none published yet; set AGENT2API_VERSION to install a specific tag ($($_.Exception.Message))"
    }
    # PowerShell 7 and 5.1 expose the resolved URL under different properties.
    $final = $null
    if ($resp.BaseResponse.PSObject.Properties['RequestMessage']) {
        $final = $resp.BaseResponse.RequestMessage.RequestUri.AbsoluteUri
    } elseif ($resp.BaseResponse.PSObject.Properties['ResponseUri']) {
        $final = $resp.BaseResponse.ResponseUri.AbsoluteUri
    }
    if (-not $final -or $final -notmatch '/tag/(?<tag>[^/]+)$') {
        Fail "cannot tell the latest version from '$final'; set AGENT2API_VERSION to pick one"
    }
    $version = $Matches['tag']
}

$installDir = $env:AGENT2API_INSTALL_DIR
if (-not $installDir) {
    $installDir = Join-Path $env:LOCALAPPDATA "Programs\$Bin"
}

$archive = "${Bin}_${version}_windows_${arch}.zip"
$base = "https://github.com/$Repo/releases/download/$version"

# --- download and verify ---------------------------------------------------

$tmp = Join-Path ([IO.Path]::GetTempPath()) ("agent2api-install-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp -Force | Out-Null
try {
    Write-Host "installing $Bin $version (windows/$arch)"

    $archivePath = Join-Path $tmp $archive
    try {
        Invoke-WebRequest -Uri "$base/$archive" -OutFile $archivePath -UseBasicParsing
    } catch {
        Fail "cannot download $base/$archive — is $version a released version for windows/$arch?"
    }

    # Checksums guard against a truncated download and against a single asset
    # having been swapped.
    $sumsPath = Join-Path $tmp 'checksums.txt'
    try {
        Invoke-WebRequest -Uri "$base/checksums.txt" -OutFile $sumsPath -UseBasicParsing
    } catch {
        $sumsPath = $null
        Write-Warning "no checksums.txt in $version; skipping verification"
    }
    if ($sumsPath) {
        $expected = $null
        foreach ($line in Get-Content $sumsPath) {
            $fields = $line -split '\s+', 2
            if ($fields.Count -eq 2 -and $fields[1].TrimStart('*') -eq $archive) {
                $expected = $fields[0]
                break
            }
        }
        if (-not $expected) {
            Write-Warning "checksums.txt does not list $archive; skipping verification"
        } else {
            $actual = (Get-FileHash -Path $archivePath -Algorithm SHA256).Hash
            if ($actual -ne $expected.ToUpperInvariant() -and $actual -ne $expected) {
                Fail "checksum mismatch for ${archive}: expected $expected, got $actual"
            }
        }
    }

    # --- install -----------------------------------------------------------

    Expand-Archive -Path $archivePath -DestinationPath (Join-Path $tmp 'unpacked') -Force
    $exe = Join-Path $tmp "unpacked\$Bin.exe"
    if (-not (Test-Path $exe)) {
        Fail "$archive did not contain $Bin.exe"
    }

    New-Item -ItemType Directory -Path $installDir -Force | Out-Null
    Copy-Item -Path $exe -Destination (Join-Path $installDir "$Bin.exe") -Force

    Write-Host "installed $(Join-Path $installDir "$Bin.exe")"
    & (Join-Path $installDir "$Bin.exe") version

    # Only the user PATH is touched, and only if the directory is missing from it.
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    $entries = @()
    if ($userPath) { $entries = $userPath -split ';' }
    if ($entries -notcontains $installDir) {
        $updated = if ($userPath) { "$userPath;$installDir" } else { $installDir }
        [Environment]::SetEnvironmentVariable('Path', $updated, 'User')
        Write-Host ""
        Write-Host "added $installDir to your user PATH — open a new terminal for it to take effect"
    }

    Write-Host ""
    Write-Host "next: $Bin doctor    # which agent CLIs this machine can serve"
} finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

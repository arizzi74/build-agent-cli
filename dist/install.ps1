$ErrorActionPreference = 'Stop'

$BaseUrl = if ($env:BACLI_BASE_URL) { $env:BACLI_BASE_URL.TrimEnd('/') } else { 'https://nowdemo.it/bacli' }
$InstallDir = if ($env:BACLI_INSTALL_DIR) { $env:BACLI_INSTALL_DIR } else { Join-Path $HOME '.local\bin' }

if (-not [Environment]::Is64BitOperatingSystem) {
    throw 'bacli requires 64-bit Windows'
}
$arch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
switch ($arch) {
    'X64' { $target = 'windows-amd64' }
    'Arm64' { $target = 'windows-arm64' }
    default { throw "Unsupported Windows architecture: $arch" }
}

$manifestUri = "$BaseUrl/version.json"
$manifest = Invoke-RestMethod -Uri $manifestUri -UseBasicParsing
if ($manifest.schemaVersion -ne 1) {
    throw 'Unsupported bacli release manifest'
}
$entry = $manifest.files.$target
if (-not $entry -or -not $entry.url -or -not $entry.sha256) {
    throw "Release manifest has no artifact for $target"
}
$artifactUri = [Uri]::new([Uri]$manifestUri, [string]$entry.url).AbsoluteUri

New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
$tmp = Join-Path ([IO.Path]::GetTempPath()) ("bacli-{0}.exe" -f [Guid]::NewGuid().ToString('N'))
try {
    Invoke-WebRequest -Uri $artifactUri -OutFile $tmp -UseBasicParsing
    $actual = (Get-FileHash -Path $tmp -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actual -ne ([string]$entry.sha256).ToLowerInvariant()) {
        throw 'Checksum verification failed'
    }
    $destination = Join-Path $InstallDir 'bacli.exe'
    Move-Item -Path $tmp -Destination $destination -Force
} finally {
    Remove-Item -Path $tmp -Force -ErrorAction SilentlyContinue
}

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
$normalized = @($userPath -split ';' | Where-Object { $_ })
if (-not ($normalized | Where-Object { $_.TrimEnd('\') -ieq $InstallDir.TrimEnd('\') })) {
    $newPath = if ([string]::IsNullOrWhiteSpace($userPath)) { $InstallDir } else { "$userPath;$InstallDir" }
    [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
    $env:Path = "$env:Path;$InstallDir"
    Write-Host "Added $InstallDir to the user PATH. Open a new terminal before running bacli."
}

Write-Host "Installed bacli $($manifest.version) to $(Join-Path $InstallDir 'bacli.exe')"

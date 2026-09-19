$ErrorActionPreference = 'Stop'

$baseUrlOverride = [string]$env:BACLI_BASE_URL
$BaseUrl = if ([string]::IsNullOrWhiteSpace($baseUrlOverride)) { 'https://github.com/arizzi74/build-agent-cli/releases/latest/download' } else { $baseUrlOverride.TrimEnd('/') }
$installDirOverride = [string]$env:BACLI_INSTALL_DIR
$userHome = [string]$HOME
if ([string]::IsNullOrWhiteSpace($userHome)) {
    $userHome = [Environment]::GetFolderPath([Environment+SpecialFolder]::UserProfile)
}
if ([string]::IsNullOrWhiteSpace($userHome)) {
    throw 'Could not determine the current Windows user profile directory'
}
$InstallDir = if ([string]::IsNullOrWhiteSpace($installDirOverride)) { Join-Path $userHome '.local\bin' } else { $installDirOverride }

if (-not [Environment]::Is64BitOperatingSystem) {
    throw 'bacli requires 64-bit Windows'
}
# RuntimeInformation.OSArchitecture can be null in Windows PowerShell 5.1 on
# older .NET Framework installations. These variables expose the native OS
# architecture even when the current PowerShell process is running under WOW64.
$arch = [string]$env:PROCESSOR_ARCHITEW6432
if ([string]::IsNullOrWhiteSpace($arch)) {
    $arch = [string]$env:PROCESSOR_ARCHITECTURE
}
$arch = $arch.ToUpperInvariant()
switch ($arch) {
    'AMD64' { $target = 'windows-amd64' }
    'ARM64' { $target = 'windows-arm64' }
    default { throw "Unsupported Windows architecture: $arch" }
}

$manifestUri = "$BaseUrl/version.json"
$manifest = Invoke-RestMethod -Uri $manifestUri -UseBasicParsing
if ($manifest.schemaVersion -ne 1) {
    throw 'Unsupported bacli release manifest'
}
$entryProperty = if ($null -ne $manifest.files) { $manifest.files.PSObject.Properties[$target] } else { $null }
$entry = if ($entryProperty) { $entryProperty.Value } else { $null }
if (-not $entry -or -not ($entry.url -is [string]) -or [string]::IsNullOrWhiteSpace($entry.url)) {
    throw "Release manifest has no artifact for $target"
}
if (-not ($entry.sha256 -is [string]) -or $entry.sha256 -notmatch '^[0-9a-fA-F]{64}$') {
    throw "Release manifest has an invalid checksum for $target"
}
try {
    $artifact = [Uri]::new([Uri]$manifestUri, [string]$entry.url)
} catch {
    throw "Release manifest has an invalid artifact URL for $target"
}
if ($artifact.Scheme -notin @('https', 'http') -or [string]::IsNullOrWhiteSpace($artifact.Host)) {
    throw "Release manifest has an invalid artifact URL for $target"
}
$artifactUri = $artifact.AbsoluteUri

New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
$tmp = Join-Path ([IO.Path]::GetTempPath()) ("bacli-{0}.exe" -f [Guid]::NewGuid().ToString('N'))
try {
    Invoke-WebRequest -Uri $artifactUri -OutFile $tmp -UseBasicParsing
    $downloadHash = Get-FileHash -Path $tmp -Algorithm SHA256
    $actual = [string]$downloadHash.Hash
    $expected = [string]$entry.sha256
    if ([string]::IsNullOrWhiteSpace($actual) -or [string]::IsNullOrWhiteSpace($expected) -or $actual.ToLowerInvariant() -ne $expected.ToLowerInvariant()) {
        throw 'Checksum verification failed'
    }
    $destination = Join-Path $InstallDir 'bacli.exe'
    Move-Item -Path $tmp -Destination $destination -Force
} finally {
    Remove-Item -Path $tmp -Force -ErrorAction SilentlyContinue
}

$userPath = [string][Environment]::GetEnvironmentVariable('Path', 'User')
$normalized = @($userPath -split ';' | Where-Object { -not [string]::IsNullOrWhiteSpace([string]$_) })
$normalizedInstallDir = ([string]$InstallDir).TrimEnd('\')
if (-not ($normalized | Where-Object { ([string]$_).Trim().TrimEnd('\') -ieq $normalizedInstallDir })) {
    $newPath = if ([string]::IsNullOrWhiteSpace($userPath)) { $InstallDir } else { "$userPath;$InstallDir" }
    [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
    $processPath = [string]$env:Path
    $env:Path = if ([string]::IsNullOrWhiteSpace($processPath)) { $InstallDir } else { "$processPath;$InstallDir" }
    Write-Host "Added $InstallDir to the user PATH. Open a new terminal before running bacli."
}

Write-Host "Installed bacli $($manifest.version) to $(Join-Path $InstallDir 'bacli.exe')"

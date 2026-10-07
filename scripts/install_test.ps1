# Run offline under Windows PowerShell 5.1 and PowerShell 7 on Windows.
param([Parameter(Mandatory = $true)] [string] $FakeHelper)
$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
$script = Get-Content -Raw -LiteralPath (Join-Path $repo 'install.ps1')
$testdir = Join-Path ([System.IO.Path]::GetTempPath()) ("pipkin-test-" + [guid]::NewGuid())
$assets = Join-Path $testdir 'assets'
New-Item -ItemType Directory -Path $assets | Out-Null
$env:TEST_INSTALLED = Join-Path $testdir 'installed'
Remove-Item Env:PIPKIN_VERSION, Env:PIPKIN_REPOSITORY -ErrorAction SilentlyContinue

$native = (Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager\Environment').PROCESSOR_ARCHITECTURE
$arch = @{ ARM64 = 'arm64'; AMD64 = 'amd64' }[$native]
$sums = foreach ($name in @('pipkin', 'pipkinw')) {
    $asset = "$name-windows-$arch.exe"
    Copy-Item -LiteralPath $FakeHelper -Destination (Join-Path $assets $asset)
    '{0}  {1}' -f (Get-FileHash -Algorithm SHA256 (Join-Path $assets $asset)).Hash.ToLower(), $asset
}
[System.IO.File]::WriteAllText((Join-Path $assets 'SHA256SUMS'), (($sums -join "`n") + "`n"))

# Match Invoke-WebRequest: binary Content unless -OutFile is supplied.
$script:noRelease = $false
function Invoke-WebRequest {
    param([switch] $UseBasicParsing, [string] $Method, [string] $OutFile, [Parameter(Position = 0)] [string] $Uri)
    if ($Method -eq 'Head') {
        if ($script:noRelease) { throw [System.Net.WebException]::new('The remote server returned an error: (404) Not Found.') }
        $tag = [uri] 'https://github.com/damian-w/pipkin-cli/releases/tag/v1.2.3'
        return [pscustomobject] @{ BaseResponse = [pscustomobject] @{ ResponseUri = $tag } }
    }
    $prefix = 'https://github.com/damian-w/pipkin-cli/releases/download/v1.2.3/'
    if (-not $Uri.StartsWith($prefix)) { throw "unexpected download: $Uri" }
    $file = Join-Path $assets $Uri.Substring($prefix.Length)
    if ($OutFile) { Copy-Item -LiteralPath $file -Destination $OutFile; return }
    return [pscustomobject] @{ Content = [System.IO.File]::ReadAllBytes($file) }
}

function Check($condition, $message) {
    if (-not $condition) { throw "Installer check failed: $message" }
}

try {
    $protocols = [Net.ServicePointManager]::SecurityProtocol
    $ErrorActionPreference = 'Continue'
    Invoke-Expression $script
    Check ($ErrorActionPreference -eq 'Continue') 'the installer changed the caller''s $ErrorActionPreference'
    $ErrorActionPreference = 'Stop'
    Check ((Get-Content -Raw $env:TEST_INSTALLED) -eq 'install') 'the helper was not run with "install"'
    Check (-not (Test-Path variable:release)) 'the installer left variables in the caller''s scope'
    Check ([Net.ServicePointManager]::SecurityProtocol -eq $protocols) 'the installer changed the process''s TLS settings'

    Remove-Item $env:TEST_INSTALLED
    [System.IO.File]::WriteAllText((Join-Path $assets 'SHA256SUMS'), "corrupt`n")
    $failed = $false
    try { Invoke-Expression $script } catch { $failed = $true }
    Check $failed 'the installer accepted an invalid checksum'
    Check (-not (Test-Path $env:TEST_INSTALLED)) 'the helper ran despite an invalid checksum'

    $script:noRelease = $true
    $message = ''
    try { Invoke-Expression $script } catch { $message = $_.Exception.Message }
    Check ($message -like 'Pipkin: could not find a published release*') "unexpected failure message: $message"
    Write-Output 'Windows installer checks passed.'
} finally {
    Remove-Item -Recurse -Force $testdir -ErrorAction SilentlyContinue
}

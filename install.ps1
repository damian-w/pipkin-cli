# Installs the Pipkin helper on Windows:
#   irm https://pipkin.io/install.ps1 | iex
& {
    $ErrorActionPreference = 'Stop'
    # Windows PowerShell progress output slows downloads.
    $ProgressPreference = 'SilentlyContinue'

    function Fail($message) {
        throw "Pipkin: $message"
    }

    # Enable TLS 1.2 for Windows PowerShell 5.1, then restore the process setting.
    $protocols = [Net.ServicePointManager]::SecurityProtocol
    [Net.ServicePointManager]::SecurityProtocol = $protocols -bor [Net.SecurityProtocolType]::Tls12
    try {
        $repository = if ($env:PIPKIN_REPOSITORY) { $env:PIPKIN_REPOSITORY } else { 'damian-w/pipkin-cli' }
        $base = "https://github.com/$repository"
        $tag = $env:PIPKIN_VERSION
        if (-not $tag) {
            try {
                $response = Invoke-WebRequest -UseBasicParsing -Method Head "$base/releases/latest"
            } catch {
                Fail "could not find a published release ($($_.Exception.Message))"
            }
            $uri = $response.BaseResponse.ResponseUri
            if (-not $uri) { $uri = $response.BaseResponse.RequestMessage.RequestUri }
            $prefix = "$base/releases/tag/"
            if (-not $uri -or -not $uri.AbsoluteUri.StartsWith($prefix)) { Fail 'no published release found.' }
            $tag = $uri.AbsoluteUri.Substring($prefix.Length)
        }
        if ($tag -notmatch '^[a-zA-Z0-9._-]+$') { Fail 'invalid release version.' }
        $release = "$base/releases/download/$tag"
        $nativeArch = (Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager\Environment').PROCESSOR_ARCHITECTURE
        switch ($nativeArch) {
            'ARM64' { $arch = 'arm64' }
            'AMD64' { $arch = 'amd64' }
            default { Fail "unsupported processor: $nativeArch." }
        }

        $workdir = Join-Path ([System.IO.Path]::GetTempPath()) ("pipkin-" + [guid]::NewGuid())
        New-Item -ItemType Directory -Path $workdir | Out-Null
        try {
            # Release assets use application/octet-stream; read the manifest as text.
            $sumsFile = Join-Path $workdir 'SHA256SUMS'
            Invoke-WebRequest -UseBasicParsing "$release/SHA256SUMS" -OutFile $sumsFile
            $sums = @{}
            foreach ($line in Get-Content -LiteralPath $sumsFile) {
                $fields = $line.Trim() -split '\s+'
                if ($fields.Count -eq 2) { $sums[$fields[1].TrimStart('*')] = $fields[0].ToLower() }
            }
            foreach ($name in @('pipkin', 'pipkinw')) {
                $asset = "$name-windows-$arch.exe"
                $target = Join-Path $workdir "$name.exe"
                Invoke-WebRequest -UseBasicParsing "$release/$asset" -OutFile $target
                $actual = (Get-FileHash -Algorithm SHA256 $target).Hash.ToLower()
                if (-not $sums.ContainsKey($asset) -or $sums[$asset] -ne $actual) {
                    Fail 'the downloaded helper failed its checksum; nothing was installed.'
                }
            }
            & (Join-Path $workdir 'pipkin.exe') install
            if ($LASTEXITCODE -ne 0) { Fail "the helper could not be installed (exit $LASTEXITCODE)." }
        } finally {
            Remove-Item -Recurse -Force $workdir -ErrorAction SilentlyContinue
        }
    } finally {
        [Net.ServicePointManager]::SecurityProtocol = $protocols
    }
}

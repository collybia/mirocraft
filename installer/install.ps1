<#
.SYNOPSIS
    Downloads and runs the Mirocraft installer.

.DESCRIPTION
    Run in an elevated PowerShell:

        irm https://raw.githubusercontent.com/collybia/mirocraft/master/installer/install.ps1 | iex

    With options, which a plain pipe into iex cannot pass:

        & ([scriptblock]::Create((irm https://raw.githubusercontent.com/collybia/mirocraft/master/installer/install.ps1))) -Mode 3 -AssumeYes

    This file is deliberately plain ASCII, and it exists because of a real
    conflict between the two ways people run an installer.

    The installer proper speaks Russian, and Windows PowerShell 5.1 reads a
    .ps1 file in the system ANSI codepage unless the file carries a UTF-8 BOM
    - without one, its Cyrillic string literals are misdecoded badly enough
    that the script fails to parse at all. So the file needs a BOM.

    But `irm | iex` hands PowerShell a *string*, and a string that starts with
    U+FEFF does not parse either: the leading character is not whitespace to
    the tokenizer, so the opening `<#` is never recognised and every line of
    the comment header is read as code. The command in the README produced a
    screenful of parse errors on the first Windows Server it ever ran on.

    One file cannot satisfy both. So this one carries no BOM and no non-ASCII,
    which makes it safe to pipe into iex, and it writes the real installer to
    a temporary file *with* a BOM, which is what running a file needs.

.PARAMETER Args
    Everything is forwarded to installer/panel.ps1 untouched, so the options
    live in one place rather than being restated here and drifting.
#>

$ErrorActionPreference = 'Stop'

$source = 'https://raw.githubusercontent.com/collybia/mirocraft/master/installer/panel.ps1'
if ($env:MIROCRAFT_INSTALLER_URL) { $source = $env:MIROCRAFT_INSTALLER_URL }

# TLS 1.2 for Windows Server 2016 and older, where the default is still SSL3
# and the download fails with a handshake error that says nothing useful.
try {
    [Net.ServicePointManager]::SecurityProtocol =
        [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
} catch {}

# $env:TEMP can hold an 8.3 short name, and Remove-Item refuses to take one -
# the cleanup then fails and leaves the file behind. Resolved to the long form
# once, here.
$tempRoot = try { (Get-Item $env:TEMP).FullName } catch { $env:TEMP }
$temp = Join-Path $tempRoot ('mirocraft-install-' + [guid]::NewGuid().ToString('N') + '.ps1')

try {
    # A local path is accepted as well as a URL, so this path can be tested
    # without the network and pointed at a private mirror without patching it.
    if ($source -match '^https?://') {
        Write-Host "-> Downloading the installer"
        $body = (Invoke-WebRequest -Uri $source -UseBasicParsing).Content
        if ($body -is [byte[]]) { $body = [Text.Encoding]::UTF8.GetString($body) }
    }
    else {
        Write-Host "-> Reading the installer from $source"
        $body = [IO.File]::ReadAllText($source, [Text.Encoding]::UTF8)
    }

    # A BOM, on purpose: see above. This is the copy PowerShell will parse.
    [IO.File]::WriteAllText($temp, $body, (New-Object Text.UTF8Encoding $true))

    & $temp @args
    exit $LASTEXITCODE
}
finally {
    Remove-Item $temp -Force -ErrorAction SilentlyContinue
}

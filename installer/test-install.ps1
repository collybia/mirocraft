<#
.SYNOPSIS
    Проверяет install.ps1, не трогая настоящую установку.

.DESCRIPTION
    Ставит панель во временный каталог, под другим именем службы и на другой
    порт, проверяет результат, ставит поверх ещё раз и убирает за собой.

    Установщик нельзя проверить юнит-тестом: он создаёт службу, правило
    фаервола и права на файлы, а заглушка вместо всего этого проверяла бы
    заглушку. Поэтому он проверяется единственным осмысленным способом —
    запуском, с осмотром того, что действительно получилось.

    Запускать от администратора:
        powershell -ExecutionPolicy Bypass -File installer\test-install.ps1
#>

[CmdletBinding()]
param(
    [string]$Binary
)

$ErrorActionPreference = 'Stop'

$RepoRoot = Split-Path -Parent $PSScriptRoot

# $env:TEMP can hold an 8.3 short name, and Remove-Item refuses to take
# one. Resolved to the long form once, here.
$TempRoot = try { (Get-Item $env:TEMP).FullName } catch { $env:TEMP }
$Scratch  = Join-Path $TempRoot 'mirocraft-install-test'
$ServiceName = 'MirocraftInstallTest'
$Port        = 8098

$script:Failures = 0

function Test-Check {
    param([string]$Name, [bool]$Ok, [string]$Detail = '')

    if ($Ok) {
        Write-Host "ok   $Name"
    }
    else {
        $suffix = if ($Detail) { " - $Detail" } else { '' }
        Write-Host "FAIL $Name$suffix" -ForegroundColor Red
        $script:Failures++
    }
}

function Remove-Everything {
    & sc.exe delete $ServiceName 2>&1 | Out-Null
    Get-NetFirewallRule -DisplayName "$ServiceName panel" -ErrorAction SilentlyContinue |
        Remove-NetFirewallRule -ErrorAction SilentlyContinue
    Remove-Item -Recurse -Force $Scratch -ErrorAction SilentlyContinue
}

function Invoke-Installer {
    param([string[]]$ExtraArgs = @(), [switch]$WithoutBinary)

    $arguments = @(
        '-NoProfile', '-ExecutionPolicy', 'Bypass',
        '-File', (Join-Path $PSScriptRoot 'install.ps1'),
        '-AssumeYes'
    )
    if (-not $WithoutBinary) {
        $arguments += @('-Binary', $Binary)
    }
    $arguments += @(
        '-InstallDir', (Join-Path $Scratch 'bin'),
        '-ConfigDir', (Join-Path $Scratch 'config'),
        '-ServiceName', $ServiceName,
        '-Port', $Port
    ) + $ExtraArgs

    $output = & powershell @arguments 2>&1
    return ($output | Out-String)
}

# Панель отвечает по HTTPS с самоподписанным сертификатом, а Windows
# PowerShell 5.1 по умолчанию не умеет ни игнорировать его, ни TLS 1.2.
function Get-PanelHealth {
    try {
        Add-Type -TypeDefinition @'
using System.Net;
using System.Security.Cryptography.X509Certificates;
public class MirocraftTestCerts : ICertificatePolicy {
    public bool CheckValidationResult(ServicePoint s, X509Certificate c, WebRequest r, int p) { return true; }
}
'@ -ErrorAction SilentlyContinue
        [Net.ServicePointManager]::CertificatePolicy = New-Object MirocraftTestCerts
        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

        return (Invoke-WebRequest -Uri "https://127.0.0.1:$Port/api/v1/health" `
            -UseBasicParsing -TimeoutSec 15).Content
    }
    catch {
        return ''
    }
}

# Путь, которым идёт настоящий оператор: локального файла нет, установщик
# скачивает релиз и сверяет его. Релиз раскладывается на диске и отдаётся по
# file://, чтобы тест не зависел от опубликованного релиза — проверяется своя
# загрузка и своя сверка суммы, а не доступность GitHub.
function Test-DownloadPath {
    $release = Join-Path $Scratch 'release'
    New-Item -ItemType Directory -Force -Path $release | Out-Null

    $asset = 'mirocraft-windows-amd64.exe'
    $target = Join-Path $release $asset
    Copy-Item $Binary $target -Force

    $hash = (Get-FileHash -Path $target -Algorithm SHA256).Hash.ToLowerInvariant()
    Set-Content -Path (Join-Path $release 'SHA256SUMS') -Value "$hash  $asset" -Encoding ascii

    $base = 'file:///' + ($release -replace '\\', '/')

    $output = Invoke-Installer -ExtraArgs @('-BaseUrl', $base) -WithoutBinary
    Test-Check 'установщик ставит из релиза' ($output -match 'Контрольная сумма сошлась') $output

    # И вторая половина: файл, который не сошёлся, ставиться не должен. Сверка,
    # которую видели только успешной, неотличима от её отсутствия.
    Add-Content -Path $target -Value 'tampered' -Encoding ascii
    $bad = Invoke-Installer -ExtraArgs @('-BaseUrl', $base) -WithoutBinary
    Test-Check 'подменённый файл отвергнут' ($bad -match 'Контрольная сумма не совпала') $bad

    # И отказ ничего не сломал: работает то, что было установлено до него.
    $intact = ((Get-Service $ServiceName -ErrorAction SilentlyContinue).Status -eq 'Running') -and
              ((Get-PanelHealth) -match '"status":"ok"')
    Test-Check 'отказ не тронул рабочую установку' $intact
}

function Main {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = New-Object Security.Principal.WindowsPrincipal($identity)
    if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        Write-Host 'Запустите от администратора: тест создаёт службу и правило фаервола.' -ForegroundColor Red
        exit 2
    }

    if (-not $Binary) {
        $Binary = Join-Path $Scratch 'mirocraft.exe'
        Write-Host '-> Собираю бинарник'
        New-Item -ItemType Directory -Force -Path $Scratch | Out-Null
        Push-Location $RepoRoot
        try { & go build -o $Binary ./cmd/mirocraft }
        finally { Pop-Location }
        if ($LASTEXITCODE -ne 0) { Write-Host 'Сборка не удалась' -ForegroundColor Red; exit 2 }
    }

    Write-Host '-> Убираю следы прошлых прогонов'
    $keepBinary = Test-Path $Binary
    if ($keepBinary) {
        $saved = Join-Path $TempRoot 'mirocraft-install-test.exe'
        Copy-Item $Binary $saved -Force
        $Binary = $saved
    }
    Remove-Everything

    try {
        Write-Host ''
        $output = Invoke-Installer -ExtraArgs @('-Mode', '3')
        Test-Check 'установщик отработал' ($output -match 'Служба запущена') $output

        $service = Get-Service $ServiceName -ErrorAction SilentlyContinue
        Test-Check 'служба создана и запущена' ($null -ne $service -and $service.Status -eq 'Running')
        Test-Check 'служба стартует сама' ($null -ne $service -and $service.StartType -eq 'Automatic')

        Test-Check 'правило фаервола создано' `
            ($null -ne (Get-NetFirewallRule -DisplayName "$ServiceName panel" -ErrorAction SilentlyContinue))

        # В конфигурации лежит токен провайдера DNS, поэтому наследование прав
        # снимается: по умолчанию ProgramData читается всеми.
        $configPath = Join-Path $Scratch 'config\mirocraft.yaml'
        $acl = Get-Acl $configPath
        Test-Check 'права на конфигурацию не наследуются' $acl.AreAccessRulesProtected
        Test-Check 'конфигурацию читают только система и администраторы' `
            ($acl.Access.Count -le 2) "правил: $($acl.Access.Count)"

        Test-Check 'панель отвечает по HTTPS' ((Get-PanelHealth) -match '"status":"ok"')
        Test-Check 'напечатан адрес со схемой https' ($output -match 'Панель:  https://')
        Test-Check 'про самоподписанный сертификат сказано' ($output -match 'самоподписанный')

        $adminFile = Join-Path $Scratch 'config\data\initial-admin.txt'
        Test-Check 'пароль администратора сгенерирован' (Test-Path $adminFile)

        # Напечатан, а не спрятан в файл: между «установлено» и «вошёл» не
        # должно быть шага «найди и открой».
        $printedPassword = if (Test-Path $adminFile) {
            $stored = (Get-Content $adminFile -Raw)
            if ($stored -match '(?m)^password:\s*(.+)$') { $Matches[1].Trim() } else { '' }
        } else { '' }
        Test-Check 'логин напечатан в вывод установщика' ($output -match 'Логин:\s+admin')
        Test-Check 'пароль напечатан в вывод установщика' `
            ($printedPassword -and $output -match [regex]::Escape($printedPassword))

        # Остановка должна проходить штатно: служба, которая не отвечает
        # диспетчеру, убивается вместе со всеми запущенными мирами.
        $stopped = $false
        try {
            Stop-Service $ServiceName -Force -ErrorAction Stop
            (Get-Service $ServiceName).WaitForStatus('Stopped', [TimeSpan]::FromSeconds(30))
            $stopped = (Get-Service $ServiceName).Status -eq 'Stopped'
        }
        catch { $stopped = $false }
        Test-Check 'служба останавливается штатно' $stopped

        # --- и ещё раз, это и есть путь обновления ---

        $marker = Join-Path $Scratch 'config\EDITED-BY-OPERATOR'
        Set-Content -Path $marker -Value 'x' -Encoding utf8

        $upgrade = Invoke-Installer
        Test-Check 'повторная установка проходит' ($upgrade -match 'Обновлено')
        Test-Check 'конфигурация не перезаписана' ($upgrade -match 'не трогаю')
        Test-Check 'файлы оператора на месте' (Test-Path $marker)
        Test-Check 'база на месте' (Test-Path (Join-Path $Scratch 'config\data\mirocraft.db'))
        Test-Check 'служба работает после обновления' `
            ((Get-Service $ServiceName -ErrorAction SilentlyContinue).Status -eq 'Running')
        Test-Check 'панель отвечает после обновления' ((Get-PanelHealth) -match '"status":"ok"')

        # --- загрузка релиза ---

        Test-DownloadPath

        # --- удаление ---

        $removal = Invoke-Installer -ExtraArgs @('-Uninstall')
        Test-Check 'удаление проходит' ($removal -match 'удалены')
        Test-Check 'служба удалена' `
            ($null -eq (Get-Service $ServiceName -ErrorAction SilentlyContinue))
        Test-Check 'правило фаервола удалено' `
            ($null -eq (Get-NetFirewallRule -DisplayName "$ServiceName panel" -ErrorAction SilentlyContinue))
        # Данные остаются: их удаление — отдельное осознанное действие.
        Test-Check 'данные не удалены вместе со службой' `
            (Test-Path (Join-Path $Scratch 'config\data\mirocraft.db'))
    }
    finally {
        Remove-Everything
    }

    Write-Host ''
    if ($script:Failures -gt 0) {
        Write-Host "$($script:Failures) проверок не прошло" -ForegroundColor Red
        exit 1
    }
    Write-Host 'все проверки прошли' -ForegroundColor Green
}

Main

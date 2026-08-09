<#
.SYNOPSIS
    Устанавливает Mirocraft как службу Windows.

.DESCRIPTION
    Запуск одной командой в PowerShell от администратора:

        irm https://raw.githubusercontent.com/collybia/mirocraft/master/installer/install.ps1 | iex

    Повторный запуск обновляет: бинарник и служба переустанавливаются,
    конфигурация, база и миры остаются на месте. Путь обновления и путь
    установки — один и тот же код; иначе обновление становится тем путём,
    который никто не проверяет.

.PARAMETER Mode
    1 — бесплатный поддомен, 2 — свой домен, 3 — без домена (по адресу).
    Без параметра скрипт спрашивает.

.PARAMETER Binary
    Путь к готовому бинарнику вместо скачивания. Так проверяется сборка из
    исходников и так работает автотест.

.PARAMETER Uninstall
    Убирает службу, правило фаервола и бинарник. Данные и конфигурацию не
    трогает: их удаление должно быть отдельным осознанным действием.
#>

[CmdletBinding()]
param(
    [ValidateSet('1', '2', '3')]
    [string]$Mode,

    [string]$Binary,

    [string]$Version = 'latest',

    # Откуда берутся файлы релиза. Переопределяется для приватного зеркала и
    # для автотеста, который поднимает свой релиз, чтобы загрузка и сверка
    # контрольной суммы проверялись, а не предполагались. Файлы ожидаются по
    # этому адресу по именам, рядом с SHA256SUMS.
    [string]$BaseUrl,

    [switch]$AssumeYes,

    [switch]$Uninstall,

    # Пути и имя службы переопределяются, чтобы установку можно было
    # проверить, не трогая настоящую: автотест ставит во временный каталог
    # под другим именем и на другой порт.
    [string]$InstallDir,
    [string]$ConfigDir,
    [string]$ServiceName = 'Mirocraft',
    [int]$Port = 8080
)

$ErrorActionPreference = 'Stop'

# --- как выглядит установка -------------------------------------------------

$Repo = 'collybia/mirocraft'

if (-not $InstallDir) { $InstallDir = Join-Path $env:ProgramFiles 'Mirocraft' }
if (-not $ConfigDir)  { $ConfigDir  = Join-Path $env:ProgramData 'Mirocraft' }

$BinPath      = Join-Path $InstallDir 'mirocraft.exe'
$ConfigPath   = Join-Path $ConfigDir 'mirocraft.yaml'
$DataDir      = Join-Path $ConfigDir 'data'
$FirewallRule = "$ServiceName panel"

# --- вывод ------------------------------------------------------------------

function Write-Step { param([string]$Text) Write-Host "-> $Text" }
function Write-Ok   { param([string]$Text) Write-Host "OK  $Text" -ForegroundColor Green }
function Write-Warn { param([string]$Text) Write-Host "!   $Text" -ForegroundColor Yellow }

# Test-PortFree сообщает, свободен ли порт.
#
# Спрашивается у системы, а не предполагается: 8080 — самый занятый порт на
# любой машине, где уже что-то работает, и записать его вслепую значит
# получить службу, которая не стартует, и браузер, который попадает в чужой
# сервис. Именно так прошла первая настоящая установка.
function Test-PortFree {
    param([int]$Candidate)

    $listeners = [System.Net.NetworkInformation.IPGlobalProperties]::GetIPGlobalProperties().GetActiveTcpListeners()
    foreach ($listener in $listeners) {
        if ($listener.Port -eq $Candidate) { return $false }
    }
    return $true
}

# Select-Port выбирает порт для панели: заданный, если он свободен, иначе
# первый свободный из запасных.
function Select-Port {
    param([int]$Preferred)

    if (Test-PortFree -Candidate $Preferred) { return $Preferred }

    foreach ($candidate in 8443, 9090, 8090, 8100) {
        if (Test-PortFree -Candidate $candidate) {
            Write-Warn "Порт $Preferred уже занят другой программой, беру $candidate."
            return $candidate
        }
    }

    throw "Порт $Preferred занят, и запасные тоже. Освободите порт или задайте свой: -Port 9443"
}
function Stop-WithError {
    param([string]$Text)
    Write-Host "X   $Text" -ForegroundColor Red
    exit 1
}

# --- проверки ---------------------------------------------------------------

function Assert-Administrator {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = New-Object Security.Principal.WindowsPrincipal($identity)
    if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        Stop-WithError 'Запустите PowerShell от имени администратора: служба и правило фаервола иначе не создадутся.'
    }
}

function Get-Architecture {
    # Служба ставится под ту же разрядность, что и ОС, а не под разрядность
    # PowerShell: 32-битная консоль на 64-битной системе — обычное дело.
    switch ($env:PROCESSOR_ARCHITECTURE) {
        'AMD64' { return 'amd64' }
        'ARM64' { return 'arm64' }
        default {
            if ([Environment]::Is64BitOperatingSystem) { return 'amd64' }
            Stop-WithError "Неподдерживаемая архитектура: $env:PROCESSOR_ARCHITECTURE. Есть сборки под amd64 и arm64."
        }
    }
}

# --- вопросы ----------------------------------------------------------------

function Read-Answer {
    param([string]$Prompt, [string]$Default = '')

    if ($AssumeYes) { return $Default }
    $answer = Read-Host -Prompt $Prompt
    if ([string]::IsNullOrWhiteSpace($answer)) { return $Default }
    return $answer.Trim()
}

function Select-Mode {
    if ($Mode) { return $Mode }

    Write-Host ''
    Write-Host 'Как к панели будут обращаться?'
    Write-Host ''
    Write-Host '  1) Бесплатный поддомен — панель сама получит имя и сертификат.'
    Write-Host '     Понадобится токен deSEC или DuckDNS (бесплатно, минута на регистрацию).'
    Write-Host ''
    Write-Host '  2) Свой домен — если он уже есть и указывает на этот сервер.'
    Write-Host '     Сертификат панель получит сама.'
    Write-Host ''
    Write-Host '  3) Без домена — по IP-адресу, с самоподписанным сертификатом.'
    Write-Host '     Браузер будет предупреждать. Всегда можно перенастроить позже.'
    Write-Host ''

    $answer = Read-Answer -Prompt 'Выберите [1/2/3] (по умолчанию 3)' -Default '3'
    if ($answer -notin @('1', '2', '3')) { return '3' }
    return $answer
}

# --- бинарник ---------------------------------------------------------------

# Сверяет скачанный файл с SHA256SUMS релиза.
#
# Скрипт работает от администратора, а скачанный файл станет службой, поэтому
# «пришло по TLS» — это не весь ответ: не тот релиз, обрубок, который прокси
# отдал с кодом 200, или чьё-то зеркало выглядят как удачная загрузка. Релиз
# без SHA256SUMS не ставится: установщик, который пропускает проверку, когда
# файла нет, — это установщик, у которого проверка не срабатывает никогда.
function Assert-Checksum {
    param([string]$Path, [string]$Asset, [string]$SumsUrl)

    # Кандидат удаляется перед каждым отказом: он не проверен, а непроверенный
    # бинарник во временном каталоге — это тот, который кто-нибудь запустит
    # руками позже.
    $sums = ''
    try {
        $raw = (Invoke-WebRequest -Uri $SumsUrl -UseBasicParsing).Content
        # По HTTP приходит строка, а по file:// (зеркало на диске) — байты.
        $sums = if ($raw -is [byte[]]) { [Text.Encoding]::UTF8.GetString($raw) } else { [string]$raw }
    }
    catch {
        Remove-Item $Path -Force -ErrorAction SilentlyContinue
        Stop-WithError "Не удалось скачать SHA256SUMS — не могу проверить, что скачался нужный файл"
    }

    $expected = ''
    foreach ($line in ($sums -split "`n")) {
        $parts = ($line.Trim() -split '\s+', 2)
        if ($parts.Count -eq 2 -and $parts[1].TrimStart('*') -eq $Asset) {
            $expected = $parts[0]
            break
        }
    }
    if (-not $expected) {
        Remove-Item $Path -Force -ErrorAction SilentlyContinue
        Stop-WithError "В SHA256SUMS нет строки для $Asset"
    }

    $actual = (Get-FileHash -Path $Path -Algorithm SHA256).Hash
    if ($actual -ne $expected.ToUpperInvariant()) {
        Remove-Item $Path -Force -ErrorAction SilentlyContinue
        Stop-WithError "Контрольная сумма не совпала для ${Asset}: ожидалась $expected, получилась $actual"
    }
    Write-Ok 'Контрольная сумма сошлась'
}

# Достаёт бинарник и убеждается, что это он, ничего ещё не меняя на машине.
#
# Отдельно от установки и до неё, потому что установка начинается с остановки
# службы: обновление, сорвавшееся на загрузке или на контрольной сумме, должно
# оставить оператора с работающей панелью, а не с остановленной.
#
# Возвращает путь к готовому файлу и признак того, что файл временный.
function Resolve-BinarySource {
    if ($Binary) {
        if (-not (Test-Path $Binary)) { Stop-WithError "Файл не найден: $Binary" }
        return @{ Path = $Binary; Temporary = $false }
    }

    $arch  = Get-Architecture
    $asset = "mirocraft-windows-$arch.exe"
    $base  = if ($BaseUrl) {
        $BaseUrl.TrimEnd('/')
    } elseif ($Version -eq 'latest') {
        "https://github.com/$Repo/releases/latest/download"
    } else {
        "https://github.com/$Repo/releases/download/$Version"
    }
    $url = "$base/$asset"

    Write-Step "Скачиваю $url"
    # Во временный файл: оборванная загрузка не должна оставить обрубок там,
    # где лежал рабочий бинарник.
    $temp = [IO.Path]::GetTempFileName()
    try {
        Invoke-WebRequest -Uri $url -OutFile $temp -UseBasicParsing
    }
    catch {
        Remove-Item $temp -Force -ErrorAction SilentlyContinue
        Stop-WithError "Не удалось скачать бинарник: $($_.Exception.Message)"
    }

    Assert-Checksum -Path $temp -Asset $asset -SumsUrl "$base/SHA256SUMS"

    return @{ Path = $temp; Temporary = $true }
}

function Install-Binary {
    param([hashtable]$Source)

    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null

    Write-Step 'Устанавливаю бинарник'
    try {
        Copy-Item -Path $Source.Path -Destination $BinPath -Force
    }
    catch {
        Stop-WithError "Не удалось записать $BinPath : $($_.Exception.Message)"
    }
    finally {
        if ($Source.Temporary) { Remove-Item $Source.Path -Force -ErrorAction SilentlyContinue }
    }

    $reported = & $BinPath --version 2>$null
    Write-Ok "Бинарник: $BinPath ($reported)"
}

# --- конфигурация -----------------------------------------------------------

function Write-Configuration {
    param([string]$SelectedMode)

    # Никогда не перезаписывается: правки оператора и токен в ней не должны
    # теряться при обновлении, а обновление — это тот же скрипт.
    if (Test-Path $ConfigPath) {
        Write-Ok "Конфигурация уже есть, не трогаю: $ConfigPath"
        return
    }

    New-Item -ItemType Directory -Force -Path $ConfigDir | Out-Null

    # Только для новой конфигурации: при обновлении порт берётся из неё, и он
    # занят этой же службой.
    $Port = Select-Port -Preferred $Port
    New-Item -ItemType Directory -Force -Path $DataDir | Out-Null

    $dnsProvider = ''; $dnsZone = ''; $dnsToken = ''
    $tlsMode = 'self-signed'; $tlsDomain = ''; $tlsEmail = ''
    $tlsChallenge = 'http-01'; $acceptTos = 'false'

    switch ($SelectedMode) {
        '1' {
            $dnsProvider = Read-Answer -Prompt 'Провайдер — desec (умеет SRV, рекомендуется) или duckdns' -Default 'desec'
            $dnsZone = Read-Answer -Prompt 'Имя, которое вы зарегистрировали (например myserver.dedyn.io)'
            $dnsToken = Read-Answer -Prompt 'Токен провайдера'
            $tlsDomain = $dnsZone
            $tlsMode = 'acme'
            # Такая установка обычно за домашним роутером, где 80-й порт
            # наименее вероятно доступен снаружи, а DNS-01 входящих
            # соединений не требует вовсе.
            $tlsChallenge = 'dns-01'
        }
        '2' {
            $tlsDomain = Read-Answer -Prompt 'Домен, указывающий на этот сервер'
            $tlsEmail = Read-Answer -Prompt 'Почта для уведомлений центра сертификации (можно пусто)'
            $tlsMode = 'acme'

            $cf = Read-Answer -Prompt 'Домен на Cloudflare и есть токен Zone:DNS:Edit? [y/N]' -Default 'N'
            if ($cf -match '^[yY]') {
                $dnsProvider = 'cloudflare'
                $dnsZone = $tlsDomain
                $dnsToken = Read-Answer -Prompt 'Токен Cloudflare'
                $tlsChallenge = 'dns-01'
            }
            else {
                Write-Host ''
                Write-Host '  Сертификат будет получен по 80-му порту. Убедитесь, что он открыт' -ForegroundColor DarkGray
                Write-Host "  снаружи и что A-запись $tlsDomain указывает на этот сервер." -ForegroundColor DarkGray
                Write-Host ''
            }
        }
        default { $tlsMode = 'self-signed' }
    }

    if ($tlsMode -eq 'acme') {
        # Спрашивается, а не подразумевается: согласиться за оператора с
        # чужими условиями — вложить ему слова в рот, и демон без этого всё
        # равно не стартует.
        $tos = Read-Answer -Prompt 'Принимаете условия центра сертификации (Let''s Encrypt)? [Y/n]' -Default 'Y'
        if ($tos -match '^[nN]') {
            Write-Warn 'Без согласия сертификат получить нельзя — ставлю самоподписанный.'
            $tlsMode = 'self-signed'; $tlsDomain = ''; $dnsProvider = ''
        }
        else { $acceptTos = 'true' }
    }

    Write-Step "Пишу $ConfigPath"

    # Обратные слеши в YAML внутри кавычек — источник тихих сюрпризов, поэтому
    # путь записывается со слешами: Go принимает их на Windows везде.
    $dataDirYaml = $DataDir -replace '\\', '/'

    $config = @"
# Конфигурация Mirocraft. Полный список полей с пояснениями —
# https://github.com/$Repo/blob/master/mirocraft.example.yaml

addr: ":$Port"
data_dir: "$dataDirYaml"

log:
  level: "info"
  format: "text"

runner:
  type: "auto"

dns:
  provider: "$dnsProvider"
  zone: "$dnsZone"
  token: "$dnsToken"
  sub: ""

tls:
  mode: "$tlsMode"
  domain: "$tlsDomain"
  email: "$tlsEmail"
  challenge: "$tlsChallenge"
  accept_tos: $acceptTos
"@

    # Без BOM: Go читает YAML как есть, а BOM превращает первый ключ в ключ с
    # невидимым префиксом, который не совпадает ни с чем.
    [IO.File]::WriteAllText($ConfigPath, $config, (New-Object Text.UTF8Encoding($false)))

    Protect-ConfigFile
}

function Protect-ConfigFile {
    # В файле лежит токен провайдера DNS. По умолчанию ProgramData читается
    # всеми, поэтому наследование прав снимается и доступ оставляется только
    # системе и администраторам.
    try {
        $acl = Get-Acl $ConfigPath
        $acl.SetAccessRuleProtection($true, $false)
        $acl.Access | ForEach-Object { [void]$acl.RemoveAccessRule($_) }

        # По SID, а не по именам: на локализованной Windows «BUILTIN\Administrators»
        # называется иначе, и перевод имени в SID не удаётся — права молча
        # остаются наследованными, то есть токен читают все.
        $wellKnown = @(
            [Security.Principal.WellKnownSidType]::LocalSystemSid,
            [Security.Principal.WellKnownSidType]::BuiltinAdministratorsSid
        )
        foreach ($sidType in $wellKnown) {
            $sid = New-Object Security.Principal.SecurityIdentifier($sidType, $null)
            $rule = New-Object Security.AccessControl.FileSystemAccessRule(
                $sid, 'FullControl', 'None', 'None', 'Allow')
            $acl.AddAccessRule($rule)
        }
        Set-Acl -Path $ConfigPath -AclObject $acl
    }
    catch {
        Write-Warn "Не удалось ограничить доступ к $ConfigPath — проверьте права вручную: $($_.Exception.Message)"
    }
}

# --- служба -----------------------------------------------------------------

# Останавливает работающую установку перед тем, как трогать бинарник.
#
# Отдельно от Install-Service и до него: Windows держит файл запущенной
# программы, и запись поверх exe работающей службы не проходит вовсе. На Linux
# это сходит с рук, поэтому порядок шагов легко списать с install.sh и
# получить установщик, который работает ровно один раз — на чистой машине.
function Stop-ExistingService {
    $existing = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    if (-not $existing -or $existing.Status -eq 'Stopped') { return }

    Write-Step 'Останавливаю службу для обновления'
    try {
        Stop-Service -Name $ServiceName -Force -ErrorAction Stop
        $existing.WaitForStatus('Stopped', [TimeSpan]::FromSeconds(60))
    }
    catch {
        # Служба, помеченная на удаление, ещё видна, но уже не открывается.
        # Это не повод прекращать установку: если файл при этом кем-то занят,
        # об этом скажет запись бинарника, и скажет понятнее.
        Write-Warn "Не удалось остановить службу: $($_.Exception.Message)"
    }
}

function Install-Service {
    if (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) {
        # Пересоздаётся всегда: параметры запуска принадлежат установщику, и
        # обновление, которому нужен новый флаг, должно его получить.
        & sc.exe delete $ServiceName | Out-Null
        Start-Sleep -Seconds 1
    }

    Write-Step 'Создаю службу Windows'
    $binLine = '"{0}" --config "{1}"' -f $BinPath, $ConfigPath
    & sc.exe create $ServiceName binPath= $binLine start= auto DisplayName= 'Mirocraft' | Out-Null
    if ($LASTEXITCODE -ne 0) { Stop-WithError 'Не удалось создать службу' }

    & sc.exe description $ServiceName 'Панель управления Minecraft-серверами' | Out-Null
    # Перезапуск после падения: панель держит запущенные серверы, и её уход
    # оставляет их без управления.
    & sc.exe failure $ServiceName reset= 86400 actions= restart/5000/restart/15000/restart/60000 | Out-Null
}

function Open-Firewall {
    param([int]$Port)

    $existing = Get-NetFirewallRule -DisplayName $FirewallRule -ErrorAction SilentlyContinue
    if ($existing) {
        Write-Ok 'Правило фаервола уже есть'
        return
    }

    Write-Step "Открываю порт $Port в фаерволе"
    try {
        New-NetFirewallRule -DisplayName $FirewallRule -Direction Inbound `
            -Action Allow -Protocol TCP -LocalPort $Port -Profile Any | Out-Null
        Write-Ok "Порт $Port открыт"
    }
    catch {
        # Не смертельно: панель работает, просто снаружи её не видно, — и об
        # этом лучше сказать, чем оставить оператора гадать.
        Write-Warn "Не удалось создать правило фаервола: $($_.Exception.Message)"
        Write-Warn "Откройте порт $Port вручную, иначе панель будет доступна только с этой машины."
    }
}

# --- удаление ---------------------------------------------------------------

function Uninstall-Mirocraft {
    Write-Step 'Убираю службу'
    $service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    if ($service) {
        if ($service.Status -ne 'Stopped') { Stop-Service -Name $ServiceName -Force }
        & sc.exe delete $ServiceName | Out-Null
    }

    Get-NetFirewallRule -DisplayName $FirewallRule -ErrorAction SilentlyContinue |
        Remove-NetFirewallRule -ErrorAction SilentlyContinue

    Remove-Item -Path $InstallDir -Recurse -Force -ErrorAction SilentlyContinue

    Write-Ok 'Служба, правило фаервола и бинарник удалены'
    Write-Host ''
    Write-Host "  Данные и конфигурация остались: $ConfigDir"
    Write-Host '  Удалите их вручную, если они больше не нужны — это отдельное'
    Write-Host '  осознанное действие, а не побочный эффект деинсталляции.'
    Write-Host ''
}

# --- адрес панели -----------------------------------------------------------

function Get-PanelUrl {
    # Из конфигурации, а не из ответов мастера: при обновлении мастер не
    # запускался, и файл — единственный источник правды в обоих случаях.
    $scheme = 'http'
    $selfSigned = $false
    $panelHost = ''
    $panelPort = "$Port"

    if (Test-Path $ConfigPath) {
        $text = Get-Content $ConfigPath -Raw

        if ($text -match '(?m)^\s*mode:\s*"([^"]*)"') {
            switch ($Matches[1]) {
                'acme'        { $scheme = 'https' }
                'self-signed' { $scheme = 'https'; $selfSigned = $true }
            }
        }
        if ($text -match '(?m)^\s*domain:\s*"([^"]+)"') { $panelHost = $Matches[1] }
        if ($text -match '(?m)^addr:\s*"([^"]*)"') {
            $addr = $Matches[1]
            if ($addr -match ':(\d+)$') { $panelPort = $Matches[1] }
        }
    }

    if (-not $panelHost) {
        $panelHost = (Get-NetIPAddress -AddressFamily IPv4 -ErrorAction SilentlyContinue |
            Where-Object { $_.IPAddress -notlike '127.*' -and $_.IPAddress -notlike '169.254.*' } |
            Select-Object -First 1 -ExpandProperty IPAddress)
    }
    if (-not $panelHost) { $panelHost = 'localhost' }

    return [pscustomobject]@{
        Url        = "${scheme}://${panelHost}:${panelPort}"
        SelfSigned = $selfSigned
        Port       = [int]$panelPort
    }
}

# --- всё вместе -------------------------------------------------------------

function Main {
    Assert-Administrator

    if ($Uninstall) { Uninstall-Mirocraft; return }

    Write-Host ''
    Write-Host 'Mirocraft — установка'
    Write-Host ''

    $upgrading = Test-Path $BinPath
    if ($upgrading) {
        Write-Step 'Найдена существующая установка — обновляю, данные и настройки не трогаю'
    }

    $source = Resolve-BinarySource
    Stop-ExistingService
    Install-Binary -Source $source

    New-Item -ItemType Directory -Force -Path $DataDir | Out-Null

    $selectedMode = '3'
    if (-not (Test-Path $ConfigPath)) { $selectedMode = Select-Mode }
    Write-Configuration -SelectedMode $selectedMode

    Install-Service

    $panel = Get-PanelUrl
    Open-Firewall -Port $panel.Port

    Write-Step 'Запускаю службу'
    Start-Service -Name $ServiceName

    # Проверяется, а не предполагается: сообщить об успехе, пока служба падает
    # по кругу, значит отправить оператора чинить не то.
    $waited = 0
    while ($waited -lt 30) {
        $service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
        if ($service -and $service.Status -eq 'Running') { break }
        Start-Sleep -Seconds 1
        $waited++
    }

    $service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    if (-not $service -or $service.Status -ne 'Running') {
        Write-Warn 'Служба не поднялась. Что пишет демон:'
        & $BinPath --config $ConfigPath 2>&1 | Select-Object -First 20 | ForEach-Object { Write-Host "    $_" }
        Stop-WithError 'Установка не завершена'
    }

    Write-Host ''
    Write-Ok 'Служба запущена'

    if ($upgrading) {
        Write-Host ''
        Write-Host 'Обновлено. Настройки и данные на месте.'
        Write-Host "  Журнал службы: Get-EventLog -LogName Application -Source Mirocraft -Newest 20"
        return
    }

    Write-Host ''
    Write-Host 'Готово.'
    Write-Host ''
    Write-Host "  Панель:  $($panel.Url)"
    if ($panel.SelfSigned) {
        Write-Host '           Сертификат самоподписанный — браузер предупредит.' -ForegroundColor DarkGray
        Write-Host '           Это ожидаемо: соединение шифруется, но подтвердить, что это' -ForegroundColor DarkGray
        Write-Host '           именно ваш сервер, некому.' -ForegroundColor DarkGray
    }
    Show-Credentials
    Write-Host ''
}

# Show-Credentials печатает логин и пароль здесь же, в том окне, куда человек
# и так смотрит.
#
# Демон печатает их при первом старте, но стартует он службой — значит, в
# журнал, а не на экран. Отправлять оператора открывать файл — это лишний шаг
# между «установлено» и «вошёл», и именно на нём установка перестаёт
# ощущаться законченной. Файл при этом остаётся: окно закроют, и он окажется
# единственной копией.
function Show-Credentials {
    $file = Join-Path $DataDir 'initial-admin.txt'

    $waited = 0
    while (-not (Test-Path $file) -and $waited -lt 10) {
        Start-Sleep -Seconds 1
        $waited++
    }

    if (-not (Test-Path $file)) {
        Write-Host '  Вход:    учётной записью, которая у вас уже есть' -ForegroundColor DarkGray
        return
    }

    $text = Get-Content $file -Raw
    $login = if ($text -match '(?m)^login:\s*(.+)$') { $Matches[1].Trim() } else { '' }
    $password = if ($text -match '(?m)^password:\s*(.+)$') { $Matches[1].Trim() } else { '' }

    if (-not $login -or -not $password) {
        Write-Host "  Логин и пароль администратора: $file"
        return
    }

    Write-Host ''
    Write-Host "  Логин:   $login"
    Write-Host "  Пароль:  $password"
    Write-Host ''
    Write-Host '  Смените пароль после первого входа. Он также лежит в' -ForegroundColor DarkGray
    Write-Host "  $file — удалите файл, когда сохраните." -ForegroundColor DarkGray
}

Main

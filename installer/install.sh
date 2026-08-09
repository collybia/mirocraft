#!/usr/bin/env bash
#
# Mirocraft installer for Debian and Ubuntu.
#
#   curl -fsSL https://raw.githubusercontent.com/collybia/mirocraft/master/installer/install.sh | sudo bash
#
# Idempotent: running it again upgrades the binary and leaves the
# configuration, the database and every world alone. That is not a nicety —
# the upgrade path and the install path being the same script is what stops
# the upgrade path from being the one nobody tests.

set -euo pipefail

# --- what the install looks like -------------------------------------------

REPO="collybia/mirocraft"
BIN_PATH="/usr/local/bin/mirocraft"
CONFIG_DIR="/etc/mirocraft"
CONFIG_PATH="${CONFIG_DIR}/mirocraft.yaml"
DATA_DIR="/var/lib/mirocraft"
SERVICE_NAME="mirocraft"
SERVICE_PATH="/etc/systemd/system/${SERVICE_NAME}.service"
SERVICE_USER="mirocraft"

# Overridable so the test in a container can point at a local build rather
# than at GitHub.
MIROCRAFT_VERSION="${MIROCRAFT_VERSION:-latest}"
MIROCRAFT_BINARY="${MIROCRAFT_BINARY:-}"

# Where the release assets are fetched from. Overridable for a private mirror
# and for the test, which serves a release of its own so the download and the
# checksum check are exercised rather than assumed. The assets are expected
# under this address by name, alongside SHA256SUMS.
MIROCRAFT_BASE_URL="${MIROCRAFT_BASE_URL:-}"

# MIROCRAFT_MODE picks the DNS/TLS arrangement without asking:
#   1  free subdomain   2  own domain   3  no domain, address only
# Set it to run unattended; without it the script asks.
MIROCRAFT_MODE="${MIROCRAFT_MODE:-}"

# The port the panel listens on. Empty means 8080, or the next free one when
# something already holds it.
MIROCRAFT_PORT="${MIROCRAFT_PORT:-}"
MIROCRAFT_ASSUME_YES="${MIROCRAFT_ASSUME_YES:-}"

# Worked out from the configuration at the end, and printed.
PANEL_URL=""
PANEL_SELF_SIGNED="no"

# --- output ----------------------------------------------------------------

if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
    C_BOLD=$'\033[1m'; C_DIM=$'\033[2m'; C_RED=$'\033[31m'
    C_GREEN=$'\033[32m'; C_YELLOW=$'\033[33m'; C_OFF=$'\033[0m'
else
    C_BOLD=""; C_DIM=""; C_RED=""; C_GREEN=""; C_YELLOW=""; C_OFF=""
fi

say()  { printf '%s\n' "$*"; }
step() { printf '%s→%s %s\n' "${C_BOLD}" "${C_OFF}" "$*"; }
ok()   { printf '%s✓%s %s\n' "${C_GREEN}" "${C_OFF}" "$*"; }
warn() { printf '%s!%s %s\n' "${C_YELLOW}" "${C_OFF}" "$*" >&2; }
die()  { printf '%s✗%s %s\n' "${C_RED}" "${C_OFF}" "$*" >&2; exit 1; }

# --- checks ----------------------------------------------------------------

# port_in_use reports whether anything is already listening on a port.
#
# ss where it exists, netstat where it does not, and a connection attempt as a
# last resort: this has to work on a minimal image, and being wrong in the
# "free" direction is the expensive direction — the service gets installed and
# then cannot start.
port_in_use() {
    local port="$1"

    if command -v ss >/dev/null 2>&1; then
        ss -lnt 2>/dev/null | awk '{print $4}' | grep -qE "[:.]${port}$"
        return
    fi
    if command -v netstat >/dev/null 2>&1; then
        netstat -lnt 2>/dev/null | awk '{print $4}' | grep -qE "[:.]${port}$"
        return
    fi
    (exec 3<>"/dev/tcp/127.0.0.1/${port}") 2>/dev/null && { exec 3<&-; return 0; }
    return 1
}

# choose_port picks the port the panel will listen on.
#
# Checked rather than assumed. 8080 is the most contended port on any machine
# that already runs anything, and writing it blind produces a service in a
# restart loop plus a browser landing on whatever else is there — which reads
# as "the panel is broken" and not as "the port was taken". That is exactly how
# the first real install of this went.
#
# Only ever called for a new configuration: on an upgrade the existing one is
# kept, and the port already in it is this daemon's own.
choose_port() {
    local port="${MIROCRAFT_PORT:-8080}"

    if ! port_in_use "${port}"; then
        printf '%s
' "${port}"
        return
    fi

    local candidate
    for candidate in 8443 9090 8090 8100; do
        if ! port_in_use "${candidate}"; then
            warn "Порт ${port} уже занят другой программой, беру ${candidate}."
            printf '%s
' "${candidate}"
            return
        fi
    done

    die "Порт ${port} занят, и запасные тоже. Освободите порт или задайте свой:
     MIROCRAFT_PORT=9443 curl -fsSL .../install.sh | sudo -E bash"
}

require_root() {
    if [ "$(id -u)" -ne 0 ]; then
        die "Запустите под root: sudo bash install.sh"
    fi
}

# Refuse rather than half-work on a distribution whose package manager and
# service layout are different. A failed install an operator can read beats a
# working-looking one that breaks on the first reboot.
require_supported_os() {
    if [ ! -r /etc/os-release ]; then
        die "Не удалось определить дистрибутив: нет /etc/os-release"
    fi
    # shellcheck disable=SC1091
    . /etc/os-release

    case "${ID:-}${ID_LIKE:-}" in
        *debian*|*ubuntu*) ;;
        *) die "Поддерживаются Debian и Ubuntu; здесь ${PRETTY_NAME:-${ID:-неизвестно}}" ;;
    esac

    if ! command -v systemctl >/dev/null 2>&1; then
        die "Нужен systemd: без него нечем держать демон запущенным"
    fi
}

detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64) echo "amd64" ;;
        aarch64|arm64) echo "arm64" ;;
        *) die "Неподдерживаемая архитектура: $(uname -m). Есть сборки под amd64 и arm64" ;;
    esac
}

# --- installing the binary -------------------------------------------------

# verify_checksum compares a downloaded file against the release's SHA256SUMS.
#
# This script runs as root and the file it downloads runs as a service, so
# "it arrived over TLS" is not the whole answer: a wrong release, a truncated
# body a proxy handed back with a 200, or a mirror nobody audited all look
# like a successful download. A release without SHA256SUMS is refused rather
# than installed unverified — an installer that skips the check when the file
# is missing is one whose check never runs.
verify_checksum() {
    local file="$1" asset="$2" sums_url="$3"

    command -v sha256sum >/dev/null 2>&1 || die "Нужен sha256sum (пакет coreutils)"

    # The candidate is removed before every die: it is unverified, and an
    # unverified binary left in /tmp is the one someone runs by hand later.
    local sums expected actual
    sums="$(mktemp)"
    if ! curl -fsSL --retry 3 -o "${sums}" "${sums_url}"; then
        rm -f "${sums}" "${file}"
        die "Не удалось скачать SHA256SUMS — не могу проверить, что скачался нужный файл"
    fi

    expected="$(awk -v a="${asset}" '$2 == a || $2 == "*" a { print $1 }' "${sums}" | head -1)"
    rm -f "${sums}"
    if [ -z "${expected}" ]; then
        rm -f "${file}"
        die "В SHA256SUMS нет строки для ${asset}"
    fi

    actual="$(sha256sum "${file}" | awk '{ print $1 }')"
    if [ "${actual}" != "${expected}" ]; then
        rm -f "${file}"
        die "Контрольная сумма не совпала для ${asset}: ожидалась ${expected}, получилась ${actual}"
    fi
    ok "Контрольная сумма сошлась"
}

install_binary() {
    local arch
    arch="$(detect_arch)"

    if [ -n "${MIROCRAFT_BINARY}" ]; then
        # A local file: used by the test in a container, and by anyone who
        # built from source.
        step "Устанавливаю бинарник из ${MIROCRAFT_BINARY}"
        [ -f "${MIROCRAFT_BINARY}" ] || die "Файл не найден: ${MIROCRAFT_BINARY}"
        install -m 0755 "${MIROCRAFT_BINARY}" "${BIN_PATH}"
    else
        local asset base url sums_url
        asset="mirocraft-linux-${arch}"
        if [ -n "${MIROCRAFT_BASE_URL}" ]; then
            base="${MIROCRAFT_BASE_URL%/}"
        elif [ "${MIROCRAFT_VERSION}" = "latest" ]; then
            base="https://github.com/${REPO}/releases/latest/download"
        else
            base="https://github.com/${REPO}/releases/download/${MIROCRAFT_VERSION}"
        fi
        url="${base}/${asset}"
        sums_url="${base}/SHA256SUMS"

        step "Скачиваю ${url}"
        # To a temporary file first: an interrupted download must not leave a
        # truncated binary where a working one was.
        local tmp
        tmp="$(mktemp)"
        if ! curl -fsSL --retry 3 -o "${tmp}" "${url}"; then
            rm -f "${tmp}"
            die "Не удалось скачать бинарник. Проверьте сеть и что релиз ${MIROCRAFT_VERSION} существует"
        fi

        verify_checksum "${tmp}" "${asset}" "${sums_url}"

        install -m 0755 "${tmp}" "${BIN_PATH}"
        rm -f "${tmp}"
    fi

    ok "Бинарник: ${BIN_PATH} ($("${BIN_PATH}" --version 2>/dev/null || echo 'версия не определилась'))"
}

# --- the service account ---------------------------------------------------

create_user() {
    if id "${SERVICE_USER}" >/dev/null 2>&1; then
        return
    fi

    step "Создаю системного пользователя ${SERVICE_USER}"
    # No login shell and no home: this account exists to own files and run one
    # process. A panel reachable from the internet should not also be an
    # account someone can log into.
    useradd --system --no-create-home --shell /usr/sbin/nologin "${SERVICE_USER}"
}

# --- Docker ----------------------------------------------------------------

install_docker() {
    if command -v docker >/dev/null 2>&1; then
        ok "Docker уже установлен"
        return
    fi

    step "Ставлю Docker (серверы будут запускаться в контейнерах)"
    if ! curl -fsSL https://get.docker.com | sh; then
        # Not fatal: the panel runs servers as processes without Docker, and
        # saying so is better than refusing to install at all.
        warn "Docker установить не удалось. Панель будет запускать серверы процессами —"
        warn "это рабочий режим, просто без лимитов памяти уровня контейнера."
        return
    fi

    usermod -aG docker "${SERVICE_USER}" 2>/dev/null || true
    ok "Docker установлен"
}

# --- the wizard ------------------------------------------------------------

ask() {
    local prompt="$1" default="${2:-}" answer
    if [ -n "${MIROCRAFT_ASSUME_YES}" ]; then
        printf '%s\n' "${default}"
        return
    fi
    read -r -p "${prompt}" answer </dev/tty || answer=""
    printf '%s\n' "${answer:-${default}}"
}

choose_mode() {
    if [ -n "${MIROCRAFT_MODE}" ]; then
        printf '%s\n' "${MIROCRAFT_MODE}"
        return
    fi

    cat >&2 <<'MENU'

Как к панели будут обращаться?

  1) Бесплатный поддомен — панель сама получит имя и сертификат.
     Понадобится токен deSEC или DuckDNS (бесплатно, минута на регистрацию).

  2) Свой домен — если он уже есть и указывает на этот сервер.
     Сертификат панель получит сама.

  3) Без домена — по IP-адресу, с самоподписанным сертификатом.
     Браузер будет предупреждать. Всегда можно перенастроить позже.

MENU
    ask "Выберите [1/2/3] (по умолчанию 3): " "3"
}

# --- configuration ---------------------------------------------------------

write_config() {
    local mode="$1"

    # Never overwritten. An operator who edited it, or whose token lives in
    # it, must not lose that to an upgrade — which is the same script.
    if [ -f "${CONFIG_PATH}" ]; then
        ok "Конфигурация уже есть, не трогаю: ${CONFIG_PATH}"
        return
    fi

    mkdir -p "${CONFIG_DIR}"

    local port
    port="$(choose_port)"

    local dns_provider="" dns_zone="" dns_token="" dns_sub=""
    local tls_mode="self-signed" tls_domain="" tls_email="" tls_challenge="http-01" accept_tos="false"

    case "${mode}" in
        1)
            local provider
            provider="$(ask "Провайдер — desec (умеет SRV, рекомендуется) или duckdns [desec]: " "desec")"
            dns_provider="${provider}"
            dns_zone="$(ask "Имя, которое вы зарегистрировали (например myserver.dedyn.io): " "")"
            dns_token="$(ask "Токен провайдера: " "")"
            tls_domain="${dns_zone}"
            tls_mode="acme"
            # The free-subdomain path usually sits behind a home router, where
            # port 80 is the thing least likely to be reachable — and DNS-01
            # needs no inbound connection at all.
            tls_challenge="dns-01"
            ;;
        2)
            tls_domain="$(ask "Домен, указывающий на этот сервер: " "")"
            tls_mode="acme"
            tls_email="$(ask "Почта для уведомлений центра сертификации (можно пусто): " "")"
            local cf
            cf="$(ask "Домен на Cloudflare и есть токен Zone:DNS:Edit? [y/N]: " "N")"
            if [ "${cf}" = "y" ] || [ "${cf}" = "Y" ]; then
                dns_provider="cloudflare"
                dns_zone="${tls_domain}"
                dns_token="$(ask "Токен Cloudflare: " "")"
                tls_challenge="dns-01"
            else
                say ""
                say "  ${C_DIM}Сертификат будет получен по 80-му порту. Убедитесь, что он открыт"
                say "  снаружи и что A-запись ${tls_domain} указывает на этот сервер.${C_OFF}"
                say ""
            fi
            ;;
        3|*)
            tls_mode="self-signed"
            ;;
    esac

    if [ "${tls_mode}" = "acme" ]; then
        # Asked, not assumed: agreeing to someone else's terms on their behalf
        # is putting words in their mouth, and the daemon refuses to start
        # without it anyway.
        local tos
        tos="$(ask "Принимаете условия центра сертификации (Let's Encrypt)? [Y/n]: " "Y")"
        if [ "${tos}" = "n" ] || [ "${tos}" = "N" ]; then
            warn "Без согласия сертификат получить нельзя — ставлю самоподписанный."
            tls_mode="self-signed"; tls_domain=""; dns_provider=""
        else
            accept_tos="true"
        fi
    fi

    step "Пишу ${CONFIG_PATH}"
    cat >"${CONFIG_PATH}" <<CONFIG
# Конфигурация Mirocraft. Полный список полей с пояснениями —
# https://github.com/${REPO}/blob/master/mirocraft.example.yaml

addr: ":${port}"
data_dir: "${DATA_DIR}"

log:
  level: "info"
  format: "text"

runner:
  type: "auto"

dns:
  provider: "${dns_provider}"
  zone: "${dns_zone}"
  token: "${dns_token}"
  sub: "${dns_sub}"

tls:
  mode: "${tls_mode}"
  domain: "${tls_domain}"
  email: "${tls_email}"
  challenge: "${tls_challenge}"
  accept_tos: ${accept_tos}
CONFIG

    # The token lives in here, so the file is readable only by the account
    # that needs it.
    chown root:"${SERVICE_USER}" "${CONFIG_PATH}"
    chmod 0640 "${CONFIG_PATH}"
}

# read_panel_url works out the address to print at the end.
#
# From the configuration, because getting this wrong is not cosmetic: a panel
# serving HTTPS and an installer printing http:// sends the operator to
# "Client sent an HTTP request to an HTTPS server", which reads like a broken
# install rather than a wrong URL. Found exactly that way, in a container.
read_panel_url() {
    local scheme="http" host port tls_mode domain
    tls_mode="$(grep -A1 '^tls:' "${CONFIG_PATH}" | grep 'mode:' | cut -d'"' -f2 || true)"
    domain="$(grep -A2 '^tls:' "${CONFIG_PATH}" | grep 'domain:' | cut -d'"' -f2 || true)"
    port="$(grep '^addr:' "${CONFIG_PATH}" | cut -d'"' -f2 | sed 's/^.*://' || true)"
    : "${port:=8080}"

    PANEL_SELF_SIGNED="no"
    case "${tls_mode}" in
        acme)        scheme="https" ;;
        self-signed) scheme="https"; PANEL_SELF_SIGNED="yes" ;;
    esac

    host="${domain}"
    if [ -z "${host}" ]; then
        host="$(hostname -I 2>/dev/null | awk '{print $1}')"
    fi
    : "${host:=localhost}"

    PANEL_URL="${scheme}://${host}:${port}"
    PANEL_PORT="${port}"
}

# --- the service -----------------------------------------------------------

# open_firewall lets the panel's port through a firewall this machine runs
# itself.
#
# The Windows installer has always added its rule; this one did not, and the
# result was an install that reports success and a panel nobody can reach —
# the same "it does not work" as a crash, with nothing in the journal to
# explain it, because from the daemon's side nothing is wrong.
#
# Only firewalls that are actually enabled are touched, and only the one port.
# A machine with no firewall gets nothing done to it: turning one on because
# an installer felt like it would be a change nobody asked for.
open_firewall() {
    local port="$1"

    if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q '^Status: active'; then
        if ufw allow "${port}/tcp" >/dev/null 2>&1; then
            ok "Открыл порт ${port} в ufw"
        else
            warn "Не смог открыть порт ${port} в ufw — сделайте вручную: ufw allow ${port}/tcp"
        fi
        return
    fi

    if command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
        if firewall-cmd --permanent --add-port="${port}/tcp" >/dev/null 2>&1 &&
           firewall-cmd --reload >/dev/null 2>&1; then
            ok "Открыл порт ${port} в firewalld"
        else
            warn "Не смог открыть порт ${port} в firewalld — сделайте вручную:
     firewall-cmd --permanent --add-port=${port}/tcp && firewall-cmd --reload"
        fi
        return
    fi

    # No firewall here to open. The one in front of the machine — the
    # hoster's — is not visible from inside it, and is the usual reason a
    # panel that works locally cannot be reached.
    PANEL_FIREWALL_UNKNOWN="yes"
}

write_service() {
    step "Пишу юнит systemd"

    # Rewritten on every run on purpose: this file is the installer's, not the
    # operator's, and an upgrade that needs a new setting should get it.
    cat >"${SERVICE_PATH}" <<UNIT
[Unit]
Description=Mirocraft — панель управления Minecraft-серверами
Documentation=https://github.com/${REPO}
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_USER}
ExecStart=${BIN_PATH} --config ${CONFIG_PATH}
WorkingDirectory=${DATA_DIR}
Restart=on-failure
RestartSec=5s

# The panel binds 443 and 80 for certificates, which are privileged ports, but
# nothing else here needs root. This grants exactly that one capability
# instead.
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE

# A panel that runs Minecraft servers is, by design, running other people's
# code. These limit what that code can reach if it gets out.
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=full
ProtectHome=yes
ReadWritePaths=${DATA_DIR}

# Servers are Java: the default limit is far too low for a JVM's threads and
# open world files.
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
UNIT

    systemctl daemon-reload
}

# --- putting it together ---------------------------------------------------

main() {
    require_root
    require_supported_os

    say ""
    say "${C_BOLD}Mirocraft${C_OFF} — установка"
    say ""

    local upgrading="no"
    if [ -f "${BIN_PATH}" ]; then
        upgrading="yes"
        step "Найдена существующая установка — обновляю, данные и настройки не трогаю"
    fi

    create_user
    install_binary

    mkdir -p "${DATA_DIR}"
    chown -R "${SERVICE_USER}:${SERVICE_USER}" "${DATA_DIR}"
    chmod 0750 "${DATA_DIR}"

    local mode="3"
    if [ "${upgrading}" = "no" ] && [ ! -f "${CONFIG_PATH}" ]; then
        mode="$(choose_mode)"
        install_docker
    fi
    write_config "${mode}"
    # Read back from the file rather than from the wizard's variables: on an
    # upgrade the wizard did not run, and the file is the truth either way.
    read_panel_url
    write_service

    step "Запускаю службу"
    systemctl enable "${SERVICE_NAME}" >/dev/null 2>&1 || true
    systemctl restart "${SERVICE_NAME}"

    # Waited for rather than assumed: reporting success while the daemon is
    # crash-looping is how an operator ends up debugging the wrong thing.
    local waited=0
    while [ "${waited}" -lt 30 ]; do
        if systemctl is-active --quiet "${SERVICE_NAME}"; then
            break
        fi
        sleep 1
        waited=$((waited + 1))
    done

    if ! systemctl is-active --quiet "${SERVICE_NAME}"; then
        say ""
        warn "Служба не поднялась. Последние строки журнала:"
        journalctl -u "${SERVICE_NAME}" -n 20 --no-pager >&2 || true
        die "Установка не завершена"
    fi

    say ""
    ok "Служба запущена"

    # After the service, not before: a port opened for a daemon that never
    # started is a hole for nothing.
    open_firewall "${PANEL_PORT}"

    if [ "${upgrading}" = "yes" ]; then
        say ""
        say "Обновлено. Настройки и данные на месте."
        say "  Журнал:  ${C_DIM}journalctl -u ${SERVICE_NAME} -f${C_OFF}"
        return
    fi

    # The generated password is written by the daemon on first run; showing
    # where rather than reprinting it, because the file is the copy that
    # survives a closed terminal.
    say ""
    say "${C_BOLD}Готово.${C_OFF}"
    say ""
    say "  Панель:  ${PANEL_URL}"
    if [ "${PANEL_SELF_SIGNED}" = "yes" ]; then
        say "           ${C_DIM}Сертификат самоподписанный — браузер предупредит."
        say "           Это ожидаемо: соединение шифруется, но подтвердить, что это"
        say "           именно ваш сервер, некому. Панель говорит о том же на своей"
        say "           странице настроек.${C_OFF}"
    fi
    print_credentials
    if [ "${PANEL_FIREWALL_UNKNOWN:-no}" = "yes" ]; then
        say "  ${C_DIM}Если панель не открывается снаружи, а локально отвечает —"
        say "  порт ${PANEL_PORT} закрыт фаерволом хостера. Он не виден с самой"
        say "  машины, откройте его в панели провайдера.${C_OFF}"
        say ""
    fi
    say "  Журнал:  ${C_DIM}journalctl -u ${SERVICE_NAME} -f${C_OFF}"
    say ""
}

# print_credentials shows the login and the password here, in the terminal the
# operator is already looking at.
#
# The daemon prints them on its first start, but it starts as a service, so
# they go to the journal and not to anybody's screen. Sending the operator to
# open a file afterwards is one more step between "installed" and "logged in",
# and it is the step where an install stops feeling finished.
#
# The file stays where it is: a terminal gets closed, and then it is the only
# copy left.
print_credentials() {
    local file="${DATA_DIR}/initial-admin.txt"

    # Written on the first start, which has just happened; give it a moment
    # rather than racing it.
    local waited=0
    while [ ! -f "${file}" ] && [ "${waited}" -lt 10 ]; do
        sleep 1
        waited=$((waited + 1))
    done

    if [ ! -f "${file}" ]; then
        # An upgrade over an existing install, where the operator saved the
        # password and deleted the file, as they were asked to.
        say "  Вход:    ${C_DIM}учётной записью, которая у вас уже есть${C_OFF}"
        return
    fi

    local login password
    login="$(sed -n 's/^login: //p' "${file}" | head -1)"
    password="$(sed -n 's/^password: //p' "${file}" | head -1)"

    if [ -z "${login}" ] || [ -z "${password}" ]; then
        say "  Логин и пароль администратора: ${file}"
        return
    fi

    say ""
    say "  ${C_BOLD}Логин:   ${login}${C_OFF}"
    say "  ${C_BOLD}Пароль:  ${password}${C_OFF}"
    say ""
    say "  ${C_DIM}Смените пароль после первого входа. Он также лежит в"
    say "  ${file} — удалите файл, когда сохраните.${C_OFF}"
    say ""
}

main "$@"

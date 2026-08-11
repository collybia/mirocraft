#!/usr/bin/env bash
#
# Runs install.sh in a clean container and checks what it produced.
#
#   ./installer/test-install.sh                 # Ubuntu 24.04 and Debian 12
#   ./installer/test-install.sh ubuntu:24.04    # one image
#
#   MIROCRAFT_TEST_RELEASE=1 ./installer/test-install.sh
#       also installs from the published GitHub release, which is the path an
#       operator takes. Needs the network and a release that exists.
#
# An installer is the one piece of this project that cannot be unit tested:
# it creates users, writes systemd units and installs packages, and a stub
# for all of that would only test the stub. So it is checked the only way that
# means anything — by running it, on a machine that has never seen it, and
# looking at what is actually there afterwards.

set -euo pipefail

# Git Bash on Windows rewrites arguments that look like Unix paths into
# Windows ones. That is right for paths on this machine and wrong for paths
# inside the container — /sys/fs/cgroup and /tmp/install.sh are the
# container's, and rewriting them leaves it dead on arrival. So the rewriting
# is off, and host paths are converted explicitly by host_path below.
export MSYS_NO_PATHCONV=1

# host_path renders a path the way the docker client expects to read it from
# this machine. A no-op anywhere but Git Bash.
host_path() {
    if command -v cygpath >/dev/null 2>&1; then
        cygpath -w "$1"
    else
        printf '%s' "$1"
    fi
}

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMAGES=("jrei/systemd-ubuntu:24.04" "jrei/systemd-debian:12")
INSTALL_URL="${MIROCRAFT_INSTALL_URL:-https://raw.githubusercontent.com/collybia/mirocraft/master/installer/install.sh}"
if [ "$#" -gt 0 ]; then
    IMAGES=("$@")
fi

failures=0

check() {
    local name="$1" ok="$2" detail="${3:-}"
    if [ "${ok}" = "yes" ]; then
        printf 'ok   %s\n' "${name}"
    else
        printf 'FAIL %s%s\n' "${name}" "${detail:+ — ${detail}}" >&2
        failures=$((failures + 1))
    fi
}

# in runs a command in the container and returns its output.
in_container() {
    docker exec "${CONTAINER}" bash -c "$1"
}

# The panel is driven through its own API, over HTTPS with an untrusted
# certificate — which is exactly how an operator reaches a mode-3 install.
login_script() {
    cat <<'SCRIPT'
PW=$(grep -oP 'password: \K.*' /var/lib/mirocraft/initial-admin.txt)
TOKEN=$(curl -sk -X POST https://127.0.0.1:8080/api/v1/auth/login   -H 'Content-Type: application/json'   -d "{\"email\":\"admin\",\"password\":\"$PW\"}"   | python3 -c 'import sys,json;print(json.load(sys.stdin)["token"])')
SCRIPT
}

create_server_script() {
    login_script
    cat <<'SCRIPT'
curl -sk -X POST https://127.0.0.1:8080/api/v1/servers   -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json'   -d '{"name":"upgrade-survivor","core":"paper","version":"1.21.4","ram_mb":1024,"eula_accepted":true}'
SCRIPT
}

# expect_one_server_script exits zero when exactly one server is recorded.
#
# The comparison lives in the script rather than in the caller: quoting it
# through docker exec twice is how a check ends up testing its own escaping.
expect_one_server_script() {
    login_script
    cat <<'SCRIPT'
COUNT=$(curl -sk https://127.0.0.1:8080/api/v1/servers -H "Authorization: Bearer $TOKEN"   | python3 -c 'import sys,json;print(len(json.load(sys.stdin)["items"]))')
[ "$COUNT" = "1" ]
SCRIPT
}

# yes_no turns a command's exit status into something check understands.
yes_no() {
    if in_container "$1" >/dev/null 2>&1; then echo "yes"; else echo "no"; fi
}

build_binary() {
    printf '→ Building a Linux binary\n'
    ( cd "${REPO_ROOT}" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
        go build -o "$(host_path "${BINARY}")" ./cmd/mirocraft )
}

start_container() {
    local image="$1"
    printf '→ Starting %s\n' "${image}"

    # Privileged with the host's cgroups, because the thing under test is a
    # systemd unit and systemd needs both. This is a throwaway container.
    CONTAINER="$(docker run -d --privileged --cgroupns=host \
        -v /sys/fs/cgroup:/sys/fs/cgroup:rw "${image}")"

    # Polled rather than "systemctl is-system-running --wait": that blocks
    # until the whole boot settles, which outlasts any timeout wrapped around
    # it and looks like the test hanging.
    local waited=0
    while [ "${waited}" -lt 30 ]; do
        local state
        state="$(in_container 'systemctl is-system-running' 2>/dev/null | tr -d '[:space:]' || true)"
        case "${state}" in
            running|degraded) break ;;
        esac
        sleep 1
        waited=$((waited + 1))
    done

    in_container "apt-get update -qq && apt-get install -y -qq curl python3" >/dev/null 2>&1 || true
}

stop_container() {
    [ -n "${CONTAINER:-}" ] && docker rm -f "${CONTAINER}" >/dev/null 2>&1 || true
    CONTAINER=""
}

run_for_image() {
    local image="$1"
    printf '\n=== %s ===\n' "${image}"

    start_container "${image}"
    trap stop_container EXIT

    docker cp "$(host_path "${REPO_ROOT}/installer/install.sh")" "${CONTAINER}:/tmp/install.sh" >/dev/null
    docker cp "$(host_path "${BINARY}")" "${CONTAINER}:/tmp/mirocraft" >/dev/null

    # Mode 3: no domain. The other two need a real one, and an installer test
    # that quietly needed the internet would fail for the wrong reasons.
    local output
    output="$(docker exec \
        -e MIROCRAFT_MODE=3 -e MIROCRAFT_ASSUME_YES=1 -e MIROCRAFT_BINARY=/tmp/mirocraft \
        "${CONTAINER}" bash /tmp/install.sh 2>&1)" || {
        printf '%s\n' "${output}" >&2
        check "the installer completes" "no"
        stop_container; trap - EXIT
        return
    }
    check "the installer completes" "yes"

    check "the service is running" "$(yes_no 'systemctl is-active --quiet mirocraft')"
    check "the service starts on boot" "$(yes_no 'systemctl is-enabled --quiet mirocraft')"

    # Printed, not filed away. The step between "installed" and "logged in"
    # should not be "go and open a file".
    local password
    password="$(in_container "sed -n 's/^password: //p' /var/lib/mirocraft/initial-admin.txt | head -1" 2>/dev/null | tr -d '
')"
    check "the login is printed"         "$(case "${output}" in *"Логин:"*admin*) echo yes ;; *) echo no ;; esac)"
    check "the password is printed"         "$(if [ -n "${password}" ] && case "${output}" in *"${password}"*) true ;; *) false ;; esac; then echo yes; else echo no; fi)"

    # Not as root: a panel that runs other people's Minecraft servers should
    # not be one of them running as root.
    local user
    user="$(in_container "ps -o user= -C mirocraft | head -1" | tr -d '[:space:]' || true)"
    check "the daemon runs unprivileged" \
        "$([ "${user}" != "root" ] && echo yes || echo no)" "running as ${user}"

    # The token lives in the config, so it must not be world-readable.
    local perms
    perms="$(in_container 'stat -c %a /etc/mirocraft/mirocraft.yaml' | tr -d '[:space:]' || true)"
    check "the configuration is not world-readable" \
        "$([ "${perms}" = "640" ] && echo yes || echo no)" "mode ${perms}"

    check "the panel answers over https" \
        "$(yes_no 'curl -fsk https://127.0.0.1:8080/api/v1/health | grep -q ok')"

    # The address the installer printed has to be the one that works. A panel
    # serving HTTPS behind an http:// in the output sends the operator to a
    # baffling error that reads like a broken install.
    check "the printed address uses the right scheme" \
        "$(printf '%s' "${output}" | grep -q 'Панель:  https://' && echo yes || echo no)"
    check "the self-signed certificate is explained" \
        "$(printf '%s' "${output}" | grep -q 'самоподписанный' && echo yes || echo no)"

    check "the admin password was generated" \
        "$(yes_no 'grep -q password: /var/lib/mirocraft/initial-admin.txt')"

    # --- and again, which is the upgrade path ---

    # A server created through the API, because that is what an operator would
    # lose. Comparing the database file itself proves nothing: the daemon is
    # running and writing to it, so its checksum changes on its own.
    in_container "$(create_server_script)" >/dev/null 2>&1 || true
    check "a server can be created before the upgrade" "$(yes_no "$(expect_one_server_script)")"

    in_container 'touch /etc/mirocraft/EDITED-BY-OPERATOR' >/dev/null

    docker exec -e MIROCRAFT_ASSUME_YES=1 -e MIROCRAFT_BINARY=/tmp/mirocraft \
        "${CONTAINER}" bash /tmp/install.sh >/dev/null 2>&1 || {
        check "the installer is idempotent" "no"
        stop_container; trap - EXIT
        return
    }
    check "the installer is idempotent" "yes"

    check "the created server survives the upgrade" "$(yes_no "$(expect_one_server_script)")"
    check "an operator's own files survive an upgrade" \
        "$(yes_no 'test -f /etc/mirocraft/EDITED-BY-OPERATOR')"
    check "the service is running after the upgrade" "$(yes_no 'systemctl is-active --quiet mirocraft')"

    check_download_path
    check_home_mode "${image}"

    stop_container
    trap - EXIT
}

# Mode 4: the panel on somebody's own computer.
#
# Checked on a fresh machine rather than by editing the previous install,
# because what is being tested is the choice the wizard makes on behalf of a
# home user: bound to loopback, plain HTTP, no firewall rule, and an address
# that actually opens. A localhost bind with an https:// in the output, or a
# link naming the machine's LAN address, is the same broken install from the
# operator's side.
check_home_mode() {
    local image="$1"

    stop_container
    start_container "${image}"
    docker cp "$(host_path "${REPO_ROOT}/installer/install.sh")" "${CONTAINER}:/tmp/install.sh" >/dev/null
    docker cp "$(host_path "${BINARY}")" "${CONTAINER}:/tmp/mirocraft" >/dev/null

    local output
    output="$(docker exec         -e MIROCRAFT_MODE=4 -e MIROCRAFT_ASSUME_YES=1 -e MIROCRAFT_BINARY=/tmp/mirocraft         "${CONTAINER}" bash /tmp/install.sh 2>&1)" || {
        printf '%s
' "${output}" >&2
        check "the home-mode install completes" "no"
        return
    }
    check "the home-mode install completes" "yes"

    check "home mode binds loopback only"         "$(yes_no 'grep -q "^addr: \"127.0.0.1:" /etc/mirocraft/mirocraft.yaml')"
    check "home mode serves plain http"         "$(yes_no 'curl -fsS http://127.0.0.1:8080/api/v1/health | grep -q ok')"
    check "home mode prints a localhost address"         "$(printf '%s' "${output}" | grep -q 'Панель:  http://localhost:' && echo yes || echo no)"
    check "home mode opens no firewall port"         "$(printf '%s' "${output}" | grep -q 'правило фаервола не нужно' && echo yes || echo no)"
    # The panel is private; the servers are not. Saying so is the whole point
    # of the mode, and an operator who thinks friends cannot connect will not
    # use it.
    check "home mode explains how friends still connect"         "$(printf '%s' "${output}" | grep -q 'Подключение' && echo yes || echo no)"
}

# The path every real operator takes: no local binary, so the installer
# downloads a release and verifies it. Served from inside the container so the
# test does not depend on a published release — what is being checked is the
# installer's own download and checksum handling, not GitHub's uptime.
check_download_path() {
    in_container 'mkdir -p /srv/release && cp /tmp/mirocraft /srv/release/mirocraft-linux-$(dpkg --print-architecture) && cd /srv/release && sha256sum mirocraft-* > SHA256SUMS' >/dev/null

    # setsid, because a server started through docker exec dies with the exec
    # when the command returns.
    in_container 'cd /srv/release && setsid python3 -m http.server 8099 >/dev/null 2>&1 < /dev/null &' >/dev/null 2>&1 || true
    in_container 'for i in $(seq 1 20); do curl -fsS http://127.0.0.1:8099/SHA256SUMS >/dev/null 2>&1 && break; sleep 1; done' >/dev/null 2>&1 || true

    local output
    output="$(docker exec -e MIROCRAFT_ASSUME_YES=1 -e MIROCRAFT_BASE_URL=http://127.0.0.1:8099 \
        "${CONTAINER}" bash /tmp/install.sh 2>&1)" || output="FAILED: ${output}"
    check "the installer can install from a release" \
        "$(printf '%s' "${output}" | grep -q 'Контрольная сумма сошлась' && echo yes || echo no)" \
        "$(printf '%s' "${output}" | tail -3)"

    # And the half that matters: a file that does not match must not be
    # installed. A checksum check that has only ever been seen to pass is
    # indistinguishable from no checksum check at all.
    in_container 'printf tampered >> /srv/release/mirocraft-linux-$(dpkg --print-architecture)' >/dev/null
    local rejected="no"
    if ! output="$(docker exec -e MIROCRAFT_ASSUME_YES=1 -e MIROCRAFT_BASE_URL=http://127.0.0.1:8099 \
        "${CONTAINER}" bash /tmp/install.sh 2>&1)"; then
        printf '%s' "${output}" | grep -q 'Контрольная сумма не совпала' && rejected="yes"
    fi
    check "a tampered download is refused" "${rejected}"

    # The refusal must also have changed nothing: the binary that was already
    # installed is still the one running.
    check "the refusal left the working install alone" \
        "$(yes_no 'systemctl is-active --quiet mirocraft && curl -fsk https://127.0.0.1:8080/api/v1/health | grep -q ok')"
}

# run_from_release installs the way an operator does: no binary handed over,
# no base URL redirected — the script downloads the published release and
# verifies it against the published checksums.
#
# This is the case the rest of this file did not cover. Every check above hands
# the installer a locally built binary through MIROCRAFT_BINARY, which tests
# everything except the download — and the download is what failed the first
# time anyone ran the installer for real: the workflow had never been tagged,
# so releases/latest/download/... answered 404 and the install stopped after
# creating the user.
#
# Needs the network and a published release, so it is opt-in.
run_from_release() {
    local image="$1"
    printf '
=== %s, from the published release ===
' "${image}"

    start_container "${image}"
    trap stop_container EXIT

    local output
    output="$(docker exec -e MIROCRAFT_MODE=3 -e MIROCRAFT_ASSUME_YES=1 "${CONTAINER}" bash -c         "curl -fsSL ${INSTALL_URL} | bash" 2>&1)" || {
        printf '%s
' "${output}" >&2
        check "the installer completes against the real release" "no"
        stop_container; trap - EXIT
        return
    }
    check "the installer completes against the real release" "yes"
    check "it verified the checksum"         "$(case "${output}" in *"Контрольная сумма сошлась"*) echo yes ;; *) echo no ;; esac)"
    check "the service is running" "$(yes_no 'systemctl is-active --quiet mirocraft')"
    check "the panel answers over https"         "$(yes_no 'curl -fsk https://127.0.0.1:8080/api/v1/health | grep -q ok')"

    # The state an operator is left in by a failed attempt: the user exists,
    # nothing else does. Re-running has to finish the job rather than trip
    # over what is already there.
    stop_container; trap - EXIT
    start_container "${image}"
    trap stop_container EXIT

    in_container "useradd --system --home-dir /var/lib/mirocraft --shell /usr/sbin/nologin mirocraft" >/dev/null 2>&1
    if docker exec -e MIROCRAFT_MODE=3 -e MIROCRAFT_ASSUME_YES=1 "${CONTAINER}" bash -c         "curl -fsSL ${INSTALL_URL} | bash" >/dev/null 2>&1; then
        check "a re-run after a failed attempt completes" "yes"
    else
        check "a re-run after a failed attempt completes" "no"
    fi
    check "and leaves a running service"         "$(yes_no 'systemctl is-active --quiet mirocraft')"

    stop_container
    trap - EXIT
}

main() {
    command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 2; }

    # Built beside the repository rather than in /tmp: on Git Bash the shell's
    # /tmp and the one a Windows program sees are different directories, so a
    # binary written to one cannot be found through the other.
    BUILD_DIR="${REPO_ROOT}/.installer-test"
    BINARY="${BUILD_DIR}/mirocraft"
    mkdir -p "${BUILD_DIR}"
    trap 'rm -rf "${BUILD_DIR}"' EXIT
    build_binary

    for image in "${IMAGES[@]}"; do
        run_for_image "${image}"
        if [ -n "${MIROCRAFT_TEST_RELEASE:-}" ]; then
            run_from_release "${image}"
        fi
    done

    printf '\n'
    if [ "${failures}" -gt 0 ]; then
        printf '%d check(s) failed\n' "${failures}" >&2
        exit 1
    fi
    printf 'all checks passed\n'
}

main "$@"

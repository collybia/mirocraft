#!/usr/bin/env bash
#
# Runs install.sh in a clean container and checks what it produced.
#
#   ./installer/test-install.sh                 # Ubuntu 24.04 and Debian 12
#   ./installer/test-install.sh ubuntu:24.04    # one image
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
    done

    printf '\n'
    if [ "${failures}" -gt 0 ]; then
        printf '%d check(s) failed\n' "${failures}" >&2
        exit 1
    fi
    printf 'all checks passed\n'
}

main "$@"

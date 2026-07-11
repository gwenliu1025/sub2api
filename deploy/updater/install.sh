#!/usr/bin/env bash
set -euo pipefail

usage() {
    cat <<'EOF'
Usage: install.sh [options]

Required:
  --compose-directory PATH
  --compose-file PATH
  --environment-file PATH
  --service-name NAME
  --container-name NAME
  --app-uid UID
  --socket-gid GID
  --expected-architecture ARCH
  --health-url URL
  --image-repository REPOSITORY
  --image-source URL

Optional:
  --prepare-timeout-seconds SECONDS     Default: 600
  --activation-timeout-seconds SECONDS  Default: 120
  --poll-interval-seconds SECONDS        Default: 2
  --help
EOF
}

fail() {
    printf 'install.sh: %s\n' "$*" >&2
    exit 1
}

require_value() {
    [[ $# -ge 2 && -n "${2:-}" ]] || fail "$1 requires a value"
}

failure_hint() {
    status=$?
    if [[ -n "${config_temporary:-}" ]]; then
        rm -f -- "$config_temporary"
    fi
    if [[ $status -ne 0 ]]; then
        printf '%s\n' \
            'Installation failed. Inspect logs with:' \
            '  journalctl -u sub2api-updater.service -n 100 --no-pager' >&2
    fi
    exit "$status"
}
trap failure_hint EXIT

[[ ${EUID:-$(id -u)} -eq 0 ]] || fail "must run as root"
[[ -x /usr/bin/python3 ]] || fail "/usr/bin/python3 is required"
command -v docker >/dev/null 2>&1 || fail "docker is required"
docker compose version >/dev/null 2>&1 \
    || fail "Docker Compose v2 is required"
command -v systemctl >/dev/null 2>&1 || fail "systemctl is required"

compose_directory=
compose_file=
environment_file=
service_name=
container_name=
app_uid=
socket_gid=
expected_architecture=
health_url=
image_repository=
image_source=
prepare_timeout_seconds=600
activation_timeout_seconds=120
poll_interval_seconds=2
config_temporary=

while [[ $# -gt 0 ]]; do
    case "$1" in
        --compose-directory)
            require_value "$@"
            compose_directory=$2
            shift 2
            ;;
        --compose-file)
            require_value "$@"
            compose_file=$2
            shift 2
            ;;
        --environment-file)
            require_value "$@"
            environment_file=$2
            shift 2
            ;;
        --service-name)
            require_value "$@"
            service_name=$2
            shift 2
            ;;
        --container-name)
            require_value "$@"
            container_name=$2
            shift 2
            ;;
        --app-uid)
            require_value "$@"
            app_uid=$2
            shift 2
            ;;
        --socket-gid)
            require_value "$@"
            socket_gid=$2
            shift 2
            ;;
        --expected-architecture)
            require_value "$@"
            expected_architecture=$2
            shift 2
            ;;
        --health-url)
            require_value "$@"
            health_url=$2
            shift 2
            ;;
        --image-repository)
            require_value "$@"
            image_repository=$2
            shift 2
            ;;
        --image-source)
            require_value "$@"
            image_source=$2
            shift 2
            ;;
        --prepare-timeout-seconds)
            require_value "$@"
            prepare_timeout_seconds=$2
            shift 2
            ;;
        --activation-timeout-seconds)
            require_value "$@"
            activation_timeout_seconds=$2
            shift 2
            ;;
        --poll-interval-seconds)
            require_value "$@"
            poll_interval_seconds=$2
            shift 2
            ;;
        --help)
            usage
            trap - EXIT
            exit 0
            ;;
        *)
            fail "unknown option: $1"
            ;;
    esac
done

for required_value in \
    "$compose_directory" \
    "$compose_file" \
    "$environment_file" \
    "$service_name" \
    "$container_name" \
    "$app_uid" \
    "$socket_gid" \
    "$expected_architecture" \
    "$health_url" \
    "$image_repository" \
    "$image_source"; do
    [[ -n "$required_value" ]] || {
        usage >&2
        fail "all required options must be provided"
    }
done

[[ -d "$compose_directory" ]] \
    || fail "compose directory does not exist: $compose_directory"
[[ -f "$compose_file" ]] \
    || fail "compose file does not exist: $compose_file"
[[ -f "$environment_file" ]] \
    || fail "environment file does not exist: $environment_file"

script_directory=$(
    CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd
)
[[ -f "$script_directory/sub2api_updater.py" ]] \
    || fail "sub2api_updater.py is missing"
[[ -f "$script_directory/updater_core.py" ]] \
    || fail "updater_core.py is missing"
[[ -f "$script_directory/sub2api-updater.service" ]] \
    || fail "sub2api-updater.service is missing"

install -d -o root -g root -m 0755 /opt/sub2api-updater
install -d -o root -g root -m 0755 /etc/sub2api-updater
install -o root -g root -m 0755 \
    "$script_directory/sub2api_updater.py" \
    /opt/sub2api-updater/sub2api_updater.py
install -o root -g root -m 0644 \
    "$script_directory/updater_core.py" \
    /opt/sub2api-updater/updater_core.py
install -o root -g root -m 0644 \
    "$script_directory/sub2api-updater.service" \
    /etc/systemd/system/sub2api-updater.service
chmod 0644 /etc/systemd/system/sub2api-updater.service

config_temporary=$(
    mktemp /etc/sub2api-updater/.config.json.XXXXXX
)
chmod 0600 "$config_temporary"

/usr/bin/python3 - \
    "$config_temporary" \
    "$compose_directory" \
    "$compose_file" \
    "$environment_file" \
    "$service_name" \
    "$container_name" \
    "$app_uid" \
    "$socket_gid" \
    "$expected_architecture" \
    "$health_url" \
    "$image_repository" \
    "$image_source" \
    "$prepare_timeout_seconds" \
    "$activation_timeout_seconds" \
    "$poll_interval_seconds" <<'PY'
import json
import math
import posixpath
import re
import sys
from urllib.parse import urlsplit

(
    output_path,
    compose_directory,
    compose_file,
    environment_file,
    service_name,
    container_name,
    app_uid_text,
    socket_gid_text,
    expected_architecture,
    health_url,
    image_repository,
    image_source,
    prepare_timeout_text,
    activation_timeout_text,
    poll_interval_text,
) = sys.argv[1:]

name_pattern = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.-]*$")
repository_pattern = re.compile(
    r"^[A-Za-z0-9][A-Za-z0-9._-]*(?::[0-9]+)?"
    r"(?:/[A-Za-z0-9][A-Za-z0-9._-]*)+$"
)


def absolute_path(value, label):
    if (
        not posixpath.isabs(value)
        or value.startswith("//")
        or posixpath.normpath(value) != value
    ):
        raise SystemExit(f"{label} must be a normalized absolute path")
    return value


def integer(value, label, minimum):
    if re.fullmatch(r"[0-9]+", value) is None:
        raise SystemExit(f"{label} must be an integer")
    parsed = int(value)
    if parsed < minimum or parsed > 2147483647:
        raise SystemExit(f"{label} is outside the supported range")
    return parsed


def positive_number(value, label):
    try:
        parsed = float(value)
    except ValueError:
        raise SystemExit(f"{label} must be a positive number") from None
    if not math.isfinite(parsed) or parsed <= 0:
        raise SystemExit(f"{label} must be a positive number")
    return parsed


def url(value, label, schemes):
    if any(character.isspace() for character in value):
        raise SystemExit(f"{label} is invalid")
    try:
        parsed = urlsplit(value)
        parsed.port
    except ValueError:
        raise SystemExit(f"{label} is invalid") from None
    if (
        parsed.scheme not in schemes
        or not parsed.netloc
        or parsed.hostname is None
        or parsed.username is not None
        or parsed.password is not None
        or parsed.fragment
    ):
        raise SystemExit(f"{label} is invalid")
    return value


for label, value in (
    ("service name", service_name),
    ("container name", container_name),
    ("expected architecture", expected_architecture),
):
    if name_pattern.fullmatch(value) is None:
        raise SystemExit(f"{label} is invalid")
if repository_pattern.fullmatch(image_repository) is None:
    raise SystemExit("image repository is invalid")

app_uid = integer(app_uid_text, "app UID", 0)
socket_gid = integer(socket_gid_text, "socket GID", 0)
allowed_uids = [0] if app_uid == 0 else [0, app_uid]
prepare_timeout = integer(
    prepare_timeout_text,
    "prepare timeout",
    1,
)
activation_timeout = integer(
    activation_timeout_text,
    "activation timeout",
    1,
)
poll_interval = positive_number(poll_interval_text, "poll interval")

config = {
    "socket_path": "/run/sub2api-updater/updater.sock",
    "socket_gid": socket_gid,
    "allowed_uids": allowed_uids,
    "image_repository": image_repository,
    "image_source": url(image_source, "image source", {"https"}),
    "compose_directory": absolute_path(
        compose_directory,
        "compose directory",
    ),
    "compose_file": absolute_path(compose_file, "compose file"),
    "environment_file": absolute_path(
        environment_file,
        "environment file",
    ),
    "service_name": service_name,
    "container_name": container_name,
    "expected_architecture": expected_architecture,
    "health_url": url(health_url, "health URL", {"http", "https"}),
    "prepare_timeout_seconds": prepare_timeout,
    "activation_timeout_seconds": activation_timeout,
    "poll_interval_seconds": poll_interval,
    "state_file": "/var/lib/sub2api-updater/state.json",
}

with open(output_path, "w", encoding="utf-8", newline="\n") as output:
    json.dump(config, output, ensure_ascii=True, indent=2)
    output.write("\n")
PY

chown root:root "$config_temporary"
chmod 0600 "$config_temporary"
mv -f -- "$config_temporary" /etc/sub2api-updater/config.json
config_temporary=
chown root:root /etc/sub2api-updater/config.json
chmod 0600 /etc/sub2api-updater/config.json

systemctl daemon-reload
systemctl enable sub2api-updater.service
systemctl restart sub2api-updater.service

APP_UID="$app_uid" SOCKET_GID="$socket_gid" /usr/bin/python3 - <<'PY'
import json
import os
import socket
import time

app_uid = int(os.environ["APP_UID"])
socket_gid = int(os.environ["SOCKET_GID"])
os.setgroups([])
os.setgid(socket_gid)
os.setuid(app_uid)

socket_path = "/run/sub2api-updater/updater.sock"
request = (
    b"GET /v1/health HTTP/1.1\r\n"
    b"Host: localhost\r\n"
    b"Connection: close\r\n\r\n"
)
deadline = time.monotonic() + 30
last_error = "service did not become ready"

while time.monotonic() < deadline:
    try:
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError("health check deadline exceeded")
            client.settimeout(min(2, remaining))
            client.connect(socket_path)

            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError("health check deadline exceeded")
            client.settimeout(min(2, remaining))
            client.sendall(request)

            response = bytearray()
            while True:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise TimeoutError(
                        "health check deadline exceeded"
                    )
                client.settimeout(min(2, remaining))
                chunk = client.recv(8192)
                if not chunk:
                    break
                response.extend(chunk)
                if len(response) > 65536:
                    raise RuntimeError("health response is too large")

        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("health check deadline exceeded")
        header_bytes, body = bytes(response).split(b"\r\n\r\n", 1)
        header_lines = header_bytes.split(b"\r\n")
        if header_lines[0].split()[1] != b"200":
            raise RuntimeError("health endpoint did not return HTTP 200")
        headers = {}
        for line in header_lines[1:]:
            name, value = line.split(b":", 1)
            headers[name.strip().lower()] = value.strip()
        if int(headers[b"content-length"]) != len(body):
            raise RuntimeError("health response length is invalid")
        payload = json.loads(body.decode("utf-8"))
        if payload != {"ready": True, "protocol_version": "v1"}:
            raise RuntimeError("health response is not ready/v1")
        break
    except (IndexError, KeyError, OSError, ValueError, RuntimeError) as error:
        last_error = str(error)
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            continue
        sleep_seconds = min(1, remaining)
        time.sleep(sleep_seconds)
else:
    raise SystemExit(f"updater health check failed: {last_error}")
PY

trap - EXIT
printf '%s\n' "sub2api-updater installed and ready"

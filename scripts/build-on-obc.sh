#!/usr/bin/env bash
#
# Build everything on the OBC and load the flight controller firmware.
#
# Usage:
#   scripts/build-on-obc.sh <ip> [user] [--no-flash] [--no-sync] [--key PATH]
#
#   --no-flash    build only, leave the flight controller alone
#   --no-sync     build what is already on the OBC, do not rsync sources
#
# What "everything" means on the OBC: the seven Go binaries (obc and the bench
# tools). The Wails console `gs` is the operator laptop's, and the OBC has
# neither npm nor webkit2gtk -- it is skipped with a message when the toolchain
# is absent, exactly as scripts/build.sh skips it without Node.
#
# The build runs NATIVELY on the OBC. Cross-compiling needs an arm64
# cross-toolchain and matching libzmq; the Pi has a Go toolchain and builds
# these in minutes, so the portability problem is simply not had.
#
# The firmware goes through scripts/flash-firmware.sh ON the OBC -- the flight
# controller is on the OBC's USB, not on the operator laptop. After the flash
# the script demands one real telemetry frame (pico_bench -raw), because a
# board that enumerated is not a board that runs the firmware.
#
# Authentication is key-only (BatchMode), like scripts/deploy.sh:
#   ssh-copy-id -i ~/.ssh/id_ed25519_rocsar.pub root@<ip>
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

KEY="$HOME/.ssh/id_ed25519_rocsar"
POS=()
SYNC=true
FLASH=true
while [[ $# -gt 0 ]]; do
    case "$1" in
        --no-sync)  SYNC=false; shift ;;
        --no-flash) FLASH=false; shift ;;
        --key) KEY="${2:?--key needs a path}"; shift 2 ;;
        --key=*) KEY="${1#--key=}"; shift ;;
        -*) echo "unknown flag: $1" >&2; exit 1 ;;
        *) POS+=("$1"); shift ;;
    esac
done
if [[ ${#POS[@]} -lt 1 || ${#POS[@]} -gt 2 ]]; then
    echo "usage: $0 <ip> [user] [--no-flash] [--no-sync] [--key PATH]" >&2
    exit 1
fi

PI_IP="${POS[0]}"
SSH_USER="${POS[1]:-root}"
SSH_HOST="${SSH_USER}@${PI_IP}"
SSH="ssh -i $KEY -o BatchMode=yes -o ConnectTimeout=10 $SSH_HOST"
RSYNC_SSH="ssh -i $KEY -o BatchMode=yes -o ConnectTimeout=10"

[[ -f "$KEY" ]] || { echo "[obc] no key at $KEY (see header for one-time setup)" >&2; exit 1; }
$SSH true || { echo "[obc] cannot reach $SSH_HOST with $KEY" >&2; exit 1; }

# The build tree on the OBC. Overridable: PI_DIR=/root/rocsar scripts/build-on-obc.sh ...
PI_DIR="${PI_DIR:-$($SSH 'printf %s "$HOME"')/rocsar-obc}"

if $SYNC; then
    echo "[obc] syncing sources to $SSH_HOST:$PI_DIR/"
    $SSH "mkdir -p '$PI_DIR'"
    rsync -az --delete -e "$RSYNC_SSH" \
        --exclude 'bin/' --exclude 'node_modules/' --exclude '__pycache__/' \
        api cmd internal tools firmware scripts go.mod go.sum \
        "${SSH_HOST}:${PI_DIR}/"
fi

echo "[obc] building on $SSH_HOST (native $( $SSH 'uname -m'))"
$SSH "set -e
    cd '$PI_DIR'
    mkdir -p bin
    CGO_ENABLED=1 go build -o bin/obc ./cmd/obc
    for t in gs_cli gs_probe camera_bench gnss_bench pico_bench sdr_bench; do
        CGO_ENABLED=1 go build -o \"bin/\$t\" \"./tools/\$t\"
    done
    if command -v wails >/dev/null 2>&1 && command -v npm >/dev/null 2>&1; then
        (cd cmd/gs && wails build -s)
    else
        echo '[obc] skipping gs: no wails/npm on the OBC (build it on the laptop)'
    fi
    ls -la bin/
"

if $FLASH; then
    echo "[obc] flashing the flight controller from the OBC"
    $SSH "cd '$PI_DIR' && ./scripts/flash-firmware.sh"

    echo "[obc] verifying: one real telemetry frame"
    $SSH "timeout 30 '$PI_DIR'/bin/pico_bench -raw -seconds 3" \
        | tee /tmp/rocsar-pico-verify.txt
    if ! grep -q -- '-- age' /tmp/rocsar-pico-verify.txt; then
        echo "[obc] FAIL: the flight controller enumerated but sent no telemetry" >&2
        exit 1
    fi
fi

echo "[obc] ok -> $SSH_HOST:$PI_DIR/bin"

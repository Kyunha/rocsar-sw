#!/usr/bin/env bash
# Deploy the OBC to the Pi: cross-compile, push, restart, verify.
#
# Usage:
#   scripts/deploy.sh <pi-ip> [ssh-user] [--key PATH] [--rollback]
#
# --rollback restores /usr/local/bin/obc.prev (the binary this script backed
# up on the previous deploy) instead of pushing a new one. There is exactly
# one generation of backup: a deploy overwrites obc.prev, so two bad deploys
# in a row leave no way back except rebuilding from git. The script says so
# after every run rather than assuming anyone remembers.
#
# Authentication is key-only (BatchMode): a deploy that stops for a password
# prompt is a deploy that hangs a CI job or an impatient operator into
# answering "yes" to something they cannot see. One-time setup:
#   ssh-copy-id -i ~/.ssh/id_ed25519_rocsar.pub root@<pi-ip>
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

ROLLBACK=false
KEY="$HOME/.ssh/id_ed25519_rocsar"
POS=()
while [[ $# -gt 0 ]]; do
    case "$1" in
        --rollback) ROLLBACK=true; shift ;;
        --key) KEY="${2:?--key needs a path}"; shift 2 ;;
        --key=*) KEY="${1#--key=}"; shift ;;
        -*) echo "unknown flag: $1" >&2; exit 1 ;;
        *) POS+=("$1"); shift ;;
    esac
done
if [[ ${#POS[@]} -lt 1 || ${#POS[@]} -gt 2 ]]; then
    echo "usage: $0 <pi-ip> [ssh-user] [--key PATH] [--rollback]" >&2
    exit 1
fi

PI_IP="${POS[0]}"
SSH_USER="${POS[1]:-pi}"
SSH_HOST="${SSH_USER}@${PI_IP}"
SSH="ssh -i $KEY -o BatchMode=yes -o ConnectTimeout=10 $SSH_HOST"
RSYNC_SSH="ssh -i $KEY -o BatchMode=yes -o ConnectTimeout=10"

# Fail here, not ten minutes in: the key must exist and the host must answer
# before anything builds. BatchMode means "no key, no deploy" rather than a
# password prompt nobody is watching.
[[ -f "$KEY" ]] || { echo "[deploy] no key at $KEY (see header for one-time setup)" >&2; exit 1; }
$SSH true || { echo "[deploy] cannot reach $SSH_HOST with $KEY" >&2; exit 1; }

# Wait for the OBC's control socket to answer, then demand one real frame.
# An open port proves the process rebound; only a frame proves the pipeline
# survived the restart. gs_cli status blocks until the first frame or its own
# timeout, so a silent OBC fails here instead of in flight.
health_check() {
    echo "[deploy] waiting for telemetry"
    for _ in $(seq 1 30); do
        if (echo > /dev/tcp/"$PI_IP"/5556) 2>/dev/null; then
            break
        fi
        sleep 2
    done
    (echo > /dev/tcp/"$PI_IP"/5556) 2>/dev/null \
        || { echo "[deploy] FAIL: telemetry socket never came back" >&2; return 1; }
    echo "[deploy] one live frame:"
    go run ./tools/gs_cli \
        --control "tcp://$PI_IP:5555" --telemetry "tcp://$PI_IP:5556" \
        status
}

if $ROLLBACK; then
    echo "[deploy] rolling back to obc.prev on $SSH_HOST"
    $SSH "sudo cp /usr/local/bin/obc.prev /usr/local/bin/obc && sudo systemctl restart obc"
    health_check
    echo "[deploy] rolled back and healthy"
    exit 0
fi

# CGO_ENABLED=1 is explicit rather than left to default. go-zeromq/zmq4 links
# against libzmq through cgo, so a CGO_ENABLED=0 build compiles cleanly and then
# fails at startup -- the worst time to find out. Cross-compiling needs a
# matching arm64 cross-toolchain and libzmq; if you would rather not maintain
# one, build natively on the Pi instead.
echo "[deploy] cross-compiling obc for linux/arm64"
CGO_ENABLED=1 GOOS=linux GOARCH=arm64 \
    go build -trimpath -ldflags "-s -w" -o bin/obc ./cmd/obc

echo "[deploy] backing up the running binary on $SSH_HOST"
$SSH "sudo cp /usr/local/bin/obc /usr/local/bin/obc.prev"

echo "[deploy] deploying to ${SSH_HOST}"
rsync -az --progress -e "$RSYNC_SSH" bin/obc "${SSH_HOST}:/tmp/obc"

echo "[deploy] restarting obc service"
$SSH "sudo mv /tmp/obc /usr/local/bin/obc && sudo systemctl restart obc"

health_check
echo "[deploy] ok -- previous binary kept at /usr/local/bin/obc.prev"
echo "[deploy] rollback, if ever needed: $0 $PI_IP $SSH_USER --rollback"

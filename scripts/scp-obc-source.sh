#!/usr/bin/env bash
#
# Quick-deploy OBC source to Pi4B for on-device compilation.
#
# Usage:
#   scripts/scp-obc-source.sh <pi-ip>            # scp source to pi user pi
#   scripts/scp-obc-source.sh <pi-ip> usr        # scp source to specific user
#   scripts/scp-obc-source.sh <pi-ip> pi --build # scp source + auto-build on Pi
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

PI_IP="${1:?usage: $0 <pi-ip> [ssh-user]}"
PI_USER="${2:-pi}"

if [[ "$3" == "--build" ]]; then
    BUILD=true
else
    BUILD=false
fi

KEY="${HOME}/.ssh/id_ed25519_rocsar"

[[ -f "$KEY" ]] || { echo "no key at $KEY"; exit 1; }
SSH="ssh -i $KEY -o BatchMode=yes -o ConnectTimeout=10 ${PI_USER}@${PI_IP}"
$SSH true || { echo "cannot reach Pi"; exit 1; }

PI_DIR="/home/${PI_USER}/rocsar-obc"

echo "[deploy] copying source to ${PI_USER}@${PI_IP}:${PI_DIR}/"
rsync -az --progress cmd/obc/ "${PI_USER}@${PI_IP}:${PI_DIR}/"
rsync -az --progress go.mod go.sum "${PI_USER}@${PI_IP}:${PI_DIR}/"

echo "[deploy] source deployed"

if $BUILD; then
    echo ""
    echo "=== Building on Pi ==="
    $SSH "cd ${PI_DIR} && go mod tidy && CGO_ENABLED=1 GOOS=linux GOARCH=arm64 go build -o /usr/local/bin/obc ./cmd/obc"

    echo "[deploy] restarting obc service"
    $SSH "sudo mv /tmp/obc /usr/local/bin/obc 2>/dev/null; sudo systemctl restart obc"

    # Health check
    echo "[deploy] waiting for telemetry"
    for _ in $(seq 1 30); do
        if (echo > /dev/tcp/${PI_IP}/5556) 2>/dev/null; then
            break
        fi
        sleep 2
    done
    (echo > /dev/tcp/${PI_IP}/5556) 2>/dev/null \
        || { echo "[deploy] FAIL: telemetry socket never came back"; exit 1; }

    echo "[deploy] ok -- obc built and deployed to ${PI_USER}@${PI_IP}"
fi
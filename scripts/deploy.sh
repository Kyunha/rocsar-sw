#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

if [[ $# -lt 1 ]]; then
    echo "usage: $0 <pi-ip> [ssh-user]" >&2
    exit 1
fi

PI_IP="$1"
SSH_USER="${2:-pi}"
SSH_HOST="${SSH_USER}@${PI_IP}"

# CGO_ENABLED=1 is explicit rather than left to default. go-zeromq/zmq4 links
# against libzmq through cgo, so a CGO_ENABLED=0 build compiles cleanly and then
# fails at startup -- the worst time to find out. Cross-compiling needs a
# matching arm64 cross-toolchain and libzmq; if you would rather not maintain
# one, build natively on the Pi instead.
echo "[deploy] cross-compiling obc for linux/arm64"
CGO_ENABLED=1 GOOS=linux GOARCH=arm64 \
    go build -trimpath -ldflags "-s -w" -o bin/obc ./cmd/obc

echo "[deploy] deploying to ${SSH_HOST}"
rsync -az --progress bin/obc "${SSH_HOST}:/tmp/obc"

echo "[deploy] restarting obc service"
ssh "$SSH_HOST" "sudo mv /tmp/obc /usr/local/bin/obc && sudo systemctl restart obc"

echo "[deploy] ok"

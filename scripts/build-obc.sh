#!/usr/bin/env bash
#
# Build the OBC binary cross-platform and optionally deploy to a Pi4B.
#
# Usage:
#   scripts/build-obc.sh                # build obc for linux/arm64 only (default)
#   scripts/build-obc.sh --all          # build for all supported platforms, skip failures
#   scripts/build-obc.sh --deploy IP    # build + SCP to pi at IP
#   scripts/build-obc.sh --deploy IP usr # build + SCP to pi user@IP
#
# On a host without arm64 cross-compilation tooling, use:
#   scripts/build-obc.sh --os linux --arch amd64
#   scripts/build-obc.sh --deploy <pi-ip>
#
# Authentication is key-only (BatchMode). One-time setup:
#   ssh-copy-id -i ~/.ssh/id_ed25519_rocsar.pub pi@<pi-ip>

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# ---- defaults --------------------------------------------------------

TARGET_OS="linux"
TARGET_ARCH="arm64"
DEPLOY=false
PI_IP=""
PI_USER="pi"
KEY="$HOME/.ssh/id_ed25519_rocsar"
BUILD_ALL=false
SKIP_FAILED=false

# ---- helpers ---------------------------------------------------------

usage() {
    echo "Usage: $0 [--all] [--deploy IP [usr]] [--key PATH] [--os OS] [--arch ARCH] [--skip-failures]"
    echo ""
    echo "  --all             Build for all supported platforms, skip failures"
    echo "  --deploy IP       After building, SCP the binary to the Pi at IP"
    echo "  --os OS           Override target OS (default: linux)"
    echo "  --arch ARCH       Override target arch (default: arm64 for Pi4B)"
    echo "  --key PATH        SSH key path (default: $KEY)"
    echo "  --skip-failures   Skip platforms that fail to build (use with --all)"
    echo "  --help/-h         Show this help"
    exit 0
}

PLATFORMS=()

# ---- parse arguments -------------------------------------------------

while [[ $# -gt 0 ]]; do
    case "$1" in
        --all|-a)        BUILD_ALL=true; shift ;;
        --deploy|-d)
            DEPLOY=true
            PI_IP="${2:?--deploy needs a Pi IP address}"
            PI_USER="${3:-pi}"
            shift 2
            ;;
        --key)
            KEY="${2:?--key needs a path}"; shift 2 ;;
        --key=*)         KEY="${1#--key=}"; shift ;;
        --os)            TARGET_OS="${2:?--os needs an OS}"; shift 2 ;;
        --arch)          TARGET_ARCH="${2:?--arch needs an architecture}"; shift 2 ;;
        --skip-failures) SKIP_FAILED=true; shift ;;
        --help|-h)       usage ;;
        *)               echo "unknown flag: $1" >&2; exit 1 ;;
    esac
done

# Determine which platforms to build
if $BUILD_ALL; then
    PLATFORMS=(
        "linux/arm64"
        "linux/amd64"
        "darwin/amd64"
        "darwin/arm64"
        "windows/amd64"
    )
else
    PLATFORMS=("$TARGET_OS/$TARGET_ARCH")
fi

# Validate at least one platform
if [[ ${#PLATFORMS[@]} -eq 0 ]]; then
    echo "[build] no platforms specified; use --all or --os/--arch" >&2
    exit 1
fi

# Validate SSH key if deploying
if $DEPLOY; then
    [[ -f "$KEY" ]] || {
        echo "[build] no key at $KEY (see header for one-time setup)" >&2
        exit 1
    }
fi

# ---- build -----------------------------------------------------------

mkdir -p bin

BUILT_PLATFORMS=()

for PLAT in "${PLATFORMS[@]}"; do
    OS="${PLAT%/*}"
    ARCH="${PLAT#*/}"

    OUTPUT="obc"
    if [[ "$OS" == "windows" && "$OUTPUT" != *.exe ]]; then
        OUTPUT="${OUTPUT}.exe"
    fi

    echo "[build] cross-compiling obc for $OS/$ARCH"
    if CGO_ENABLED=1 GOOS="$OS" GOARCH="$ARCH" \
        go build -trimpath -ldflags "-s -w" -o "bin/$OUTPUT" ./cmd/obc 2>&1; then
        echo "[build] written: bin/$OUTPUT"
        BUILT_PLATFORMS+=("$PLAT")
    else
        echo "[build] FAILED: $OS/$ARCH — skipping" >&2
        if ! $SKIP_FAILED; then
            exit 1
        fi
    fi
done

# ---- deploy ----------------------------------------------------------

if $DEPLOY; then
    # Determine which binary to deploy
    BINARY="bin/obc"
    if [[ "$TARGET_OS" == "windows" ]]; then
        BINARY="bin/obc.exe"
    fi

    # If we built linux/arm64 but it failed and --skip-failures is set,
    # fall back to whatever we did build (must be linux/amd64 or similar)
    if [[ ! -f "$BINARY" ]]; then
        echo "[deploy] no obc binary found — did any platform build succeed?" >&2
        exit 1
    fi

    RSYNC_SSH="ssh -i $KEY -o BatchMode=yes -o ConnectTimeout=10 ${PI_USER}@${PI_IP}"

    echo "[deploy] backing up running binary on ${PI_USER}@${PI_IP}"
    $RSYNC_SSH "sudo cp /usr/local/bin/obc /usr/local/bin/obc.prev" 2>/dev/null \
        || echo "[deploy] warning: no existing obc to backup (first deploy?)"

    echo "[deploy] deploying to ${PI_USER}@${PI_IP}"
    rsync -az --progress -e "$RSYNC_SSH" "$BINARY" "${PI_USER}@${PI_IP}:/tmp/obc"

    echo "[deploy] restarting obc service"
    $RSYNC_SSH "sudo mv /tmp/obc /usr/local/bin/obc && sudo systemctl restart obc"

    # Health check: wait for telemetry socket
    echo "[deploy] waiting for telemetry"
    for _ in $(seq 1 30); do
        if (echo > /dev/tcp/${PI_IP}/5556) 2>/dev/null; then
            break
        fi
        sleep 2
    done
    (echo > /dev/tcp/${PI_IP}/5556) 2>/dev/null \
        || { echo "[deploy] FAIL: telemetry socket never came back" >&2; exit 1; }

    echo "[deploy] one live frame: querying status..."
    go run ./tools/gs_cli \
        --control "tcp://${PI_IP}:5555" --telemetry "tcp://${PI_IP}:5556" \
        status

    echo "[deploy] ok -- previous binary kept at /usr/local/bin/obc.prev"
    echo "[deploy] rollback: scripts/deploy.sh ${PI_IP} ${PI_USER} --rollback"
fi

echo "[build] ok -> $ROOT/bin"
echo "Built platforms: ${BUILT_PLATFORMS[*]:-none}"
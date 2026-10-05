#!/usr/bin/env bash
#
# Compile and flash the RP2040 flight controller firmware.
#
# Usage:
#   scripts/flash-firmware.sh                # auto-detect the Pico
#   scripts/flash-firmware.sh /dev/ttyACM0   # explicit port
#   ARDUINO_PORT=/dev/ttyACM0 scripts/flash-firmware.sh
#   scripts/flash-firmware.sh --no-flash     # compile only
#
# The route is the one verified on the OBC (README "Firmware"):
#
#   1. compile with the rp2040 core for the Pico W -- NOT the mbed core;
#   2. open the port at 1200 baud and close it, which drops DTR and reboots
#      the board into the BOOTSEL bootloader;
#   3. load the UF2 with picotool (falling back to copying it onto the
#      RPI-RP2 mass-storage volume);
#   4. wait for the board to come back as a serial device.
#
# Every step that can hang carries its own timeout. An arduino-cli upload that
# waits forever for a volume that never appears is worse than one that fails,
# because it leaves the operator guessing whether to unplug the flight
# controller of an aircraft.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

FQBN="rp2040:rp2040:rpipicow"
BUILD_DIR="/tmp/pico-build"
PORT="${ARDUINO_PORT:-${1:-}}"
FLASH=true
[[ "${1:-}" == "--no-flash" ]] && { FLASH=false; PORT=""; }

command -v arduino-cli >/dev/null 2>&1 || {
    echo "[flash] arduino-cli is not installed" >&2
    exit 1
}

# find_pico_port prints the Pico's serial port, or nothing.
#
# The 2e8a vendor filter is the point: on the OBC the first tty* node is a
# PL2303 GNSS adapter, not the flight controller, and flashing whichever node
# happens to sort first is how a working board gets a firmware it cannot run.
find_pico_port() {
    local p
    for p in /dev/ttyACM* /dev/tty.usbmodem*; do
        [[ -e "$p" ]] || continue
        if udevadm info -q property -n "$p" 2>/dev/null | grep -q 'ID_VENDOR_ID=2e8a'; then
            echo "$p"
            return 0
        fi
    done
    # No vendor match: a bare board with only one candidate is still a Pico.
    set -- /dev/ttyACM*
    [[ -e "$1" && "$#" -eq 1 ]] && echo "$1"
    return 0
}

if [[ -z "$PORT" ]]; then
    PORT="$(find_pico_port)"
fi

echo "[flash] compiling firmware for the Pico W ($FQBN)"
timeout 600 arduino-cli compile --fqbn "$FQBN" --output-dir "$BUILD_DIR" firmware

if ! $FLASH; then
    echo "[flash] compile only; UF2 at $BUILD_DIR/firmware.ino.uf2"
    exit 0
fi

# bootsel returns success when the RP2 Boot USB device is present.
bootsel_present() {
    lsusb -d 2e8a:0003 >/dev/null 2>&1
}

if [[ -n "$PORT" ]]; then
    if fuser "$PORT" >/dev/null 2>&1; then
        echo "[flash] $PORT is held by another process (the OBC running?)" >&2
        exit 1
    fi
    echo "[flash] rebooting $PORT into the bootloader (1200-baud open)"
    if python3 -c 'import serial' 2>/dev/null; then
        timeout 10 python3 -c "
import serial, time
s = serial.Serial('$PORT', 1200)
time.sleep(0.5)
s.close()
"
    else
        # Without pyserial: setting the rate to 1200 and bouncing the fd does
        # the same thing -- the RP2040 core watches for a 1200-baud open.
        stty -F "$PORT" 1200 raw -echo
        timeout 5 bash -c "exec 3<>'$PORT'; sleep 0.5" || true
    fi
fi

echo "[flash] waiting for the BOOTSEL device"
for _ in $(seq 1 20); do
    bootsel_present && break
    sleep 0.5
done
if ! bootsel_present; then
    echo "[flash] no RP2 Boot device appeared; hold BOOTSEL while plugging in USB" >&2
    exit 1
fi

UF2="$BUILD_DIR/firmware.ino.uf2"
PICOTOOL="$(ls /root/.arduino15/packages/rp2040/tools/pqt-picotool/*/picotool \
              "$HOME"/.arduino15/packages/rp2040/tools/pqt-picotool/*/picotool \
              2>/dev/null | head -1 || true)"
PICOTOOL="${PICOTOOL:-$(command -v picotool || true)}"

if [[ -n "$PICOTOOL" ]]; then
    echo "[flash] loading $UF2 with picotool"
    timeout 120 "$PICOTOOL" load -f -x "$UF2"
else
    # Fallback: copy the UF2 onto the RPI-RP2 mass-storage volume. The
    # bootloader runs the image when the FAT write completes.
    echo "[flash] picotool not found; copying the UF2 to the RPI-RP2 volume"
    VOL="$(lsblk -rno NAME,LABEL,TYPE | awk '$3=="part" && $2=="RPI-RP2" {print "/dev/"$1; exit}')"
    if [[ -z "$VOL" ]]; then
        echo "[flash] no RPI-RP2 volume found (lsblk)" >&2
        exit 1
    fi
    MNT="$(mktemp -d)"
    mount "$VOL" "$MNT"
    cp "$UF2" "$MNT/"
    sync
    umount "$MNT"
    rmdir "$MNT"
fi

echo "[flash] waiting for the board to come back"
for _ in $(seq 1 20); do
    if ! bootsel_present && [[ -n "$(find_pico_port)" ]]; then
        break
    fi
    sleep 0.5
done
BACK="$(find_pico_port)"
if [[ -z "$BACK" ]]; then
    echo "[flash] firmware written, but no serial port came back" >&2
    exit 1
fi
echo "[flash] ok -- $BACK is up"

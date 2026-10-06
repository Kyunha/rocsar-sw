# ROCSAR

Autonomous antenna-pointing and motion-compensation for a gondola carried under
an airborne platform. Two antennas are kept pointed at a target bearing while the
platform underneath them rotates and is buffeted.

`ARCHITECTURE.md` is the binding design document and outranks this file.
`GUI_ARCHITECTURE.md` covers the operator console in detail. This README is
operational: what the pieces are, how to build and run them, and what the
non-obvious constraints are.

---

## The system

| Node | Hardware | Role |
| :--- | :--- | :--- |
| **OBC** | Raspberry Pi 4B | Process of record. Owns the SSD, the SDR, the camera, and the link to the operator. Binary: `cmd/obc`. |
| **Flight controller** | RP2040 Pico | Real-time control loop at 50 Hz. Owns the IMU, the servo bus and the heaters. `firmware/`. |
| **GS** | Operator laptop | Web console (`cmd/gs`) or terminal (`tools/gs_cli`). |
| **SDR** | Ettus B200mini | SAR acquisition, USB to the OBC. |
| **GNSS ×3** | u-blox receivers | Redundant positioning via `Read_uB` on the Pi. |

```
   GROUND STATION                    OBC (Pi 4B)                 FLIGHT CONTROLLER (RP2040)
   ┌────────────────┐   ZMQ DEALER   ┌────────────────┐  USB CDC   ┌──────────────────────┐
   │ gs / gs_cli    │ ─────────────▶ │ :5555 commands │  COBS      │ 50 Hz loop           │
   │                │ ◀───────────── │                │  protobuf │ BNO055 IMU  (I2C)   │
   │                │   ZMQ SUB     │ :5556 telemetry│  115200    │ 2× ST3215 servo (UART)│
   │                │ ◀───────────── │                │ ◀────────▶ │ 2× heater            │
   │                │     HTTP      │ :5557 artefacts│            └──────────────────────┘
   └────────────────┘ ◀───────────── └────────────────┘
                                          │  │  │
                          UDP :2001-2003  │  │  └── fswebcam ─▶ /dev/video0
                          GNSS ×3         │  └───── connect ──▶ B200mini (USB)
```

### Hard constraints

These are facts about the environment, not preferences. Designs that violate them
are wrong.

- **H1 — The link is 115 kbit/s** (≈14 kB/s), shared. Artefacts are referenced by
  name and size; bytes never travel in telemetry or a command reply.
- **H2 — The control loop runs on the Pico at 50 Hz**, not on the Pi. The Pi is not
  a real-time target and is not allowed into that loop.
- **H3 — If the OBC dies, the Pico keeps pointing.** The Pico holds the last valid
  bearing and runs its own loop, which is why it owns the kinematics.
- **H4 — The link may die.** The GS may vanish mid-flight. Nothing in the control
  path may depend on the GS being present.
- **H5 — Nothing is simulated unless explicitly asked for.** A subsystem running
  against a mock is named in the telemetry frame, so a fabricated reading can never
  be mistaken for a measurement.

---

## Prerequisites

| Tool | Needed for | Notes |
| :--- | :--- | :--- |
| Go 1.26+ | everything | `go.mod` pins the language version. |
| `libzmq` + headers | building OBC and GS | `go-zeromq/zmq4` is cgo. Debian/Ubuntu: `libzmq3-dev`. |
| Python 3 + `pytest` | firmware tests | `pyzmq`, `protobuf` for `gs_probe.py`. |
| `gcc` / `g++` | firmware tests | They compile `gondola_model.h` and link it against the real nanopb sources. Without a compiler those tests skip rather than fail. |
| `buf` + `protoc-gen-go` | regenerating protobuf | Only needed when a `.proto` changes. |
| `arduino-cli` | flashing firmware | Not needed to build or test the Go side. |
| Node + npm | building `gs` frontend | `cmd/gs` serves `frontend/dist`; see [Build](#build). |

The whole set is available through the committed dev shell:

```sh
nix-shell          # or: nix-shell --run '...'
```

`shell.nix` supplies Go, buf, Python with the test packages, Node and zeromq.
It drops you into `fish`. There is no browser package — the operator's browser is
a runtime fact, not a build dependency.

---

## Quick start

```sh
git clone <repo> && cd rocsar
nix-shell                                  # or install the prerequisites above

./scripts/build.sh                         # build every binary into bin/
./scripts/dev.sh                           # mock OBC + gs_cli watch on localhost
```

`./scripts/dev.sh` starts the OBC with all three subsystems simulated and follows
it with the console. It overrides the data root to `./data` (gitignored) because
`rocsar.toml` ships `http.root = "/mnt/ssd"`, which exists only on the Pi, and a
missing data root is a **fatal** startup error rather than something the OBC
creates for you. Ctrl-C stops both processes.

In a second terminal, without the helper:

```sh
mkdir -p data
ROCSAR_HTTP_ROOT="$PWD/data" go run ./cmd/obc --mock-pico --mock-camera --mock-sdr

go run ./tools/gs_cli \
  -control tcp://127.0.0.1:5555 \
  -telemetry tcp://127.0.0.1:5556 \
  -http http://127.0.0.1:5557 \
  watch
```

Both console defaults point at the aircraft (`192.168.1.50`), so the `-control` /
`-telemetry` / `-http` overrides are required for anything local.

---

## Build

```sh
./scripts/build.sh              # development build
./scripts/build.sh --release    # stripped, -trimpath, for deployment
```

Both write to `bin/`. `--release` adds `-trimpath -ldflags "-s -w"`.

The equivalent `go build` invocations, if you would rather not use the script:

```sh
mkdir -p bin
go build -o bin/obc           ./cmd/obc
go build -o bin/gs_cli        ./tools/gs_cli
go build -o bin/gs_probe      ./tools/gs_probe
go build -o bin/camera_bench  ./tools/camera_bench
go build -o bin/gnss_bench    ./tools/gnss_bench
go build -o bin/pico_bench    ./tools/pico_bench
go build -o bin/sdr_bench     ./tools/sdr_bench

# Release:
go build -trimpath -ldflags "-s -w" -o bin/obc ./cmd/obc
```

### Building the Ground Station

`cmd/gs/main.go` carries `//go:embed all:frontend/dist`, and an embed pattern that
matches nothing is a compile error. A fresh checkout therefore cannot build `gs`
until the frontend exists:

```sh
cd cmd/gs/frontend && npm install && npm run build && cd -
go build -o bin/gs ./cmd/gs
```

Build the frontend, then run the console:

```sh
cd cmd/gs/frontend && npm install && npm run build && cd -
go run ./cmd/gs             # prints a URL, open it in your browser
```

For frontend development with hot reload, run the Go server in one terminal
and Vite in another (Vite proxies `/ws` to Go):

```sh
go run ./cmd/gs &
cd cmd/gs/frontend && npm run dev
```

`scripts/build.sh` does the npm step for you when `npm` is present, and skips `gs`
with an explanatory message when it is not. Every other target builds with no Node
toolchain at all — there is no webview and no cgo in the GUI, so a plain
`go build ./...` never reaches a browser.

There is no build tag. The console is a plain `net/http` server; the browser is
the operator's. See `GUI_ARCHITECTURE.md` §12.

### Protobuf

One schema, three generators. Regenerate after changing any `.proto`:

```sh
./scripts/generate.sh
```

That runs `buf lint`, `buf build`, `buf generate` (Go + Python) and a separate
vendored-`protoc` invocation for the C target, then asserts four things that
otherwise fail silently. Equivalent raw commands:

```sh
buf lint
buf build -o /dev/null
buf generate                                     # Go -> api/, Python -> gs/

# C, firmware only, and it must run from api/ -- see below
cd api && ../third_party/nanopb/generator-bin/protoc -I . \
    --nanopb_out=OUT rocsar/v1/common.proto rocsar/v1/pico.proto
```

Three things about this that are not obvious:

- **The C target runs from `api/`, not the workspace root.** nanopb resolves
  `pico.options` relative to the *current working directory*, not the include path.
  When the options file is not found nanopb does not warn — it emits
  `pb_callback_t antennas;` instead of a static array, and the RP2040's RAM stops
  being budgetable at build time.
- **The firmware sees only `common.proto` and `pico.proto`.** `command.proto` and
  `telemetry.proto` are Ground Station schemas, and Arduino compiles every `.c`
  next to the `.ino` by proximity — so a GS message compiled in would land in the
  flight controller's flash. The script uses an explicit file list and fails if an
  unexpected `.c` appears in `firmware/`.
- **Go output is gitignored; Python and C output are committed.** The GS must
  install on a laptop with no toolchain, and the Arduino build compiles by folder
  proximity, so `firmware/*.c` has to exist on disk.

`./scripts/proto-check.sh` regenerates everything and then `git diff --exit-code`s
the result, which is the form that works in a pre-commit hook.

---

## Test

```sh
./scripts/test.sh
```

Raw:

```sh
go build ./...
go vet ./...
go test ./...
ruff check firmware/tests/
python -m pytest firmware/tests/ -v
```

| Suite | Covers |
| :--- | :--- |
| `test/` | Architecture boundaries, schema contracts, magic numbers, end-to-end command paths. The layering tests parse the import graph with `go/parser`, so a docstring mentioning `net` is not a violation and a function-local import is. |
| `internal/*/` | Per-package units. |
| `firmware/tests/` | Compiles `gondola_model.h` — the heading filter, tick maths, command handling and ST3215 framing — into a host binary and drives it, because that header includes no Arduino headers. Also checks the receive-buffer logic in `firmware.ino` by reading its source. The safety policies live here too: the boot interlock (`TestBootHold`, asserted from the transmit side), the heater dead-man timeline (`test_firmware_heater.py`, which also *reads* `internal/command/heater_keeper.go` and fails if two keep-alives no longer fit in the firmware's window), the IMU 50-miss counter (`TestMissThreshold`), and the non-finite-command guards. |
| `internal/client/live_test.go` | Against a real OBC. Skipped unless `ROCSAR_LIVE_OBC=1`. Read-only: telemetry and listing, no commands, no writes. |

The firmware tests skip rather than fail when no C/C++ compiler is present, so a
missing toolchain never reads as a broken build.

---

## Run

### OBC

```sh
go run ./cmd/obc [flags]
./bin/obc [flags]
```

| Flag | Default | Effect |
| :--- | :--- | :--- |
| `-config PATH` | `$ROCSAR_CONFIG`, else `./rocsar.toml` | Configuration file. |
| `-mock-pico` | off | Simulate the flight controller. |
| `-mock-camera` | off | Simulate the camera. |
| `-mock-sdr` | off | Simulate the SDR. |
| `-no-link-shaping` | off | Do not touch `tc`/netlink. |
| `-version` | — | Print the schema version and exit. |

Every flag defaults to `false`/`""` on purpose: a flag carrying its own default
cannot be distinguished from "nobody said anything", and would overwrite whatever
came from the file or the environment.

Missing hardware **degrades** rather than aborts — a telemetry server that refuses
to start tells the operator nothing — and every degraded subsystem is named in the
telemetry frame. Set `require_hardware = true` in `rocsar.toml` (or
`ROCSAR_REQUIRE_HARDWARE=true`) to make a missing device fatal instead, which is the
right posture for a bench and for CI. It is configuration only; there is no flag
for it, because the decision belongs to the deployment rather than to whoever
happens to type the command.

An unwritable or missing data directory is the one fatal-at-startup failure:
everything under it is flight data.

### Terminal console

```sh
go run ./tools/gs_cli [flags] <command>
./bin/gs_cli [command]
```

| Command | Does |
| :--- | :--- |
| `watch` | Follow telemetry, one block per frame. |
| `status` | Print one frame and exit. |
| `cmd <name> [args]` | Send one command. `gs_cli commands` lists them. |
| `commands` | List the command names `cmd` accepts. |
| `ls [path]` | List artefacts on the OBC. |
| `fetch <artefact>` | Download over HTTP, resuming if interrupted. |

| Flag | Default | |
| :--- | :--- | :--- |
| `-control` | `tcp://192.168.1.50:5555` | OBC ROUTER. |
| `-telemetry` | `tcp://192.168.1.50:5556` | OBC PUB. |
| `-http` | `http://192.168.1.50:5557` | Artefact server. |
| `-o PATH` | stdout | Write the fetched artefact here. |
| `-resume` | `true` | Continue a partial download. |
| `-raw` | `false` | Print the whole frame as JSON. |
| `-topic` | `telemetry` | Telemetry topic. |

Commands: `query`, `photo`, `gnss <id>`, `heading <deg>`, `jog <servo> <tick>`,
`zero [servo]`, `mount <servo> <deg>`, `dir <servo> ±1`, `heater <1|2> <on|off>`,
`stop <servo|all>`, `pico-status`, `sdr-probe`, `sdr-connect`, `sdr-reset-usb`,
`link <kbit>`, `reboot`.

There is deliberately **no arm gate** in `gs_cli`. It added a step between an
operator and a command without making anything safer, because the one consumer
that had it was a bench tool. The Wails window *does* require confirmation for
motion, for reasons that do not transfer to a terminal.

### Ground Station window

```sh
npm run dev                 # frontend dev server (hot reload)
go run ./cmd/gs             # Go server + bridge
```

Flags: `-control`, `-telemetry`, `-http`. The defaults point at the aircraft.
Configuration from `rocsar.toml` is not yet wired in — see `GUI_ARCHITECTURE.md`
§11 — so these flags are the whole story for now.

### Cross-language probe

Everything else in `tools/` is Go. These two are the only code that can prove
pyzmq and go-zeromq agree about the wire, which no amount of Go testing finds. The
Python one is the one that settles it.

```sh
python tools/gs_probe.py --listen                    # telemetry only
python tools/gs_probe.py                            # telemetry + one command
python tools/gs_probe.py --take-photo               # fetch the JPEG, verify SOF/EOI + Range

go run ./tools/gs_probe -frames 5 -seconds 10 -take-photo
```

Against a mock OBC: `./bin/obc --mock-pico --mock-camera --mock-sdr` in one
terminal, probe in another.

### Bench tools

Each talks to one piece of hardware directly, with no OBC involved:

```sh
go run ./tools/pico_bench   -port /dev/ttyACM0 -raw -seconds 10
go run ./tools/gnss_bench   -mode inject|listen|bank
go run ./tools/camera_bench -device /dev/video0 -out /tmp/shot.jpg
go run ./tools/sdr_bench    -program third_party/sdr-ettus-b200mini
```

`gnss_bench -mode inject` is the useful one on a laptop: it synthesises the
142-byte `UDP_message` the real receivers send, so the decoder can be exercised
with no sky view.

---

## Deploy

```sh
./scripts/deploy.sh <pi-ip> [ssh-user]     # default user: pi
./scripts/deploy.sh 192.168.1.50
```

Cross-compiles `obc` for `linux/arm64` with `-trimpath -ldflags "-s -w"`, rsyncs it
to `<host>:/tmp/obc`, then over ssh moves it to `/usr/local/bin/obc` and restarts
the systemd unit.

Equivalent by hand:

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=1 \
  go build -trimpath -ldflags "-s -w" -o bin/obc ./cmd/obc
rsync -az bin/obc pi@192.168.1.50:/tmp/obc
ssh pi@192.168.1.50 'sudo mv /tmp/obc /usr/local/bin/obc && sudo systemctl restart obc'
```

**CGO is the catch.** `go-zeromq/zmq4` links against `libzmq`, so a
`CGO_ENABLED=0` build produces a binary that compiles and then fails at startup.
Cross-compiling for the Pi needs a matching cross-toolchain and `libzmq` for
`arm64`; if you would rather not maintain one, build natively on the Pi:

```sh
./scripts/build-on-obc.sh 192.168.1.50 root    # sync, build all seven binaries on
                                               # the OBC, flash the flight
                                               # controller from its USB, verify
                                               # with one real telemetry frame
```

Configuration is deployed separately — `rocsar.toml` is committed, and
`rocsar.local.toml` is the gitignored per-machine overlay.

---

## Firmware

```sh
./scripts/flash-firmware.sh                    # auto-detects the port
ARDUINO_PORT=/dev/ttyACM0 ./scripts/flash-firmware.sh
```

Raw:

```sh
arduino-cli compile --fqbn rp2040:rp2040:rpipicow --output-dir /tmp/pico-build firmware

# Put the board in BOOTSEL (a 1200-baud open drops DTR and the core reboots
# into the bootloader), then copy the UF2 to the boot volume:
python3 -c "import serial,time; s=serial.Serial('/dev/ttyACM0',1200); time.sleep(0.5); s.close()"
cp /tmp/pico-build/firmware.ino.uf2 /dev/sdX1   # the 128 MB RPI-RP2 volume — check lsblk first
```

Port detection finds the Pico by its `2e8a` USB vendor id rather than by
taking the first serial node — on the OBC the first `tty*` is a GNSS adapter —
and `ARDUINO_PORT` overrides it when in doubt.
`scripts/flash-firmware.sh` drives the same route as the commands above:
`rp2040:rp2040:rpipicow` compile, a 1200-baud open to drop into BOOTSEL, then
`picotool load` of the UF2 (falling back to copying it onto the RPI-RP2
volume). The stale `arduino:mbed_rp2040:pico` FQBN it used to name is gone;
that core is not the one this tree builds with.

The sketch is split so the interesting half is testable: `gondola_model.h` holds
the kinematics, the heading filter, command handling and the ST3215 wire protocol
and includes no Arduino headers, so `firmware/tests/` compiles it on the host.
`firmware.ino` keeps only what needs pins, timers and the UART.

### What the firmware does without being asked

- **It boots silent.** Power-up transmits nothing to the servo bus until a
  `set_target` or `jog` arrives; `stop`, `zero`, `mount` and `dir` do not
  release the interlock, because none of them asks a servo to go anywhere.
  Before this, roughly four boots in five ran an axis to the rail within one
  control tick of power-up — `target 0` with a 270° mount offset and a 5:1 gear
  is reachable only for a 72° window of headings.
- **Heaters have a 20 s dead-man.** Any heater whose last on-command is 20 s
  old (`HEATER_AUTO_OFF_MS`) is cleared, whatever anyone intended. A running
  OBC re-sends an acknowledged on every 10 s (`HeaterRefreshInterval`), so a
  heater you asked for stays on, and killing the OBC, pulling the cable, or a
  Ground Station that never started all cool within one window. `off` is
  immediate and can never turn a heater on.
- **The IMU is reported absent after ~1 s of silence.** Fifty consecutive
  missed reads flip `imu_present` false (the bearing is held), and the 1 s
  re-probe then picks the sensor up again if it came back. One dropped read
  changes nothing.

Each of these has a host test in `firmware/tests/` — see `TestBootHold`,
`test_firmware_heater.py` and `TestMissThreshold`.

Two buses both run at 115200 and are unrelated — the ST3215 servo UART and the
USB CDC link to the Pi. They are separate named constants with a comment at each
site, because a reader who has seen one `115200` has no way to know the other is a
different wire.

---

## Configuration

`rocsar.toml`, read by both the OBC and the GS. Resolution order, highest first:

```
explicit flag  >  ROCSAR_* environment  >  rocsar.toml  >  code default
```

| Key | Default | Meaning |
| :--- | :--- | :--- |
| `server.control_endpoint` | `tcp://*:5555` | ZMQ ROUTER bind. |
| `server.telemetry_endpoint` | `tcp://*:5556` | ZMQ PUB bind. |
| `client.control_endpoint` | `tcp://127.0.0.1:5555` | GS → OBC commands. |
| `client.telemetry_endpoint` | `tcp://127.0.0.1:5556` | GS → OBC telemetry. |
| `http.addr` | `:5557` | Artefact server. |
| `http.root` | `/mnt/ssd` | Data directory. Must exist. |
| `link.device` | `eth0` | Interface for HTB shaping. |
| `link.rate_kbps` | `115` | Link cap. A property of the radio, not a knob. |
| `link.shaping` | `false` | Install the HTB hierarchy. Needs `CAP_NET_ADMIN`. |
| `pico.port` | `/dev/ttyACM0` | Flight controller serial port. |
| `pico.baudrate` | `115200` | USB CDC rate. Not the servo bus. |
| `gnss.ports` | `[2001, 2002, 2003]` | One per `Read_uB` instance. |
| `gnss.selected` | `1` | Trusted receiver, by 1-based position in `ports`. |
| `gnss.stale_after` | `2s` | Fix age past which `fix_ok` goes false. |
| `camera.device` | `/dev/video0` | The node `fswebcam` reads. |
| `sdr.program` | `third_party/sdr-ettus-b200mini` | Directory holding `parameters/` and `Data/` — **not** the `connect` binary, which is found on `PATH`. Must be named `sdr-ettus-b200mini`; see below. |
| `telemetry.interval` | `1s` | Frame rate. |
| `qos.bulk_rate_bps` | `8192` | Artefact download ceiling. `0` = unbounded. |
| `require_hardware` | `false` | Missing hardware is fatal instead of degraded. |

Every key has an environment form: uppercase, dots to underscores, `ROCSAR_` prefix.
`http.root` → `ROCSAR_HTTP_ROOT`, `gnss.stale_after` → `ROCSAR_GNSS_STALE_AFTER`.

```sh
export ROCSAR_HTTP_ROOT="$PWD/data"
export ROCSAR_REQUIRE_HARDWARE=true
export ROCSAR_LINK_RATE_KBPS=115
```

Three rules that are load-bearing:

- **Defaults are not written into `rocsar.toml`.** A default in the file is a
  second home for a fact and two homes drift. `obc --help` lists them. An empty
  file is the correct state on a fresh checkout.
- **An unrecognised key is ignored, not an error**, so a newer GS carrying a key
  this OBC predates does not stop it from starting. A version skew should not
  become an outage. Typo'd keys are reported at startup so they are still visible.
- **`rocsar.local.toml` is merged over `rocsar.toml` and is gitignored.** Use it
  for a different Pi address or data directory so the override does not become a
  commit that fights the next person.

### The SDR program on the Pi

The acquisition program is **installed on `PATH`**, not shipped beside the OBC
binary. That is what lets a compiled `obc` be `scp`'d onto the Pi and drive a
program that was built and installed separately:

```sh
ln -s /root/rocsar-rpi/sdr-ettus-b200mini/connect /usr/local/bin/connect
export ROCSAR_SDR_PROGRAM=/root/rocsar-rpi/sdr-ettus-b200mini
```

Two things about that value, both of which look like pedantry and are not:

- **It is a directory, not a program.** It names where `parameters/params.json`
  and `Data/` live, and it is the working directory the program is started in.
  `connect` itself is found on `PATH`, with a fallback to `<that dir>/connect`
  so a fresh checkout still works with nothing installed.
- **It must be called `sdr-ettus-b200mini`.** `connect.cpp` hardcodes
  `load_config("./../sdr-ettus-b200mini/parameters/params.json")`, which resolves
  against the working directory and therefore only lands on
  `<that dir>/parameters/params.json` when the directory has that name. Point it
  anywhere else and the OBC writes parameters to a file the program never reads:
  the Ground Station reports the edit as saved and the capture keeps using the
  old values. The OBC refuses to start an acquisition in that case rather than
  flying a silent mismatch, and `sdr_bench validate` says the same thing on the
  bench.

---

## Scripts

| Script | Purpose |
| :--- | :--- |
| `./scripts/build.sh [--release]` | Build every binary into `bin/`. Handles the frontend for `gs`. |
| `./scripts/test.sh` | `go build`, `go vet`, `go test ./...`, firmware `pytest`. |
| `./scripts/dev.sh` | Mock OBC + `gs_cli watch` on localhost, Ctrl-C to stop both. |
| `./scripts/deploy.sh <ip> [user]` | Cross-compile for arm64, rsync, restart the systemd unit. |
| `./scripts/lint.sh` | `go vet`, `ruff`, `buf lint`. |
| `./scripts/clean.sh` | Remove `bin/`, generated protobuf, `__pycache__`, tool caches. |
| `./scripts/flash-firmware.sh` | `arduino-cli compile` + `upload` to the RP2040. |
| `./scripts/proto-check.sh` | Regenerate, then `git diff --exit-code` the generated tree. |
| `./scripts/generate.sh` | Regenerate all three protobuf targets and verify them. |

All of them resolve the repository root from their own location, so they work from
any working directory.

---

## Architecture

```
transport  →  command  →  domain  →  Port (interface)  →  adapter (real | mock)
```

One direction. `internal/domain` and `internal/telemetry` import nothing from
outside the standard library and nothing from `api/rocsar/v1` — they do not know
protobuf exists. Hardware and wire formats are declared as interfaces in the
package that consumes them, so the dependency points from user to implementation.
Every port has both a real adapter and a mock, and a test asserts both exist.

This is enforced mechanically by `test/layering_test.go`, which walks the import
graph with `go/parser` rather than by review. Discipline decays; that test does
not.

| Link | Transport | Port | Wire |
| :--- | :--- | :--- | :--- |
| Commands | ZMQ ROUTER / DEALER | 5555 | `[identity, CommandRequest]` → `[identity, CommandResponse]` |
| Telemetry | ZMQ PUB / SUB | 5556 | `[topic, TelemetryFrame]` |
| Artefacts | HTTP | 5557 | JSON listing; bytes with `Range`/`206` |
| Pico | USB CDC, COBS + protobuf | `/dev/ttyACM0` | `PicoMessage`, `0x00` delimited |

`cmd/obc/main.go` and `cmd/gs/main.go` are composition roots and nothing else:
they are the only places that decide whether a subsystem is real or a double.

Full detail in `ARCHITECTURE.md` and `GUI_ARCHITECTURE.md`. Where code and those
documents disagree, the code is wrong.

---

## Key takeaways

- **The link is 115 kbit/s and that shapes everything.** Artefacts go over HTTP
  with resume, never through ZeroMQ. A multi-megabyte JPEG in a command response
  would go through the priority class and starve commanding. Telemetry has a hard
  **2048-byte budget** (it currently serialises to ~400), asserted by a test,
  because the priority class is 41 kbit/s and a frame that outgrows it starves
  commands.
- **Exactly one telemetry topic exists: `telemetry`.** `TelemetryFrame` is one
  message carrying the whole 1 Hz picture. Five per-subsystem topics were described
  in an earlier revision of the architecture and never implemented — a client built
  from that document subscribes to five, matches none, and reports a dead OBC,
  because a ZeroMQ filter mismatch and a dead OBC are indistinguishable from the
  client side.
- **The control loop is on the Pico at 50 Hz.** If the OBC dies the antennas keep
  pointing. The Pi is not allowed into that loop.
- **A board that boots, boots silent.** Power-up transmits nothing to the servo
  bus until `set_target` or `jog` says otherwise. The default heading (`0°`) is
  reachable from only a 72° window of bearings, so the pre-change firmware ran
  an axis to the rail within one tick of power-up in roughly four boots out of
  five — with a pinned tick and no alarm as the only visible hint. Silence is
  the only safe default when "do nothing" requires a command to express.
- **The heater dead-man is two halves that only work as a pair.** The firmware
  clears any heater not refreshed within 20 s; the OBC re-sends an acknowledged
  on every 10 s. Every way the host half can fail — crash, cut cable, reboot —
  leaves the firmware half to do the turning off. The intent is asymmetric on
  purpose: an ON is kept alive only once the board acknowledged it, an OFF is
  recorded even when the round trip fails, because dropping a keep-alive can
  only cool. A Python test reads the Go constant so the two halves cannot drift.
- **`feedback_state` and `imu_present` are not decoration.** A servo that stopped
  answering and one reporting a genuine zero load must be distinguishable, or a
  console says "0 A" for a motor that has not moved. Earlier firmware showed a
  healthy-looking `0` in exactly that case.
- **Nothing is simulated unless asked, and mocks are named on the wire.** A system
  that fabricated a reading because a device was not found is the worst failure
  mode this project has; hiding it in a log nobody has open is not disclosure.
- **Link shaping caps the link but does not classify it.** The HTB hierarchy works
  — measured on the target at a 41 kbit/s floor with a 115 kbit/s ceiling — but the
  `tc flower` filter that was meant to steer artefact downloads into a bulk class
  never worked, after three separate fixes. Bulk downloads are bounded in-process
  by `qos.bulk_rate_bps` instead. Proper per-class constraint is deferred to its
  own module rather than declared finished. Read `internal/qos/shaper.go` before
  re-adding a filter.
- **DEALER, not REQ.** The OBC's ROUTER sends `[identity, payload]` with no empty
  delimiter; REQ always inserts one, so the reply arrives a frame late. The Go
  test client hand-rolled the REQ envelope for a while, so the tests passed and the
  real Python GS was silently dropped. `tools/gs_probe.py` exists because of it.
- **A SUB that connects mid-stream gets the backlog.** Discard the first 5 frames
  after subscribing or a rate measurement reads ~10× true, then goes silent.
- **Coordinates are degrees.** The C producer converts before the datagram leaves
  `Read_uB`; a consumer that converts again is wrong by a factor of 57.3 while
  still looking plausible. That bug shipped. Every binary contract in this repo now
  has a test that decodes real bytes.
- **Every binary contract has a test that decodes real bytes.** A document
  describing a wire format without one is a guess. `NOTES.md` was deleted for
  exactly this reason and its contracts moved into `firmware/` and `test/`.
- **No CI by design.** The hardware and the live link are the specification; an
  automated suite cannot tell you a GNSS fix is real. The tests here pin contracts,
  boundaries and framing, and stop there.
# ROCSAR System Architecture

**Binding document.** This is the contract the code is written against. Where the
code and this document disagree, the code is wrong — fix the code, or change
this document in the same commit that changes the behaviour.

## 0. Precedence

This document outranks every other file in the repository, with one exception:
`GUI_ARCHITECTURE.md` **amends** this document for the Ground Station console. The
console is specified there in detail — its link library, its settings, its
interaction model — and this document defers to it on those points rather than
duplicating and contradicting it. Every subsystem, link and constraint owned by the
OBC is governed here and is not negotiable from there.

`NOTES.md`, named in earlier revisions as the authority on *frozen binary
contracts*, does not exist (§2). The contracts it would have held are in
`firmware/` (§4.4) and are pinned by tests that decode real bytes, which is the
stronger guarantee: the file could describe the struct, the test can prove the
consumer reads it correctly.

The previous `NOTES.md` in `obc_rocsar/` was wrong in a way that shipped a broken
product: it documented GNSS latitude and longitude as arriving in radians when
the C++ producer had already converted them to degrees, and the consumer
converted a second time. Every position the operator ever saw was garbage. A
document that describes a wire format without a test that reads the real bytes is
a guess. So: **every binary contract in this repository has a test that decodes
real bytes.**

---

## 1. What ROCSAR is

ROCSAR is an autonomous antenna-pointing and motion-compensation system for a
gondola carried under an airborne platform. The requirement is that two antennas
stay continuously pointed at a target bearing while the platform underneath them
rotates and is buffeted.

Four computers and one radio link are involved:

| Node | Hardware | Role |
| :--- | :--- | :--- |
| **OBC** | Raspberry Pi 4B | Process of record. Owns the SSD, the SDR, the camera, and the link to the operator. |
| **Flight controller** | RP2040 Pico | Real-time control loop at 50 Hz. Owns the IMU, the servo bus and the heaters. |
| **GS** | Operator laptop | Web console (`cmd/gs`, Go server + browser) or terminal (`tools/gs_cli`). Telemetry display and command surface. |
| **SDR** | Ettus B200mini | SAR acquisition, USB to the OBC. |
| **GNSS ×3** | u-blox receivers | Redundant positioning, `Read_uB` on the Pi. |

```
+---------------------------------------------------------------------------+
|  GROUND STATION  (Go console on the operator laptop)                      |
|    ZMQ DEALER -> :5555        ZMQ SUB <- :5556        HTTP <- :5557       |
+---------------------------------------------------------------------------+
                                   |
                    Ethernet, shaped by HTB (rtnetlink) to 115 kbit/s
                                   |
+---------------------------------------------------------------------------+
|  OBC  (Raspberry Pi 4B, Go)                                              |
|                                                                           |
|    +-----------+   +-----------+   +-----------+   +-----------+          |
|    | transport |   | telemetry |   |    qos    |   |  storage  |          |
|    +-----------+   +-----------+   +-----------+   +-----------+          |
|         |               ^               ^               |                 |
|    +----+---------------+---------------+---------------+----+            |
|    |                     domain (types + Ports)                |            |
|    +----+---------------+---------------+---------------+----+            |
|         |               |               |               |                 |
|    +--------+     +--------+     +--------+     +---------+ +--------+     |
|    |  gnss  |     |  pico  |     | camera |     |   sdr   | |   qos   |     |
|    +--------+     +--------+     +--------+     +---------+ +--------+     |
|         |              |              |              |                     |
|   UDP :2001-3     USB serial     fswebcam       USB + connect (on PATH)    |
+---------------------------------------------------------------------------+
                                   |
                     USB CDC, COBS-framed protobuf, 115200
                                   |
+---------------------------------------------------------------------------+
|  FLIGHT CONTROLLER  (RP2040, Arduino .ino + nanopb)                      |
|    50 Hz loop | BNO055 IMU on I2C | 2× ST3215 servos on UART | heaters   |
+---------------------------------------------------------------------------+
```

### 1.1 Hard constraints

These are facts about the environment, not preferences. Designs that violate them
are wrong.

- **H1 — The link is 115 kbit/s** (≈14.4 kB/s), a shared radio link. This is why
  artefacts are *referenced by name and size*, never carried in telemetry.
- **H2 — The control loop runs on the Pico at 50 Hz**, not on the Pi. The Pi is
  not a real-time target and is not allowed into that loop.
- **H3 — If the OBC dies, the Pico keeps pointing.** The Pico holds the last
  valid bearing and runs its loop. This is why the Pico owns the kinematics.
- **H4 — The link may die.** The GS may vanish mid-flight. Nothing in the control
  path may depend on the GS being present.
- **H5 — Nothing is simulated unless explicitly asked for.** See §9.

---

## 2. Repository layout

```
rocsar/
├── ARCHITECTURE.md          ← this document
├── GUI_ARCHITECTURE.md      ← the Ground Station console, in detail. §14 notes where it amends this document.
├── go.mod                   ← module github.com/rocsar/obc
├── rocsar.toml              ← configuration, read by the OBC and the GS
├── buf.yaml                 ← buf workspace
├── buf.gen.yaml             ← three generators, one schema
├── api/
│   ├── buf.yaml             ← module `rocsar`, root here
│   └── rocsar/v1/*.proto    ← THE schema. one source of truth.
├── internal/
│   ├── domain/              ← types and Port interfaces. imports nothing external.
│   ├── gnss/                ← UDP receiver bank
│   ├── pico/                ← COBS + protobuf + serial link to the flight controller
│   ├── camera/              ← fswebcam snapshot
│   ├── sdr/                 ← params.json, connect lifecycle, probe, USB reset
│   ├── storage/             ← data directory, atomic placement
│   ├── telemetry/           ← 1 Hz aggregate assembly. no I/O.
│   ├── qos/                 ← HTB link shaping over rtnetlink. no userspace queue.
│   ├── transport/           ← ZMQ + HTTP, and the artefact download limiter (§6.6)
│   └── client/              ← the GS link library: subscribe, command, artefacts
├── firmware/                ← the .ino, gondola_model.h, pico_wire.h, generated C
├── third_party/
│   ├── Read_uB/             ← vendored, unmodified. §10.
│   └── sdr-ettus-b200mini/  ← vendored, unmodified. §10.
├── gs/                      ← Python: generated protobuf stubs only (§14)
├── tools/                   ← gs_cli (the GS console), camera_bench, gs_probe
└── test/                    ← architecture boundary tests
```

Generated Python code is committed (`gs/rocsar/v1/*_pb2.py`) so that a Python
subscriber can be installed on an operator laptop without a toolchain. The Ground
Station itself is Go and needs no generated code at runtime.

`NOTES.md` and `README.md` are listed in earlier revisions of this document and do
not exist. The binary contracts they were meant to hold are in `firmware/` (§4.4)
and are pinned by tests in `test/` (§12), which is the stronger guarantee; build and
run instructions live in the repository's commit history and in `scripts/` until the
two files are written.

---

## 3. The layering rule

```
transport  →  command  →  domain  →  Port (interface)  →  adapter (real | mock)
```

One direction. Never the reverse. Concretely:

- `internal/transport` may call `domain.CommandHandler`. It may not open a serial
  port, spawn a process, or touch a camera.
- `internal/client` is the Ground Station's mirror of `internal/transport`: it owns
  the wire for the GS side, and it may not reach into any subsystem adapter.
- `internal/domain` and `internal/telemetry` import **nothing** from outside the
  standard library and nothing from `api/rocsar/v1`. They do not know that
  protobuf exists.
- Hardware and wire formats are declared as interfaces **in the package that
  consumes them**, so the dependency points from user to implementation.
- `os/exec` appears in exactly two packages: `internal/sdr` and `internal/camera`.
  This section previously named `internal/qos`, which no longer shells out at all
  (§6.6).

This is enforced mechanically, not by discipline. See §12.

### 3.1 Ports

`internal/domain` declares one interface per external dependency. Each has a
real implementation and a mock; a test asserts both exist.

| Port | Real adapter | Mock |
| :--- | :--- | :--- |
| `Pico` | `pico.Link` | `pico.Mock` |
| `Camera` | `camera.Capture` | `camera.Mock` |
| `Sdr` | `sdr.Service` | `sdr.Mock` |
| `LinkShaper` | `qos.Shaper` | `qos.NullShaper` |

`qos.TcShaper` was the type name while the adapter ran `tc`. There is no `tc` in
`qos` now (§6.6), so the adapter is `qos.Shaper` and the netlink calls behind it
are `qos.NetlinkOps`.
| `Shutdown` | `pico.Link` | *(none — see below)* |

There is no `GnssReceiver` port. It was declared, nothing consumed it — the bank
holds concrete `*gnss.Receiver` — and an interface with one implementation is a
class with no seam. It went when the GNSS mock did.

`Shutdown` is excluded from the mock rule: it is a lifecycle constraint, not a
hardware port. There is nothing to disagree about — either a type can be closed
or it cannot — and a `MockShutdown` that recorded `Close` calls would be theatre.

---

## 4. The schema

One protobuf package, `rocsar.v1`, in `api/rocsar/v1/`. It serves **both** links.
This is a deliberate departure from ROCSAR_PI, which kept `pico_protocol.proto`
and `groundlink.proto` separate on the reasoning that the firmware must never
learn a ground-station concept. The reasoning was sound; the separation was
maintenance burden paid for a risk that the nanopb generator configuration
already handles (§4.3). Here the firmware target is a *separate generation step*
with a *separate options file*, so ground-station messages are never compiled
into the RP2040 binary even though they share a package.

### 4.1 Files

| File | Contents |
| :--- | :--- |
| `common.proto` | `ErrorCode`, `FeedbackState`, `SubsystemState`, `AntennaAxis` |
| `command.proto` | Every command the GS can send, as one `Command` with a `oneof` |
| `telemetry.proto` | `TelemetryFrame` — the 1 Hz aggregate |
| `pico.proto` | `PicoMessage` (the COBS payload), `PicoCommand`, `PicoTelemetry`, `PicoAck` |

### 4.2 The Pico envelope

The Pico link payload is `PicoMessage`. It is the same shape the shipped firmware
already uses, deliberately: `sequence` + `timestamp_us` + a `oneof` of
`telemetry` or `response`. Keeping it means the existing `.ino` and
`gondola_model.h` port with mechanical changes only.

### 4.3 Generation

```
buf generate
  ├── go       → api/rocsar/v1/*.pb.go            (OBC)
  └── python   → gs/rocsar/v1/*_pb2.py            (Python subscribers, committed)

scripts/generate.sh, firmware target only (RP2040):
  └── nanopb   → firmware/common.pb.{c,h}, firmware/pico.pb.{c,h}
```

The nanopb plugin is the **vendored binary** at
`third_party/nanopb/generator-bin/protoc-gen-nanopb`, referenced by path in
`buf.gen.yaml`, not fetched from the BSR. Two reasons: the build works offline,
and nanopb is pinned to the exact version the firmware was validated against.

Two options files:

- `pico.options` — nanopb, referenced by path from the generation script. It caps
  repeated fields (`PicoTelemetry.antennas max_count:2`); without it the generated
  C struct grows without bound and the RP2040 runs out of RAM.
- *(no GS options file needed — the GS is Go; the Python side uses generated stubs.)*

The nanopb generation runs a **separate buf invocation** over a restricted file
set (§4.4), so `command.proto`'s ground-station messages cannot reach the
firmware even by accident.

### 4.4 What the firmware is allowed to see

The RP2040 build consumes only:

```
rocsar/v1/common.proto      (ErrorCode, FeedbackState, AntennaAxis)
rocsar/v1/pico.proto        (PicoMessage, PicoCommand, PicoTelemetry, PicoAck)
```

`rocsar/v1/command.proto` and `rocsar/v1/telemetry.proto` are **excluded** from
firmware generation. The generation script lists the firmware protos explicitly and
fails if an unexpected `.c` file appears in `firmware/`. An earlier revision of this
document also claimed a Go test grepped the generated C for ground-station type
names; no such test exists, and the script-level check is what is actually enforced.

---

## 5. Links

### 5.1 Ground Station ↔ OBC

| Direction | Socket | Port | Content |
| :--- | :--- | :--- | :--- |
| Commands | ZMQ ROUTER (bind) / DEALER (dial) | `tcp://*:5555` | `[identity, CommandRequest]` → `[identity, CommandResponse]` |
| Telemetry | ZMQ PUB (bind) / SUB (dial) | `tcp://*:5556` | `[topic, TelemetryFrame]` |
| Artefacts | HTTP | `tcp://*:5557` | JSON directory listing; file bytes with `Range`/`206` |

Protobuf on both ZMQ sockets. JSON only inside the HTTP directory listing,
because that is the one payload a human reads with `curl`.

**ROUTER, not REP.** `REP` requires strict alternation and holds state, so a
second operator connecting breaks the first. `ROUTER` returns the sender's
identity in every frame, which is what lets a reply go back to whoever asked.

**`request_id`.** Every `CommandRequest` carries a `request_id` chosen by the
GS. Every `CommandResponse` echoes it. Without it, a reply cannot be matched to
the button that caused it. The Pico leg adds a second, independent `uint32
sequence` (§5.2), because the Pico is a different machine with its own counter.

**Topic frame on PUB.** Telemetry is `[topic, frame]`. ZeroMQ `SUBSCRIBE` filters
on frame prefix, so putting the topic in its own leading frame makes the filter
exact.

**Exactly one topic is published: `telemetry`.** An earlier revision of this
document described five topics (`telemetry.system`, `telemetry.gnss`,
`telemetry.pico`, `telemetry.sdr`, `telemetry.camera`) and said the topic let a GS
subscribe to a subset. That is not what the code does and it is not what the
aggregate frame is for: `TelemetryFrame` is one message carrying the whole 1 Hz
picture, so there is nothing to subscribe to selectively. `qos.AllTopics()` returns
`["telemetry"]` and `cmd/obc` publishes only that.

`qos` declares a second constant, `TopicControl` = `control.response`, which no
publisher or subscriber references anywhere in the tree. It is left in place rather
than deleted because it names a topic the command path is *intended* to need, and
deciding whether commands belong on the PUB socket is a design question, not a
cleanup. Until something publishes or subscribes to it, it is inert: it is not on
the wire and nothing depends on it.

**Slow joiner.** A SUB that connects mid-stream receives the queued backlog. A GS
measuring rate must discard its first `SLOW_JOINER_FRAMES` frames or it will
report ~10× the true rate followed by silence. This is a property of the
protocol, not of the software; the GS client implements the discard.

**HTTP.** `GET /` lists a directory as JSON. `GET /<path>` returns bytes and
honours `Range`/`206` so a download can resume. Paths are confined to the data
directory — a request that escapes it after cleaning is a `403`, never a read.

### 5.2 OBC ↔ Pico

| Property | Value |
| :--- | :--- |
| Physical | USB CDC serial, `/dev/ttyACM0` |
| Rate | 115200 baud |
| Framing | COBS, `0x00` delimiter |
| Payload | `PicoMessage` (protobuf, `rocsar.v1`) |
| Direction | OBC sends `PicoCommand`, receives `PicoAck` and `PicoTelemetry` |

**COBS, not length-prefixing, not ASCII.** COBS was chosen because the payload is
binary and a delimiter-based ASCII protocol would need escaping; COBS removes
every `0x00` from the payload so `0x00` is unambiguous as a delimiter. The RP2040
side already implements it in `firmware/cobs.c`.

**Two independent sequence numbers.** The `PicoCommand.sequence` is generated by
the Pico link and echoed in `PicoAck.command_sequence`. A command that is not
acknowledged within `PicoAckTimeout` is a timeout, and the link counts it as
unsolicited traffic. This is how the OBC learns the flight controller has gone
silent — see §8.

**Three baud rates, one coincidence.** `SERVO_BAUD` (115200) is the ST3215 servo
UART. `PICO_BAUD` (115200) is the USB link to the Pi. `GNSS_BAUD` (57600) is the
u-blox receiver. Two of them are the same number and they are unrelated. They are
separate constants in separate packages and a comment at each site says so.

---

## 6. Subsystems

### 6.1 GNSS

Three `Read_uB` instances send a 142-byte `UDP_message` to `127.0.0.1` on ports
`2001`, `2002`, `2003`. The layout is frozen and pinned by `test/gnss_test.go`,
which decodes the real 142 bytes (§0).

**Units.** Latitude, longitude and heading arrive in **degrees**. The producer
converts before sending. The decoder must not convert again. This is stated here
because it was the exact bug that shipped.

**Validation.** A datagram is accepted only if it is exactly 142 bytes, starts
with `0xAA`, ends with `0x99`, and every `float64` is finite. A coordinate outside
±90 / ±180 is rejected. Publishing `(lat=90.0, lon=1e300)` is worse than
publishing nothing, because the GS cannot tell it from a real fix.

**The bank.** `gnss.Bank` holds N receivers, exactly one of which is *selected*.
Selection is explicit — the first receiver by default, changeable by command,
never automatic. Every receiver carries its own health (`lastFix`, `age`,
`packetCount`, `rejectCount`) so the operator can see all three and know which one
is trusted. The selected receiver's fix goes into telemetry; the others do not.

**Staleness.** A fix older than `gnss_stale_after` is reported with
`fix_ok=false` even though the last decoded value was valid. An old fix is worse
than no fix. The threshold is the `gnss.stale_after` config key (default `2s`);
this section previously called it `gnss_stale_after`, which is not a key that
exists.

**A second datagram exists and is out of scope.** `Read_uB.cpp:357` also sends a
420-byte `#pragma pack(1) NavData` on a separate socket, carrying `flags`,
`stage`, flight-trend bits and predicted landing points. It is richer. We do not
read it. It is recorded here so its existence is not rediscovered as a mystery.

### 6.2 Pico / flight controller

`pico.Link` owns the serial port, the COBS framing, the protobuf codec, the
command sequence counter, and ACK correlation. Telemetry fan-out is a callback
list.

There is **no automatic reconnect loop inside `Link`.** Reconnection is a decision
the composition root makes, because a link that reconnects on its own turns a
recoverable absence into an invisible retry storm that hides the fault.

**The firmware's safety policy lives in `gondola_model.h`, not the sketch.** The
sketch calls it at the right moments; the policy itself is host-testable, which
is why every rule below has a test in `firmware/tests/` rather than a note in a
commit (§12).

**The servo holds its own centre, and `zero` writes it.** The centre tick used to
be a firmware variable that `zero` overwrote with the axis's current reading. Two
things were wrong with that. It was lost on every power cycle. And it could be
set from a position that had never been measured: the old code read `currentTick`
unconditionally, which is an encoder reading only when `feedbackState` is
`MEASURED` and otherwise a command echo — so `zero` on an axis whose servo had
never answered adopted the last commanded tick as the mechanical centre and
reported success.

Now the ST3215 holds it. `zero` writes a position offset into the servo’s EEPROM
(register `0x1F`) so the encoder reports 2048 at the boresight. The sequence —
unlock `0x37`, write, re-lock, 20 ms settle either side — is the vendor’s, taken
from `docs/ST3215_Configure/ST3215_Configure.ino`. What is ours is the
verification: the firmware reads the register back *and* reads the position back,
and refuses with `ERROR_CALIBRATION_FAILED` unless both agree. That second check
is not belt-and-braces. It is the thing that detects the documented-but-unverified
assumption that `0x1F` shifts the reported position at all rather than only the
commanded one, and it turns a servo pointing somewhere plausible into an
acknowledgement that says the teach failed.

Three consequences, each recorded where it is enforced:

- **It is idempotent.** The offset is always recomputed from wherever the servo
  actually is, so a run that half-failed — a write lost to bus contention, a
  servo pulled mid-sequence — converges on a second run. There is no reset
  command and no recovery procedure, because re-running `zero` *is* the recovery
  procedure.
- **The centre survives a reflash, so the firmware records only that it was
  taught.** An axis taught while sitting at exactly 2048 stores an offset of
  zero, which is indistinguishable from one never taught. That fact lives in the
  Pico’s own EEPROM (`calibration.h`, 14 bytes, CRC’d) and rides the wire as
  `AntennaTelemetry.center_zeroed`. Without it, a fresh servo and a centred one
  report the same 2048 and the console cannot tell them apart.
- **Register `0x28` (torque) is never written.** A third-party table claims
  EEPROM writes require torque disabled; the vendor’s tool does not do that, and
  torque-off on a 5:1 gear reduction means the antenna drops. The read-back
  settles the question per write, so the firmware does not write it and find out.

`zero` is consequently the one command that is not finished by
`applyCommand()`, which cannot know whether a bus write landed. Its policy is
three functions — `validateZeroCommand()`, `judgeServoZero()`, `commitServoZero()`
— with the bus work between the second and the third, and there is deliberately no
`zero` case left in `applyCommand()` that could set a centre without a verified
read-back. It is also the slowest command on the aircraft, 100–250 ms during which
the control loop does not tick, which is why `DefaultAckTimeout` is documented
against it rather than against the single-digit-millisecond round trip everything
else gets.

**A board that boots, boots silent.** Power-up holds both axes in manual mode at
centre with `awaitingCommand` set, and `shouldDriveServo()` — the interlock plus
the deadband, one predicate so the sketch cannot apply one and forget the other —
transmits nothing until `set_target` (releases both axes) or `jog` (releases that
axis only). `stop`, `zero`, `mount` and `dir` deliberately do not release it:
none of them asks a servo to go anywhere, so none of them may move an axis that
has never been commanded. A teach is in that list even though it writes to
hardware, because it relabels the position the axis is already holding rather
than asking for a new one. This replaced a boot that drove to the rail.
`target_heading_deg` boots at 0, and with the 270° mount offset and 5:1 gear that
target is reachable only for gondola headings in a 72° window — four boots in
five ran an axis to the clamp within one control tick of power-up, with a tick
pinned at 4095 and no alarm behind it as the only hint. The interlock does not
swallow the first command: `lastSentTick` is still the `0xFFFF` never-sent
sentinel when it clears, and every tick differs from `0xFFFF` by more than the
deadband, so the first commanded position always reaches the bus even when it
computes to centre. That sentinel is also never published as a position: while
feedback is still `UNKNOWN`, the fallback reports the centre tick instead of
65535, because `0xFFFF` as `current_tick` reads as ~5580° off centre with
`feedback_state` as the only hint that it is fiction.

**The heater dead-man is two halves, and only the pair works.**
Firmware half: `HEATER_AUTO_OFF_MS` (20 s). `expireHeaters()` clears any heater
whose last on-command is older than the window, every control tick, whatever
anyone intended; `syncHeaterPins()` is the only writer of the heater pins, so
the flags an operator reads and the pins can never disagree.
Host half: `HeaterKeeper` (`internal/command/heater_keeper.go`) re-asserts every
wanted heater on a `HeaterRefreshInterval` (10 s) ticker, run by
`Dispatcher.Run(ctx)` from the composition root. Two refreshes fit inside one
window, so a single lost keep-alive cannot flicker a heater that is on; a test
reads both constants — one from each language — and fails if they drift apart.
The intent recorded is deliberately asymmetric: an ON enters the keeper only
after the board has acknowledged it, because an intent that never arrived must
not be revived by the keep-alive later; an OFF is recorded even when the round
trip fails, because dropping a keep-alive can only cool. Every failure mode of
the host half — crash, cut cable, reboot, a Ground Station that never came up —
therefore leaves the firmware half to do the turning off, which is the direction
that is safe to fail in.

**IMU silence ends at 50 consecutive misses.** `noteImuMiss()` counts
`getEvent()` failures while the sensor is believed present; on the 50th (~1 s at
the 50 Hz tick, `IMU_MISSED_SAMPLES_MAX`) the sketch declares absence through
`applyImuHeading(..., false)` — the same call the no-sensor branch makes, so
presence still has exactly one writer. That flips `imu_present` false instead of
letting the wire claim a heading source it has not heard from in a second, and it
lets the 1 s re-probe run, so a sensor re-seated mid-run is picked up without a
reflash. A read that *answers* resets the counter even when its value was
non-finite — the question is "did the sensor speak", not "was the number usable"
— and the declaration resets it too, so a flapping sensor gets the full budget
each time and two separate runs of dropped reads cannot add up into one
declaration. Below the threshold nothing changes: one dropped read on a busy I2C
bus must not disturb a bearing.

**`sendMessage` writes only while a host is listening.** On arduino-pico,
`if (!Serial)` is TinyUSB's DTR bit (`tud_cdc_connected()`), so frames are
dropped — not queued — while nobody has the port open. A write to a CDC nobody
reads fills the TX buffer and then blocks, which would stall the 20 ms control
tick and freeze the antennas because nobody was there to read their telemetry. A
frame with no reader is not a frame. The handshake that proves `Link.Open` is
itself a `sendMessage`, so a link that comes up after a flash is this guard's
bench test.

### 6.3 Camera

Snapshot only. On `take_photo`, grab one full-resolution JPEG, write it to the
data directory atomically, and return its name and size in the command response.
The GS then fetches the bytes over HTTP. There is no live stream: it consumed
bandwidth that constraint H1 does not have, in exchange for a picture nobody
needed continuously.

Capture shells out to `fswebcam`. The hand-written V4L2 binding — `syscall`/`unsafe`
struct definitions against `/dev/video0`, which was the camera adapter before — is
**deleted**, along with its layout test. It was hand-written because there is no
maintained Go V4L2 package, and that is exactly the argument for not doing it: the
struct layout was an untestable liability maintained for the sake of a JPEG that
`fswebcam` produces in one line.

This is a reduction in capability, recorded as such: nothing can now set format,
resolution or exposure through the OBC. If a capture needs tuning that
`fswebcam` cannot express, the honest fix is a maintained capture package, not a
resurrected struct definition. `camera.device` still defaults to `/dev/video0`
because that is the device `fswebcam` reads.

### 6.4 SDR

The shape `obc_rocsar/sdr_service.go` described, cleaned up:

- `sdr_set_params` reads, validates and writes `parameters/params.json`.
  Write is atomic: temp file, then rename. A half-written `params.json` is a
  bricked SDR.

  **Every key `load_config()` reads is settable.** That is all twelve, and for a
  long time it was seven. `T_MIN_US`, `T_MAX_US` and `START_OFFSET_S` were not
  modelled at all and `TX_ANTENNA`/`RX_ANTENNA` were readable but absent from
  the patch and the wire, so an operator could retune the radio and set its
  gains but could not change the shape of the sweep it flies, the arming delay,
  or which RF path it used — all four were a hands-on edit to `params.json` on
  the aircraft. `internal/sdr/params_contract.go` is the one table of what is
  required, what is bounded and where each bound comes from, and per-key bounds
  are only part of it:

  `config.hpp` also throws on **`T_MIN_US >= T_MAX_US`**, which no per-key range
  can express, so `Service.SetParams` checks the pair on the merged result
  rather than on the incoming patch (the usual edit moves one edge and reads the
  other from the file). Writing a file that trips it is the worst outcome
  available — the SDR will not start and the only recovery is editing the file
  by hand — so it is refused host-side.

  `PULSE_DURATION` is the deliberate exception in the other direction:
  `config.hpp` has its read commented out, so it is carried through every
  update untouched and offered as a control nowhere.
- `sdr_connect` starts `connect` detached, with stdout and stderr to a
  timestamped log under the data directory. It refuses to start a second one
  while one is running, and the refusal names the running PID.

  **The child is reaped.** `Wait` runs on a goroutine, and it is what makes the
  refusal above transient. The first implementation called `Start` and returned
  the PID with nothing ever calling `Wait`, so an exited child stayed a zombie
  for the life of the OBC process — and `Running`/`Connect` decided liveness by
  signalling the PID, which *succeeds* against a zombie. One crashed connect
  therefore held the device permanently: every later `sdr_connect` was refused
  with "an acquisition is already running: pid N since <the original start>",
  naming a PID and a timestamp that were both true and both misleading.

  One watcher per acquisition is the single writer of the death transition:
  running flag, state and last error all come from it, and it guards on the PID
  so a late reap cannot overwrite a newer acquisition. The three ways a run can
  end are distinguished, because they are different facts: a session that
  reached `SESSION_DURATION` returns 0 and is **READY** with no error (it is the
  ordinary end of a capture, not an event), a non-zero exit or a fatal signal is
  **ERROR** with the exit status and the log path in `last_error`, and a
  `Stop` we asked for is **READY** with no error.

  The binary is found **on `PATH`**, falling back to `<sdr.program>/connect`
  when it is not installed. The deployed system installs it (`ln -s
  /root/rocsar-rpi/sdr-ettus-b200mini/connect /usr/local/bin/`) rather than
  shipping it beside the OBC, so a binary `scp`'d onto the Pi drives a program
  that was built and installed separately.

  `sdr.program` is therefore **not** the executable. It is the directory
  holding `parameters/` and `Data/`, and it is still the working directory the
  child is started in, because the C++ resolves both its configuration and its
  capture paths against `CWD`. That directory must be named
  `sdr-ettus-b200mini`: `connect.cpp` hardcodes
  `load_config("./../sdr-ettus-b200mini/parameters/params.json")`, which only
  reaches `<sdr.program>/parameters/params.json` for a directory with that
  name. The OBC checks the two derivations against each other on every start
  and refuses a mismatch, because otherwise `sdr_set_params` would report a
  successful write to a file the program never reads — an operator's gain and
  PRF changes appearing to save and doing nothing.
- `sdr_reset_usb` shells out to `uhubctl` for a power cycle.
- `sdr_probe` returns `uhd_usrp_probe` output verbatim.
- `sdr_get_params` reads `parameters/params.json` and returns it as JSON in
  the reply message. The read side of `sdr_set_params`: the Ground Station
  shows current values as placeholder hints while blank inputs keep meaning
  "leave alone". Same verbatim-output rule as `sdr_probe`.

`os/exec` is confined to this package (plus `camera`, which runs `fswebcam`).
`qos` no longer shells out at all — see §6.6.

### 6.5 Telemetry

`telemetry.Engine` is pure assembly with no goroutine and no I/O. The pipeline
owns a 1 Hz ticker, samples every provider, and encodes one `TelemetryFrame`.

Frame contents: system status and per-subsystem state, the selected GNSS fix
plus per-receiver health, Pico telemetry (heading, target, per-axis tick, angle,
load, temperature, `feedback_state`; heater states; `imu_present`), SDR state,
camera state, CPU temperature, uptime, and a monotonic `sequence`.

**`feedback_state` is not optional.** A servo that has stopped answering must not
be indistinguishable from one reporting a genuine zero load. `FEEDBACK_MEASURED` /
`FEEDBACK_HELD` / `FEEDBACK_UNKNOWN` is what separates them, and `imu_present` is
what separates a measured bearing from a held one. Both exist because the earlier
firmware showed a healthy-looking `0` on a motor that had not moved.

**Size budget.** The frame must stay under **2048 bytes**. It currently
serialises to roughly 400. The budget is asserted by a test, because the link in
H1 has a priority class of 41.4 kbit/s and a frame that outgrows it starves
commands.

### 6.6 QoS and link shaping

**One layer today: the kernel.** An HTB hierarchy on the link device:

```
root htb
├── 1:10  priority   rate 41.4kbit  ceil <link_rate>   ← telemetry, commands
└── 1:20  bulk       rate <link_rate - 41.4>bit        ← artefacts, HTTP
```

The root's default class is **1:10**, so a device with no classifier still gives
telemetry a floor rather than dumping everything into the class named "bulk".
Handles are fixed (`1:`, `1:10`, `1:20`, `110:`, `120:`) so that telemetry can name
them the way an operator reads them out of `tc qdisc show`; `HandleString` formats
them the same way rather than as decimal.

#### No classifier ships

There is **no filter**, and that is a measured result rather than an omission.
A `tc flower` filter on the bulk TCP port was implemented, described here as
load-bearing, and asserted by a test that passed. It did not classify anything.
Three separate faults, found in sequence on the target:

1. **No `classid`.** A flower filter with a match and no classid inspects
   packets and then does nothing with them. Bulk traffic went wherever the root
   default sent it, filter or no filter.
2. **Root default was `20`** — the bulk class. So *every* unclassified packet went
   to bulk and **1:10 was unreachable**; nothing could land there, telemetry
   included.
3. With the classid added, a proper classful tree (explicit root class `1:1`,
   children parented to it, `default 10`) and `GRO`/`GSO` disabled so packets
   reached the filter layer un-coalesced, a 30 KB ranged fetch of a real artefact
   **still landed in 1:10** at 41 kbit/s while 1:20 stayed at zero packets.

A separate real defect was fixed on the way and is worth keeping: `dst_port`
alone is rejected outright by tc (`Illegal "dst_port"`) because a layer-4 match
has no meaning until the transport is known, so `ip_proto tcp` is required
first. `tc-flower(8)` says so.

The rate cap is kept because it works, and is measured working: 41 kbit/s floor,
115 kbit/s ceiling, bulk 74 kbit/s. Telemetry reports *"rate limited to N kbit/s;
traffic is NOT classified"* so that `shaping_active` is never mistaken for a
priority class that does something.

**What replaced it for the minimal system:** artefact downloads are bounded
**in-process**, by a `golang.org/x/time/rate` limiter on the HTTP copy path
(`internal/transport/limiter.go`, driven by `qos.bulk_rate_bps`). That is the
traffic which was actually starving telemetry. It is not link shaping — it cannot
constrain the SDR, a system service, or anything else on the box, and it
back-pressures rather than prevents.

This limiter is a hand-rolled token bucket no longer: it delegates to
`x/time/rate`, a maintained library, rather than reimplementing arithmetic that is
easy to get subtly wrong. It keeps two behaviours the library does not give for
free: reads are granted in `quantum`-sized chunks so the HTTP path is not woken
once per buffer, and a grant is capped at `min(len(p), quantum, burst)`.

That cap is a bug fix, not an optimisation. The old bucket computed its wait from
`burst` and `rate` and then handed the reader `burst` bytes regardless of how many
were asked for. Below 512 B/s the quantum exceeded the burst, so the bucket
granted more than it had and the reader blocked forever. The low-rate case is now
covered by a test that exercises a rate the old code could not survive.

**Deferred to its own module.** Per-class link constraint needs a classifier that
demonstrably matches on this interface. Candidates, none yet proven here: `u32`
on the TCP port, `net_cls` on the artefact server's cgroup, or nftables. Choosing
between them by guesswork is how the flower filter came to be written.

#### Installed over rtnetlink, not `tc`

Every `tc` subprocess is **deleted**. Shaping is applied through
`github.com/vishvananda/netlink`, which speaks the kernel's own netlink socket
directly, and it sits behind the `qos.KernelOps` interface so the two implementations
can be swapped in one file.

The subprocess version was the fragile part, not the shaping:

- `sudo` on a Pi with no TTY blocks forever waiting for a password. A 5-second
  timeout per invocation was the mitigation, and it was a mitigation for a
  failure the design did not need to have.
- **`tc` fails for dozens of reasons unrelated to our logic** — absent binary,
  wrong interface, kernel without HTB, a non-interactive sudo — and detection was
  anchored on matching English error text. `"executable not found"` and
  `"Cannot delete qdisc"` are substrings of each other's failure modes, so the
  shaper could report a healthy device that it had never actually touched.
- `tc qdisc del` on a device with no qdisc *errors*, and for us that error is
  success. Distinguishing "nothing to clear" from "the tool is missing" by string
  matching is the exact fragility being removed.

Netlink removes all of it: there is no subprocess, no PATH lookup, no password
prompt, no timeout, and no text parsing. Errors are `syscall.Errno`, so
"not there" is `ENOENT`/`EINVAL` and is not confused with "tool missing".

The shaper requires `CAP_NET_ADMIN`, and says so in telemetry when it lacks it —
the OBC runs unshaped rather than refusing to start.

Three behaviours the implementation had to get right, all pinned by tests in
`internal/qos`:

1. **Root qdisc add/delete needs `netlink.HANDLE_ROOT`.** Passing a zero `Parent`
   returns `ENOENT`, because `0` names no qdisc.
2. **A fresh interface reports deleting its handle-zero `noqueue` qdisc as
   `EINVAL`.** "No hierarchy here" is `ENOENT`/`EINVAL`; both are treated as
   *absent*, never as failure.
3. **Netlink rates are bits per second.** `NewHtbClass` divides by 8 internally, so
   passing kbit/s as a bits/s value silently sets the rate 8× too high. The layer
   converts once, at the edge.

Leaves are `fq_codel`, not `pfifo limit N`. The netlink binding cannot express a
`pfifo` byte limit, so the leaves get CoDel's fair queueing with no explicit queue
length. This is a real, if small, deviation from the intended design: the classes
are what enforce the rates, but the queues no longer bound latency the way a
sized `pfifo` would. It is recorded here rather than described as intended.

**There is no userspace priority queue, and this section previously claimed
there was.** A two-class limiter (`PriorityHigh` bypass, `PriorityBulk`
token-bucket) was written, tested and then deleted, because it had no second
class with a producer:

- artefact bytes go over **HTTP**, not ZeroMQ — see §5.1;
- the camera is **snapshot only**, and `take_photo` returns a name and a size
  rather than an image — see §6.3.

So there is no ZeroMQ bulk traffic, the priority class is the only class, and a
limiter with one class is a token bucket around everything. The kernel hierarchy
is the only thing doing useful work, and it is the only thing that can: it shapes
traffic this process does not own.

A previous revision named `qos.Limiter` as the seam where a userspace priority
queue would return if a bulk publisher ever appears on the ZeroMQ socket. No such
type exists; the deleted queue was in `internal/qos` and the limiter that remains
lives in `internal/transport` (§6.6). The reason it was removed is recorded here
rather than left as an absence nobody can explain.

The classifier question is still open, and netlink did not close it. Installing
HTB over netlink is easier than `tc` was; *choosing which class a packet belongs
to* is a separate problem that netlink makes no easier. `net_cls` remains the
strongest candidate, because it needs a cgroup and the artefact server can be run
as its own process — but it is untested on this interface and stays deferred.

The published topic vocabulary (`qos.TopicTelemetry`, `qos.TopicControl`) stays:
it is the wire contract in §5.1, not part of the limiter.

### 6.7 Storage

Deliberately minimal: a data directory, atomic file placement, and a name that is
unique and sortable. **No artefact catalogue, no index.json, no checksums, no
paging.** Those were in ROCSAR_PI and are not in scope. HTTP lists the directory
and serves the bytes.

This is a real reduction in capability and is recorded as such: there is no
notion of "this file is still being written", so a listing taken during a capture
may show a partial SAR file. The write is atomic via rename, so a file is either
absent or complete — which is the property that actually matters.

---

## 7. Concurrency

| Goroutine | Owns | Never does |
| :--- | :--- | :--- |
| `gnss-rx-1..3` | one UDP socket each | mutate anything |
| `pico-rx` | serial read side | write to the port |
| `pico-tx` | serial write side, the sequence counter | read from the port |
| `camera` | the `fswebcam` child process | touch the network |
| `sdr` | the child process, `params.json` | block the command path |
| `telemetry` | the 1 Hz ticker | perform I/O in assembly |
| `zmq-control` | the ROUTER socket | touch the PUB socket |
| `zmq-publish` | the PUB socket | touch the ROUTER socket |
| `qos` | the priority queue | spawn a process except `tc` |

Three invariants:

1. **No goroutine mutates shared state directly.** Everything goes through a
   command or a channel. This is what makes the system debuggable: there is
   exactly one place where a transition happens.

2. **One goroutine per socket — never two on one, and never one for all.** A
   ZeroMQ socket is not thread-safe, and the failure mode of sharing one is a
   corrupted message stream that presents as a network fault. This was originally
   written as "all sockets on one goroutine", which is *also* safe, and which is
   what the first implementation did.

3. **Command handling blocks the control socket** for its duration. Acceptable
   because the slowest thing a handler does is the Pico acknowledgement timeout,
   bounded at 500 ms, and there is a single operator. A handler that blocked
   indefinitely would stop the Ground Station commanding anything at all, so this
   is a constraint to respect rather than a detail.

### 7.1 Why not one goroutine for both sockets

This is a correction, and the reason is worth recording because the obvious
design does not work with the chosen library.

`go-zeromq/zmq4` has **no receive timeout**. There is no `SetReadDeadline`, and
`WithTimeout` — which looks as though it would cover it — bounds only `Send`.
`Recv` derives a plain cancellable context with no deadline, so it blocks until a
message arrives or the socket is closed.

A single loop that drained the publish queue and then called `Recv` would
therefore publish **nothing** while no command was inbound: with a 1 Hz
telemetry stream and an idle operator, telemetry would never leave the process.
Polling is not available and cannot be added from outside the library.

Hence two goroutines, one per socket, and sockets are closed rather than
interrupted to shut the receive loop down.

### 7.2 Router frame layout

Asymmetric, and measured against real sockets rather than assumed from the ZMTP
pattern:

| Direction | Frames on the wire |
| :--- | :--- |
| Arriving at the ROUTER | `[sender identity, payload]` or `[sender identity, <empty>, payload]` |
| Sent from the ROUTER | `[destination identity, payload]` |

**The payload is the last frame, and the frames before it are envelope.** Both
arrival forms are legal ZMTP and both must work: a DEALER sending one part
produces the first, a REQ produces the second. Requiring the empty delimiter
rejects every peer that follows this table — found by `tools/gs_probe.py`,
because the Go test client had been sending `NewMsgFrom([]byte(""), body)`,
hand-rolling the REQ envelope, so the tests passed and the Python Ground Station
was silently dropped.

The first frame of a `Send` is consumed as routing and is **not** put on the
wire, so a DEALER peer receives the payload alone. A reply is built with
`NewMsgFrom(identity, payload)` — passing an extra empty delimiter, as the
REQ/REP pattern suggests, sends one fewer frame than expected and the peer reads
the wrong thing.

`context.Context` is the shutdown mechanism throughout, with sockets closed to
release a blocked `Recv`. There are no ad-hoc goroutine kills.

---

## 8. Failure policy

**Degrade and report. Never crash.** A telemetry server that is down tells the
operator nothing; one that is up and saying "the flight controller is silent"
tells them everything.

| Failure | Behaviour |
| :--- | :--- |
| GS disconnects | OBC unaffected. Telemetry publishes to nobody. Commands queue nowhere. |
| Pico unplugged | `SubsystemState.DISCONNECTED`. The Pico, if alive, holds its last bearing. The OBC does not retry silently — the state is reported. |
| Pico stops answering | ACK timeout counted, `feedback_state` degrades to `HELD`, `imu_present` stays true in the last frame received (fitted, not answering). On-board IMU silence is a different failure, counted on the board — see below. |
| Board powers up | Axes held, nothing transmitted until `set_target`/`jog`. No motion on boot, by design (§6.2). |
| Host dies while a heater is on | The firmware expires it within `HEATER_AUTO_OFF_MS` (20 s) of the last acknowledged on. Every host failure fails toward cooling (§6.2). |
| IMU fitted, then silent for 50 ticks | `imu_present` goes false after ~1 s, bearing held, the 1 s re-probe runs (§6.2). One dropped read changes nothing. |
| Nobody has the CDC port open | Frames are dropped at the source rather than filling the TX buffer and stalling the control tick (§6.2). |
| GNSS receiver silent | `fix_ok=false` after `gnss.stale_after`; the other receivers are unaffected; the selected one stays selected. |
| GNSS receiver returns garbage | The datagram is rejected and counted in `reject_count`. Not published. |
| Camera missing | `DISCONNECTED`. `take_photo` returns an error with a real reason. |
| SDR missing | `DISCONNECTED`. `sdr_connect` returns an error. Everything else runs. |
| `tc` unavailable | `LinkShaper` reports `ok=false` with the reason. Telemetry is unaffected. |
| `tc` hangs | 5-second timeout per command. |
| Data directory unwritable | Startup fails loudly — unlike a missing device, this one silently loses everything. |
| `/mnt/ssd` not mounted | Asserted at startup, with the reason. Continuing would write ephemerides to the root filesystem and fill it. |

`--require-hardware` inverts the policy for bench and CI work: it makes the
server exit non-zero if a device is missing, instead of degrading. The default is
degrade; the flag exists because some operators prefer to fail loudly and early.

The one asymmetry: **an unwritable data directory is fatal, a missing camera is
not.** Data loss is silent and unbounded; a missing camera is visible.

---

## 9. Real and mock hardware

**The default is real hardware.** A system that fabricates telemetry because a
device was not found is the most dangerous failure mode this project has, and it
is trivially easy to build by accident.

Three modes, and no fourth:

- **real** — the default.
- **mock** — only with an explicit `--mock-*` flag, and it announces itself loudly
  in the log and in telemetry (`SubsystemState` and a `mocked` set). Telemetry
  built from mocks is visibly mock.
- **degraded** — no mock, hardware absent. Reports `DISCONNECTED`, server runs.

There is no fallback-to-mock. If you want mock, you ask for it.

**`--mock-gnss` existed and was a lie; it is deleted.** It set `mocked = [gnss]`
while `gnss.NewBank` went ahead and constructed real `gnss.Receiver`s bound to
real UDP ports — so the OBC announced a simulation while three live receivers
were listening. That is the failure mode this section exists to prevent, running
in the opposite direction, and nothing in telemetry could distinguish it.

The GNSS bank always binds real receivers. To exercise the decoder without a
receiver, use `tools/gnss_bench inject`, which builds a real 142-byte datagram
and runs it through the real decoder — that tests the thing that was broken, and
it cannot be mistaken for a simulation of it.

The remaining flags are `--mock-pico`, `--mock-camera` and `--mock-sdr`, and each
one actually substitutes the implementation it names. `test/layering_test.go`
fails if a `domain` port loses its mock, so a new port cannot quietly arrive
unmocked.

**A flag that claims a subsystem is simulated must have a test that proves the
implementation behind it changed.** There is not one for the three above either
— the flags are wired in `cmd/obc/main.go` and read by eye. It is on the list.

---

## 10. Vendored third-party code

`third_party/Read_uB` and `third_party/sdr-ettus-b200mini` are vendored
**unmodified**, with their own makefiles. We did not clean them up and we will
not, for two reasons: they work, and they are the only things standing between the
project and an aircraft.

Consequences we accept:

- The 142-byte `UDP_message` is a **frozen contract**. We do not change it and we
  read it correctly; `test/gnss_test.go` decodes the real bytes and
  `TestGNSSDecodesDegreesNotRadians` is the test that exists because getting this
  wrong shipped a broken product.
- `Read_uB` also sends a 420-byte `NavData` we do not read (§6.1).
- The SDR program writes its output relative to its own working directory, so the
  OBC sets the child's CWD explicitly rather than inheriting one.

---

## 11. Configuration

One `rocsar.toml`, read by **both** the OBC and the GS. Resolution order is
mandated:

```
explicit flag  >  ROCSAR_* environment  >  rocsar.toml  >  code default
```

Every option is registered with a `nil` default so that the flag parser's own
default cannot silently overwrite the file. Every flag is `default=None` and
resolved by `internal/config` — never by `flag`.

`rocsar.local.toml` is gitignored and merges over `rocsar.toml` for per-machine
overrides.

An unknown key is **not an error.** A key that is neither known nor under a known
section is ignored, so a newer Ground Station talking to an older OBC does not
refuse to start.

Defaults live in `--help`, not in `rocsar.toml`. A default copied into the file
becomes a second home for a fact, and two homes drift.

| Section | Key | Default |
| :--- | :--- | :--- |
| `[server]` | `control_endpoint` | `tcp://*:5555` |
| `[server]` | `telemetry_endpoint` | `tcp://*:5556` |
| `[client]` | `control_endpoint` | `tcp://127.0.0.1:5555` |
| `[client]` | `telemetry_endpoint` | `tcp://127.0.0.1:5556` |
| `[http]` | `addr` | `:5557` |
| `[http]` | `root` | `/mnt/rocsar/data` |
| `[link]` | `device` | `eth0` |
| `[link]` | `rate_kbps` | `115` |
| `[link]` | `shaping` | `false` |
| `[pico]` | `port` | `/dev/ttyACM0` |
| `[pico]` | `baudrate` | `115200` |
| `[gnss]` | `ports` | `[2001, 2002, 2003]` |
| `[gnss]` | `selected` | `1` |
| `[gnss]` | `stale_after` | `2s` |
| `[camera]` | `device` | `/dev/video0` |
| `[sdr]` | `program` | `third_party/sdr-ettus-b200mini` |
| `[telemetry]` | `interval` | `1s` |
| `[qos]` | `bulk_rate_bps` | `8192` |
| *(top level)* | `require_hardware` | `false` |

`gnss.stale_after`, `qos.bulk_rate_bps` and `require_hardware` were missing from
this table in earlier revisions although all three are parsed. `[link] shaping`
defaulted to `true` here; the code default is `false` (§6.6 — an unshaped link is
the safe default, and `rocsar.toml` ships `false` with the reasoning inline).

`[sdr] program` is the one key whose value is **not** the thing its name
suggests. It is a directory, not a program, and it is not where `connect` is
found — that is on `PATH`. It must be named `sdr-ettus-b200mini`, for the
C++'s hardcoded config path to resolve to the file the OBC edits; see §6.4.

There is no `[gui]` section and no `gui.*` key. `GUI_ARCHITECTURE.md` describes the
Ground Station console's own settings; none of them are read by the OBC, and
`[client]` here holds only the two endpoints the GS dials.

`http.addr` used to be load-bearing: the `tc` flower filter classified on
`dst_port 5557`, so changing the port meant the filter silently stopped matching
and downloads joined the priority class. There is no filter now, and
`config.BulkPort` is gone with it.

The in-process limiter bounds the artefact handler wholesale rather than matching
a port, which is one less concept and cannot be defeated by changing the port.
`Validate` no longer refuses a mismatched pair, because nothing mismatches any
more: a wrong port used to be a silent starvation, and now it is a different
number.

---

## 12. How the architecture is kept true

Discipline decays. Five mechanical checks:

1. **Import-graph boundary test.** `test/layering_test.go` parses every `.go`
   file with `go/parser` and fails if `internal/domain` or `internal/telemetry`
   imports `go-zeromq`, `net`, `os/exec`, or `api/rocsar/v1`. Imports inside
   function bodies count — a function-local `import "net"` is exactly the
   shortcut that rots a layering rule.
2. **Port completeness test.** Every interface in `internal/domain` must have a
   real implementation and a mock, reachable from the tree. An interface with one
   implementation is a class with no seam and no test.
3. **Config completeness test.** Every flag the command registers must appear in
   the configuration table, and every configuration key must be reachable from a
   flag or an environment variable. Adding one without the other fails the build.
4. **`os/exec` confinement test.** The only packages permitted to spawn a process
   are `internal/sdr` and `internal/camera`. `internal/qos` was removed from this
   list when it stopped shelling out (§6.6).
5. **Generated-protobuf confinement test.** Generated stubs may be imported only
   by the codec packages. This is what makes "regenerate and the build still
   works" a property rather than a hope.

Plus a set of **contract tests that decode real bytes**, because a document
describing a wire format is a guess until something reads it:

| Test | Guards against |
| :--- | :--- |
| `TestGNSSDecodesDegreesNotRadians` | the double-conversion bug that shipped |
| `TestCobsRoundTrip`, `TestCobsSizeFormula`, `TestCobsDecodeRejectsMalformed` | framing corruption at the 254-byte edge |
| `TestCobsEncodeMatchesFirmwareC`, `…Randomised` | the Go encoder and the C encoder disagreeing |
| `TestEveryCommandEmitsOneDelimitedFrame`, `TestMaxFrameMatchesTheFormula` | the Pico envelope drifting from `firmware/pico_wire.h` |
| `TestTelemetryFrameFitsThePriorityClassBudget` | the frame outgrowing the priority class |
| `TestNoClassificationFilterIsInstalled` | §6.6 ships no filter, and says so |
| `TestWriteFileAtomicLeavesNoPartialFile` | a half-written `params.json` or artefact |
| `TestPartialUpdatePreservesUnmodelledKeys`, `TestMissingRequiredKeyIsRefused` | an SDR params write silently dropping keys |

**The firmware host suite tests the other half of every pairing.**
`firmware/tests/` compiles `gondola_model.h` into a probe binary and drives the
real `applyCommand()`/`expireHeaters()` from pytest, plus source-read assertions
on `firmware.ino` for what only exists in the sketch. Three of its tests exist
specifically to stop two files from drifting apart:
`test_the_host_refreshes_at_least_twice_per_firmware_window` reads
`internal/command/heater_keeper.go` from Python and fails if two refreshes no
longer fit inside `HEATER_AUTO_OFF_MS`;
`test_power_up_holds_both_axes_and_transmits_nothing` asserts the boot interlock
from the transmit side (DRIVE), not from what the model believes; and
`test_the_fiftieth_miss_declares_the_sensor_absent` pins the IMU counter's
threshold, its reset-on-declaration, and that a sample arriving resets it too.

**Most of the names in earlier revisions of this table did not exist.** Listed
were `TestGNSSDecodesDegrees`, `TestPicoFrameMatchesFirmware`,
`TestTelemetryFrameUnderBudget`, `TestFlowerFilterPresent`,
`TestFirmwareExcludesGroundStationTypes` and `TestSDRParamsAtomicWrite`; not one
was a test that exists. The behaviours mostly *are* covered, under different names
and in different packages — three were near-misses of a renamed test, and three
describe capabilities that have since been deleted. Two consequences:

- A test name in this document is a claim about the tree, and it was wrong six
  times out of eight. Names here are to be read as "the coverage exists here",
  verified against the source, not as documentation of intent.
- `TestFlowerFilterPresent` is gone because there is no filter (§6.6);
  `TestFirmwareExcludesGroundStationTypes` never existed — the firmware exclusion
  is enforced by `scripts/generate.sh` (§4.4), not by a Go test;
  `TestSDRParamsAtomicWrite` was misfiled, since atomic write is a `storage`
  concern, and SDR params have their own contract tests.

---

## 13. Build

```
buf generate            # api/rocsar/v1 → Go, Python
scripts/generate.sh     # firmware → nanopb C, common + pico only (§4.4)
go build ./...
go test ./...
go run ./tools/gs_cli   # the Ground Station terminal console
arduino-cli compile -u -p /dev/ttyACM0 --fqbn rp2040:rp2040:rpipicow firmware/
```

Toolchain: Go 1.26, buf 1.72, `arduino-cli` with the `rp2040:rp2040` core, gcc
for the firmware host tests.

An earlier revision listed `python -m pytest gs/`. There is no Python application
under `gs/` — only generated protobuf stubs (§2) — so there is nothing there to
test. Python 3.11 is still needed to *generate* the stubs, and `tools/gs_probe.py`
runs on it, but the Ground Station is Go.

There is **no CI.** This is stated plainly because it is a real gap: every check
above currently depends on a human remembering to run it. Adding CI is the
highest-value non-functional change available.

---

## 14. What is not covered

Known limits, stated so nobody mistakes silence for coverage:

- No CI.
- No hardware in the loop. The Pico, the servos, the SDR, the camera and the
  real GNSS receivers are untested by the suite. Tests use doubles and injected
  fakes; the integration is assumed correct until proven otherwise in flight.
  What *has* been exercised by hand on real hardware (bench session, Oct 2026):
  flash and link bring-up through the `if (!Serial)` guard, the boot interlock
  (a fresh boot held at `target 0` for 20 s where the pre-change firmware had
  both axes stalled at the rail), both interlock release paths, and the heater
  pair (held past the firmware window by the keeper, then expired after the OBC
  was killed, with no resurrection on restart). A bench session is not
  automation; none of it runs unattended.
- **Link shaping is verified by reading back the qdisc, not by measuring
  throughput under load.** `internal/qos/netlink_test.go` installs the hierarchy in
  a real network namespace and asserts the rates the kernel reports back, and
  `test/qos_test.go` asserts the intended hierarchy against a fake `KernelOps`. The
  41/74/115 kbit/s figures in §6.6 come from `tc qdisc show` output on a
  development interface. What is **not** established is achieved throughput on the
  real radio link with a real receiver attached. Both the measurement and the
  classifier remain open.
- `internal/qos` leaves are `fq_codel`, not plain `pfifo limit`. The netlink
  binding used here cannot express a `pfifo` byte limit, so leaves get fair
  queueing with no explicit queue length. See §6.6.
- The 420-byte `NavData` stream is not read (§6.1).
- Servo status reads are **blocking**, bounded by `SERVO_STATUS_WAIT_MS` (5 ms,
  ~4× a reply). A split-phase read was assessed and deferred: the worst observed
  case is ~8–9 ms of the 20 ms tick, and splitting it would couple every servo
  write to the read state machine. The cheap alternative, host-side overrun
  detection from reply sequence gaps, has not been built either.
- `SERVO_LOAD_PERCENT_SCALE` (10.0) is unverified. On the bench it reads
  104–105% both stalled against the rail and at mid-travel, and 0% at the
  opposite end — the number has never been calibrated against amperes, and a
  load percentage that saturates is not yet an operator signal.
- There is no artefact catalogue, so no integrity checking on stored files
  (§6.7).
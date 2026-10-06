# ROCSAR Ground Station — GUI Architecture

**Binding document, subordinate to `ARCHITECTURE.md`.** Read that first. This
one covers exactly one thing: the operator console, `cmd/gs`. Where the two
disagree, `ARCHITECTURE.md` wins and this one is wrong.

Where this document *amends* `ARCHITECTURE.md`, it says so and names the
section. Every amendment is also an edit to `ARCHITECTURE.md` in the same
commit — see §14. A subordinate document that quietly contradicts its parent is
worse than no document.

## 0. Precedence

```
ARCHITECTURE.md  >  GUI_ARCHITECTURE.md  >  code
```

`ARCHITECTURE.md` §0 outranks every other file. This document ranks immediately
below it. `ARCHITECTURE.md` describes the link, the schema and the OBC; this
describes the one program that consumes that link.

**Amendments to `ARCHITECTURE.md` claimed by this document:**

| §  | Amendment |
| :--- | :--- |
| 1 | The GS row of the node table is Go + a browser, not PySide6. |
| 1 | The topology diagram's GS box is `cmd/gs`. |
| 2 | `gs/` (Python) leaves the layout; `cmd/gs/`, `internal/client/`, `internal/gsview/` enter it. |
| 5.1 | The topic vocabulary is **one** topic, not five. `ARCHITECTURE.md` §5.1 is wrong. |
| 11 | `[client]` gains an HTTP endpoint key and a `[gui]` section appears. |
| 12 | Two more mechanical checks, and the scan roots widen. |
| 13 | `python -m pytest gs/` leaves the build; `npm run build` enters it. |
| 14 | One more known limit: no second-implementation check of the ZeroMQ wire. |

---

## 1. What this adds

The Ground Station is the operator console: it displays telemetry and sends
commands. It is `cmd/gs`, a web app (Go server + browser) on the operator laptop.

**It is a second binary, not a mode of the first.** The OBC stays headless on
the Pi. The reasoning is constraint H4 — the link may die, nothing in the
control path may depend on the GS existing — read in the other direction: a
console must not be able to take down the thing it watches. A single binary
that served both roles would make a webview fault a control-path fault, and
H2/H3 exist precisely because the Pi is not a place to run anything that can
hang.

Consequences that follow, and are not revisited:

- The OBC builds and runs with no browser, no webkit and no node.
- `go build ./...` at the repo root does not need a browser either (§12).
- A GS crash loses the operator's view and nothing else.

### 1.1 Decisions, with the alternatives

| Decision | Chosen | Rejected | Why |
| :--- | :--- | :--- | :--- |
| Deployment | Laptop, separate binary | GUI on the Pi | H2/H3/H4. A webview under the aircraft. |
| Toolkit | Pure web app (external browser) | Wails v2/v3, Lorca, go-gui | The map (§1.2) needs WebGL; the system browser has the most mature pipeline. No embedded engine, no CGO, no window management. See §1.3–§1.4. |
| Frontend | Vanilla TS + Vite | Svelte, React | The binding layer is framework-agnostic; a GUI that is mostly 1 Hz panels does not earn a component model. Swapping later is mechanical. |
| Wire format in the browser | **none** — Go decodes once | `protoc-gen-ts`, protobuf-es | One decoder, in Go, with the tests §13 lists. A second decoder in TypeScript is a second thing to disagree. |
| Client code | Promote `tools/gs_cli` | Write fresh | It already exists and is good. See §6.1. |
| Python `gs/` | Delete | Keep alongside | Half-maintained frontends rot. Loss recorded in §14.3. |

### 1.2 The map requirements (new, and they drive the toolkit question)

§15 deferred the map. It is no longer deferred: the map is a primary input
surface, and three requirements come with it. They are stated here because
they constrain the toolkit more than anything else in this document.

1. **Offline, always.** No network at runtime. The basemap is bundled into the
   binary as GeoJSON assets (`cmd/gs/frontend/public/geojson/`, committed). A
   tile server, a CDN, and a `fetch()` to the open internet are all equally
   wrong: the console runs on a laptop that may be in a field, on a plane, or
   on a ship. The assets are embedded, not fetched.
2. **Interactive.** The operator clicks a point on the map to command the
   antennas to point at it. This is not a future nicety — it is the primary
   input method for the console's most common action. The map must report
   click coordinates (latitude/longitude, degrees, unconverted — §7.3) to the
   command path, and render a marker at the commanded bearing. Pan, zoom, and
   a position trail are the minimum viable set.
3. **Correct rendering.** The console is dense, dark, monospace-leaning and
   CSS-heavy. An engine that mis-renders it is not a console. This is the
   requirement the current WebKitGTK shell fails.

**The map must be drawn by a browser engine.** MapLibre GL JS renders vector
data through WebGL and Web Workers, and no Go-native toolkit reproduces it.
This is the constraint that decides the toolkit question, and it is why every
Go-native option below is rejected regardless of its other merits.

#### 1.2.1 The basemap: Natural Earth 50m GeoJSON

The requirement is *low-detail geographic context* — rivers, lakes, coastlines
and mountains at a regional scale — not a street basemap. Two datasets were
measured against that requirement:

| Dataset | Size (gzip) | Rivers | Lakes | Mountains | Verdict |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **Natural Earth 10m** | 21.4 MB | 1 in Portugal | **0 in Portugal** | **0 in Portugal** | **Rejected.** Bigger than anything else and delivers no Portuguese hydrography or peaks. |
| **Natural Earth 50m** | **1.5 MB** | 462 | 405 | 127 ranges, 76 summits | **Chosen.** |
| Natural Earth 110m | 0.29 MB | 13 | 25 | 10 ranges, 12 summits | Fallback if 50m proves too detailed a look. |
| OpenMapTiles pmtiles | 2.5 MB (Porto only) | street-scale | yes | terrain | Replaced: a real operating-area extract is far larger, and the requirement is context, not streets. |

50m was chosen because 10m — the scale that *sounds* most detailed — has no
lakes and no mountains anywhere in Portugal, while costing 14× more. The
detail that matters is coverage of the operating area, not the nominal scale.

The assets are produced by `scripts/fetch-basemap.py`, which downloads the
upstream GeoJSON, rounds coordinates to 6 dp (the linework is generalised;
14-digit precision is noise), and filters `geography_regions_polys` to
`featurecla == "Range/mtn"` — 127 of its 514 features are mountains, and
shipping island groups and deserts to draw mountains would be paying 3 MB for
map never rendered. That filter turns a 10.4 MB file into 78 KB gzipped. The
script writes pre-compressed `.json.gz`; `cmd/gs/bridge.go` serves them with
`Content-Encoding: gzip`, so the bytes embedded in the binary are the
compressed ones and the frontend still reads plain JSON URLs.

Natural Earth is **public domain** (`naturalearthdata.com/about/terms-of-use`),
which removes the attribution obligation a tile basemap would carry.

**Recorded limitation.** Natural Earth has no terrain, no hillshade, no
contours, and no dedicated mountain file. Mountains exist only as range
*outlines* (coarse polygons) and named summit *points* with metre elevations.
If visual relief is ever required, it is a raster hillshade layer, which is a
new asset and a new decision — not a change to this one.

### 1.3 Toolkit options

The console needs a browser engine for exactly one reason: the map. Everything
else (telemetry panels, command buttons, artefact browser) is ordinary HTML.
So the question is not "which GUI framework" but "which browser engine, and
how is it launched". The candidates, with what was found on each:

| Option | Engine | Map | Verdict |
| :--- | :--- | :--- | :--- |
| **Wails v2** (current) | WebKitGTK 4.1 | JS (Leaflet) | **Rejected.** The rendering bug that started this. WebKitGTK 4.1 is old and the console is mis-formatted. |
| **Wails v3** (beta) | WebKitGTK 6.0 (GTK4) | JS (Leaflet) | **Possible, with reservations.** Newer WebKit may render correctly — 6.0 is a large jump from 4.1. But it is still WebKitGTK: the same engine family that failed, one version later. Beta (v3.0.0-beta.26, 2026-09). GTK3 + WebKit2GTK 4.1 remains as a `-tags gtk3` legacy option through v3.0.x. Hot reload, in-memory IPC, auto-generated bindings. |
| **Lorca** | Chrome/Chromium (CDP) | JS (Leaflet/MapLibre) | **Possible.** Different engine family (Blink, not WebKit). Chrome is the most-tested rendering engine in the world. Requires Chrome installed on the operator's laptop (weakens G1 — §2). The library is small and stable but lightly maintained. |
| **Pure web app** | Any browser the operator chooses | JS (Leaflet/MapLibre) | **Possible.** The Go binary serves the UI over HTTP on `127.0.0.1`; the operator opens Chrome, Firefox or Safari. No embedded engine, no rendering surprises, no single-binary constraint, no window management. The frontend is identical to the embedded case; only the launch differs. Loses the "app" feel (a browser tab, not a window) and requires the operator to open the URL. |
| **go-gui + go-map** | Native (no browser) | Native widget | **Rejected for the map.** `go-gui-org/go-map` is a slippy-tile widget (raster + vector, pan/zoom, markers) but has no pmtiles support, draws tiles itself, and is very new (2 stars, Aug 2026). The offline pmtiles archive would need a Go renderer written. The rest of go-gui (immediate-mode, GPU, no JS) is interesting but does not help with the map. |
| **Fyne** | Native (OpenGL) | None | **Rejected.** No map support. No JS. |
| **Gio** | Native (no browser) | None | **Rejected.** No map support. |
| **Tauri** | WebKitGTK (Linux) | JS | **Rejected.** Same WebKitGTK engine as Wails on Linux, plus a Rust backend this project does not have. |
| **Neutralinojs** | System WebView | JS | **Rejected.** Same system-webview problem. |

### 1.4 The decision

**Chosen: the pure web app (external browser).** The Go binary is a plain
`net/http` server on `127.0.0.1`; the operator opens the URL in their own
browser. The deciding factor is WebGL: MapLibre GL JS renders vector tiles
through WebGL, Web Workers and hardware acceleration, and the system browsers
(Chrome, Firefox, Edge, Safari) have the most mature WebGL pipelines available.
An embedded engine — WebKitGTK or a bundled Chromium — is a less-tested WebGL
path and the one that already failed (§1.3, Wails v2).

Consequences, recorded rather than implied:

- **No CGO, no GTK, no webkit, no browser process.** The Go binary is a
  self-contained `net/http` server. It cross-compiles trivially to Linux,
  macOS and Windows with no C bindings and no OS-specific build toolchain.
- **No window management.** The console is a browser tab, not a managed
  window. There is no frameless mode, no title bar control, no window
  lifecycle — the browser owns all of it.
- **The operator opens the URL.** The binary prints
  `http://127.0.0.1:<port>` on startup. This is the whole of the "launch".
- **Pivot-friendly.** The frontend, the bridge (§8) and `internal/client` +
  `internal/gsview` are identical to the embedded case. If a dedicated window
  is wanted later, the same architecture wraps into Wails, Tauri or a
  webview with no frontend change.

Wails v3 is the fallback if the system browser also mis-renders: it is the
same codebase with a newer WebKitGTK, and the migration from v2 is smaller
than the migration to a web app. It is a fallback, not a first choice,
because the engine family is the one that already failed.

---

## 2. Constraints

H1–H5 from `ARCHITECTURE.md` §1.1 bind the console. Restated as they bear on a
*client*, which is not the same reading as for a server:

- **H1 — 115 kbit/s.** Artefacts are fetched over HTTP with resume, never
  through ZeroMQ. Progress and an ETA are mandatory, not polish: the default
  `qos.bulk_rate_bps` is 8192 B/s, so a 30 MB capture is over an hour. A
  progress bar with no ETA is a hang from the operator's side.
- **H4 — the link may die.** The console must survive the OBC disappearing,
  reconnect when it returns, and never queue a command across the gap. A command
  sent at the moment the link drops must fail, not arrive late (§6.4).
- **H5 — nothing simulated unless asked for.** `system.mocked_subsystems` is
  rendered as a banner, always, when non-empty. It is on the wire; hiding it
  would be the exact failure this project is organised against.

Two that are new because this is a client:

- **G1 — a single static binary.** The laptop may have no Go toolchain, no
  node, no protobuf compiler. `go build` produces one file that runs. This
  is why the Python stubs were committed for the old GS and why nothing
  generated is required at install time now.
- **G2 — the console is not in the control path.** No GS behaviour may be
  required for the vehicle to keep pointing. The Pico holds its bearing
  regardless of what this program does, including not existing.

---

## 3. Topology

```
+---------------------------------------------------------------------------+
|  GROUND STATION   cmd/gs, pure web app, operator laptop                     |
|                                                                          |
|    internal/client ── ZMQ DEALER -> :5555   (commands)                   |
|                  ├─ ZMQ SUB     <- :5556   (telemetry, topic "telemetry") |
|                  └─ HTTP        <- :5557   (artefact listing + bytes)    |
|                                                                          |
|    internal/gsview ── TelemetryFrame + CommandResponse -> plain structs   |
|    cmd/gs (web app) ── structs -> JSON -> frontend                        |
+---------------------------------------------------------------------------+
                                   |
                    Ethernet, 115 kbit/s
                                   |
                          cmd/obc  on the Pi
```

Three connections, matching the three the OBC serves. `ARCHITECTURE.md` §5.1
gives the reasons: commands and telemetry are ZeroMQ, artefact bytes are HTTP.

### 3.1 The topic is one topic

`ARCHITECTURE.md` §5.1 lists `telemetry.system`, `telemetry.gnss`,
`telemetry.pico`, `telemetry.sdr` and `telemetry.camera`. **None of those
strings exist.** `internal/qos/topics.go` declares `TopicTelemetry = "telemetry"`
and `TopicControl = "control.response"`, and `AllTopics()` returns exactly
`["telemetry"]`. Only the aggregate frame is ever published; the second constant
is declared and unused.

A client written from §5.1 subscribes to five topics, matches none, and reports
a dead OBC — because ZeroMQ filter mismatch and a dead OBC are indistinguishable
from the client. This is corrected in `ARCHITECTURE.md` by this commit. The
console subscribes to `"telemetry"` and nothing else.

---

## 4. Repository layout

```
rocsar/
├── GUI_ARCHITECTURE.md   ← this document
├── ARCHITECTURE.md       ← amended by §14 in the same commit
├── shell.nix             ← GS toolchain, §12
├── cmd/
│   ├── obc/              ← unchanged. headless.
│   └── gs/               ← the console: web app (Go server + browser)
│       ├── main.go       ← composition root, like cmd/obc
│       ├── app.go        ← http server, bound methods, event fan-out
│       ├── bridge.go     ← WebSocket hub + dispatch (replaces wailsjs)
│       └── frontend/     ← vanilla TS + Vite, §9
├── internal/
│   ├── client/           ← the GS side of the link. §6. promoted from tools/gs_cli.
│   ├── gsview/           ← TelemetryFrame -> plain structs. §7. no I/O.
│   └── ...               ← unchanged
├── tools/
│   └── gs_cli/           ← becomes a thin main over internal/client. §6.1
└── test/                 ← + client_test.go, gsview_test.go
```

`internal/client` and `internal/gsview` are new directories. `internal/` and not
`client/`, because `test/layering_test.go` scans `cmd/` and `internal/` and
nothing else — putting them anywhere unscanned would place them outside the
architecture's mechanical enforcement, which is worse than not having the rule.
The scan roots are widened explicitly in §13.

---

## 5. Layering

The client is a **peer** of `internal/transport`, not a layer above it.
`transport` is the server side of the wire; the client is the other side. Neither
imports the other, and neither knows the other exists.

```
cmd/gs  ──▶  internal/gsview  ──▶  internal/client  ──▶  api/rocsar/v1
                     │                  │
                     └──── no I/O ─────┘
```

- `internal/gsview` imports **no** package outside the standard library and not
  `api/rocsar/v1`. It takes already-decoded values and produces plain structs.
  This is the same rule `internal/telemetry` lives under, for the same reason:
  assembly must be testable without a socket.
- `internal/client` owns every socket, every timeout and every retry. It is the
  only new package permitted to import `api/rocsar/v1`.
- `cmd/gs` may not talk to a socket. It wires the two together and serves the web app.

### 5.1 Consequences for the boundary test

`test/layering_test.go` has two rules that this layout collides with, both
requiring deliberate edits rather than workarounds:

1. `TestGeneratedProtobufIsConfinedToCodecPackages` allows
   `github.com/rocsar/obc/api/rocsar/v1` in exactly `internal/transport`,
   `internal/pico`, `internal/command` and `cmd/obc`. **`internal/client` is
   added.** The client encodes `CommandRequest` and decodes `TelemetryFrame`, so
   the codec genuinely lives there, and the same reasoning as `internal/pico`
   applies: it converts at the socket edge and nothing below that point touches
   protobuf.
2. `allGoFiles` walks `{"cmd", "internal"}`. That already covers the new
   packages, so **no change is needed** — recorded here because the alternative
   (parking the client in `client/` at the top level) would have silently
   exempted it from every rule in the file.

`internal/gsview` is added to the same allowlist as a **prohibition**, not a
permission: it must not import `api/rocsar/v1`, `net`, `os/exec` or
`go-zeromq`, on the model of the existing `internal/domain` and
`internal/telemetry` rules.

---

## 6. `internal/client`

The client half of the link. Promoted from `tools/gs_cli`, which already
implements most of it.

### 6.1 Promotion, not replacement

`tools/gs_cli` (HEAD `e0b471b`) is a complete console from a terminal:
`subscribe`-before-`dial` ordering, DEALER-not-REQ, `request_id` correlation,
reply frame-count assertion, per-command argument validation, a resumable
Range-based fetch with atomic rename, and tests for the validation. It is good
work and it is `package main`, so `cmd/gs` cannot import it.

The move is mechanical — `package main` → `package client`, exported method
names, `fmt.Printf` → return values — and **the comments move with the code**.
Several of them record bugs found the hard way:

- `client.go:57` — a REQ socket inserts an empty delimiter the OBC's ROUTER
  does not send, so the reply lands a frame late.
- `client.go:71` — the SUB filter is set before dialing, or the first frames are
  lost invisibly.
- `render.go:161` — an over-strict artefact-name check refused the exact
  `photos/…` names the tool had printed one line earlier.

`gs_cli` remains, as a thin `main` over the same package. A laptop with no
webkit still has a way in, which is worth keeping for a console that supports a
vehicle in flight.

### 6.2 One goroutine per socket

The same invariant as `internal/transport/zmq.go`, and for the same documented
reason: `go-zeromq/zmq4` has **no receive timeout**, so `Recv` blocks until a
message arrives or the socket closes. A SUB reader and a DEALER reader, each
owning its socket, each unblocked by closing rather than by context
cancellation.

The client differs from the server in one respect: it reconnects (§6.5), so its
receive loops are restartable. They are owned by a supervisor that rebuilds the
sockets, not by the loops themselves.

### 6.3 Slow joiner

`ARCHITECTURE.md` §5.1 states the GS client must discard the first frames after
subscribing, or it measures ~10× the true rate followed by silence. **No such
constant exists in the repository** — the requirement has been documented since
the beginning and never implemented, because there was no client.

```go
// SlowJoinerFrames is how many frames to discard after subscribing.
const SlowJoinerFrames = 5
```

Frames are still counted and the discard is visible in the UI as a "measuring"
state, not a silent drop. Five is enough for a 1 Hz PUB/SUB handshake and small
enough that reconnecting does not feel broken.

### 6.4 One command in flight

`internal/transport/zmq.go:40` and `ARCHITECTURE.md` §7: the ROUTER handles one
command at a time, and the slowest handler is the Pico acknowledgement timeout
at a fixed **500 ms** (`internal/pico/framing.go:135`). Two commands sent back to
back are not refused; the second waits, and if it has a 20 s client timeout it
may expire while the OBC is still working through the first.

So the client serialises: **one in flight, one queued, the rest refused with a
reason.** A GUI makes this easy to get wrong, because a slider or a row of
buttons can each fire an event. Refusing is deliberate — silently queueing
operator clicks turns "I pressed jog" into "jog happened at some point".

A command queued while the link is down is **dropped, not queued**. It fails with
`client.ErrNotConnected` -- a Go sentinel, not a synthesised `CommandResponse`
carrying `NOT_CONNECTED`. That code is a wire error the OBC puts in a reply, and
manufacturing one locally for a reply the OBC never sent would be a lie about
where the refusal came from. The operator-visible sentence is the same either
way; the provenance is honest this way. A jog issued when the link dies must not
be executed when the link returns, an indeterminate time later, at a bearing the
operator has since forgotten.

### 6.5 Reconnect

`tools/gs_cli` has none — it is a foreground tool that exits. A GUI cannot.

- Backoff 1 s → 2 s → 4 s → 8 s, capped at 8 s, reset on a successful frame.
- Each reconnect discards `SlowJoinerFrames` again (§6.3).
- Reconnect is **the console's** decision, not the client's, for the same reason
  `pico.Link` has no reconnect loop (`ARCHITECTURE.md` §6.2): a component that
  retries on its own turns a recoverable absence into an invisible retry storm.
  Here the retry loop is one explicit, visible, bounded supervisor rather than
  scattered inside socket code, and its state is on screen.
- The SUB and DEALER reconnect **independently**. Telemetry can resume while
  commands are still failing, and the UI says exactly that.

### 6.6 Sequence gaps

`TelemetryFrame.sequence` is monotonic (`telemetry.proto:96`). Three cases, and
the console distinguishes all of them:

| Observation | Meaning | Shown as |
| :--- | :--- | :--- |
| `seq == last + 1` | nominal | — |
| `seq > last + 1` | frames dropped on a 115 kbit/s link | gap count, cumulative |
| `seq < last` | the OBC restarted | "OBC restarted", counters reset |

The third is the one worth surfacing: a restart resets the PID and the uptime,
and an operator watching a stale uptime is being lied to by a number.

### 6.7 Link state

One struct, polled by the UI and pushed on change:

```go
type LinkState struct {
    ControlConnected   bool
    TelemetryConnected bool
    LastFrameAt        time.Time  // zero = never
    LastFrameAge       time.Duration
    FramesReceived     uint64
    SequenceGaps       uint64
    FramesDiscarded    uint64     // slow joiner, §6.3
    CommandsInFlight   int
    LastError          string
}
```

**Staleness greys the controls.** No frame for three telemetry intervals
(default 3 s) means the console is looking at a picture of a vehicle it can no
longer reach. Motion commands become disabled. This is H4 read from the
operator's side, and it is the one piece of GS behaviour that is a safety
property rather than a convenience.

### 6.8 Artefacts

The HTTP contract, from `internal/transport/http.go` and `internal/storage`:

- `GET /` and `GET /<dir>` → `{"path": …, "files": [Entry]}`, where `Entry` is
  `{name, size_bytes, modified_unix, kind, directory}`.
- `kind` is a closed set — `camera`, `sdr`, `log`, `unknown`.
- `GET /<file>` honours `Range` and answers `206`.
- Anything escaping the root after cleaning is `403`.

The client keeps `render.go`'s resumable fetch and adds what a GUI needs:
progress events (coalesced to ~10 Hz — see §9.2), a `cancel`, and an ETA derived
from the observed rate rather than from `qos.bulk_rate_bps`, which the client
cannot read and which the operator may have overridden anyway.

---

## 7. `internal/gsview`

`TelemetryFrame` → plain structs the frontend can render. No sockets, no window,
no clock. Pure, and therefore testable, which is the whole reason it is a
separate package from `cmd/gs`.

### 7.1 Absence is not zero

The single most important rule in this document, and it is already implemented
in `tools/gs_cli/render.go`. The proto3 wire makes absence and zero
indistinguishable unless the consumer is deliberate about it, and a console that
conflates them is the failure this project exists to prevent.

Every field that can be absent **is** absent in the view struct, as a pointer or
an explicit `Known bool`:

| Wire | Absent means | Renders as |
| :--- | :--- | :--- |
| `pico` | never heard from the flight controller | "no data", not heading 0° |
| `pico_last_ack` | never acknowledged | "no ack", not error NONE |
| `gnss[i].fix_age_s` | never had a fix | "never", distinct from age 0 |
| `gnss[i]` with `fix_ok=false` | stale or rejected | coordinates **suppressed** |
| `camera.photos_taken` | camera subsystem unspecified | "—", not 0 photos |
| `imu_present=false` | heading is held, not measured | "HELD" |
| `antennas[i].feedback_state` | servo never answered | "no reading" |

`render.go:46` already refuses to print coordinates beside a `no fix`, because a
receiver with no fix still carries the last values it decoded and printing them
invites reading the number instead of the qualifier. That is preserved exactly.

### 7.2 `feedback_state` is an int32, and stays one

`common.proto` declares `FeedbackState` as an enum but puts a bare `int32` on
the wire, so a value from a future firmware decodes to a plain integer instead
of failing the frame. `test/schema_test.go` pins this.

The view switches on 0/1/2 with a defined fallback for anything else, and
renders `MEASURED` / `HELD` / `no reading` — never the number. `render.go:68`
only shows load and temperature when `feedback_state == MEASURED`, because a held
value is the last reading or the command echo, and showing it as a measurement
is the specific thing that column exists to prevent. The earlier firmware showed
a healthy-looking `0` on a motor that had not moved.

### 7.3 Things the view must not display

- **A satellite count.** There is none anywhere in the schema; the frozen
  142-byte `UDP_message` does not carry one. `GnssReceiverStatus` field 9 is
  `reserved`, not reused. A console that shows "0 satellites" beside a valid
  position is worse than one that shows nothing.
- **`sdr.last_output_file`.** Field 6 of `SdrStatus`, declared, never populated
  by `transport.EncodeTelemetry`. Not a live value; not rendered.
- **Degrees converted.** `latitude_deg` and `longitude_deg` are degrees and are
  shown as degrees. The double conversion that shipped once (`ARCHITECTURE.md`
  §0, §6.1) is the reason `test/gnss_test.go` exists.

### 7.4 Load and temperature are bound by name

`AntennaTelemetry` numbers `load` 5 and `temperature_c` 6 — **swapped relative
to the shipped firmware header**. Both are `int32` and both survive a round
trip, so a mismatch yields a servo reporting −100 % load at 85 °C and nothing
fails. The view references generated Go field names, never numbers, and the
generation step is the only place a `.proto` is read.

### 7.5 Mocked subsystems

`system.mocked_subsystems` non-empty ⇒ a persistent banner naming each one, in
the words the wire uses. `gs_probe.py` called this the failure a Ground Station
most needs to avoid; it is still true of a GUI, which is easier to believe than
a terminal.

---

## 8. The bound surface

The browser talks to Go over the WebSocket bridge (bridge.go). The bridge
exposes Go methods as WebSocket RPCs and events as WebSocket pushes. This
list is a **contract**, in the same sense `internal/domain/ports.go` is: adding
to it is cheap, renaming is not.

Grouped by what the operator is doing, not by struct.

**Connection** — `Connect(cfg)`, `Disconnect()`, `LinkState()`, `Endpoints()`

**Telemetry** — pushed, not polled. `Snapshot()` exists once, for first paint
before the first event.

**Antenna / flight controller** — `SetHeading(deg)`, `Jog(servo, tick)`,
`ZeroServo(servo)`, `ZeroAll()`, `MountOffset(servo, deg)`,
`SetDirection(servo, mult)`, `SetHeater(id, on)`, `StopServo(servo)`,
`StopAll()`, `PicoStatusRequest()`

**GNSS** — `SelectReceiver(id)`, `RotateReceiver()`

**Camera** — `TakePhoto()`

**SDR** — `SdrProbe()`, `SdrGetParams()`, `SdrConnect()`, `SdrResetUSB()`, `SetSdrParams(patch)`

`SetSdrParams` names every key `connect.cpp` reads with `j.at()`, which is the
whole point of it: the sweep window (`t_min_us`, `t_max_us`), the arming delay
(`start_offset_s`) and the two RF paths (`tx_antenna`, `rx_antenna`) were read
by the program while being unreachable from the window, so the geometry of a
capture was a hand edit to `params.json` on the aircraft. They are inputs here
like the rest, blank still meaning "leave alone". `PULSE_DURATION` is
deliberately absent and must stay so: `config.hpp` has its read commented out,
so a control for it would save a value the program never looks at. See
`ARCHITECTURE.md` §6.4 for the bounds and the one cross-field check.

**Link** — `SetLinkLimit(kbit)`

**Artefacts** — `ListArtefacts(path)`, `DownloadArtefact(name, destPath)`,
`CancelDownload()`, `PreviewArtefact(name)`

**System** — `QueryStatus()`, `Version()`

Three absences, all deliberate:

- **`SystemReset` is not bound.** The dispatcher answers it `ERROR_UNSUPPORTED`
  (`internal/command/dispatcher.go:116`). A GUI button for a command that is
  guaranteed to fail is a button that trains the operator to ignore errors.
- **No generic `SendCommand(json)`.** The frontend never constructs a
  `CommandRequest` or touches a `oneof`. Every command is a named, typed method
  that validates its arguments host-side, exactly as `buildRequests` does in
  `tools/gs_cli/client.go:229`. A generic escape hatch would put a protobuf
  oneof in the browser, where a renumbering is a silent runtime failure.
- **No `RevealArtefact`.** Showing a file in the OS file manager needs either
  a platform API (a browser has none) or `os/exec` (forbidden to
  everything but `internal/sdr` and `internal/qos` by the layering test). The
  browser row copies the path to the clipboard instead, through the runtime's
  own clipboard -- no new bound method needed, because the frontend already
  speaks to that runtime directly.

### 8.1 Events

| Event | Rate | Payload |
| :--- | :--- | :--- |
| `telemetry:frame` | 1 Hz | `{view, gaps, restart, link}` — the §7 view plus what the stream noticed and link health at that instant |
| `command:result` | per command | request id, success, error code, message, artefact metadata |
| `link:state` | on frame, after each command, on connect/disconnect | §6.7 with age in seconds (never nanoseconds) |
| `download:progress` | ≤10 Hz | name, bytes, total (-1 when unknown), measured rate |
| `download:done` | per transfer | name, bytes, elapsed, rate, or the error |
| `log` | as logged | one line |

---

## 9. Frontend

Vanilla TypeScript, Vite. No UI framework, no state library, no protobuf.

### 9.1 No protobuf in the browser

Go decodes `TelemetryFrame` once, in `internal/client`, and `internal/gsview`
converts it to plain JSON structs. The frontend receives JSON.

This is the decision most worth defending, so: a TypeScript protobuf runtime
would be a **second decoder for the same wire format**, in a language with none
of §13's tests. The failure mode it introduces is precisely the one this
repository has already been bitten by — a schema that one side reads and the
other does not — except now the divergence is invisible to `go test`. The saving
is a few kilobytes and one npm dependency tree.

Enforced by a test (§13), because a rule nobody checks is a comment.

### 9.2 Rate

Telemetry is 1 Hz and arrives whole; render it whole. Download progress is the
only high-frequency event and is coalesced to ~10 Hz in Go — at 8 KiB/s the UI
cannot usefully show more, and a progress event per chunk on a marginal link is
one more thing competing with telemetry for H1.

### 9.3 Staleness in the UI

§6.7's rule is a frontend concern as much as a client one: on stale, disable
motion controls, dim the telemetry panels, and show the age. Do not blank the
panels — the last known values are still the best information available, and
their age is displayed alongside them.

### 9.4 What the frontend may not do

- Convert units. It receives degrees, metres, m/s, °C and renders them.
- Interpret absence. `gsview` has already decided what "no data" means; the
  frontend draws the distinction it is given and does not invent one.
- Retain a command it has sent. The reply arrives as an event, matched on
  request id.
- Open a native file dialog. GTK's file chooser aborts the whole process with
  SIGABRT when GSettings schemas are missing from the environment, which is
  uncatchable from Go (`git log` the `SaveFileDialog` removal for the trace).
  Destinations are typed into the window instead. This is not a styling choice;
  it is the only file picker that cannot kill the console.

---

## 10. Command safety

Motion commands — `Jog`, `SetHeading`, `Mount`, `SetDirection`, `Stop`,
`SetHeater`, `ZeroServo`, `ZeroAll` — require an explicit confirmation step.
Non-motion commands (`QueryStatus`, `TakePhoto`, `Probe`, `SelectReceiver`,
`SetLinkLimit`) do not.

`ZeroServo` and `ZeroAll` were missing from this list while the code already
gated both, and they are listed now on their own merits rather than as a
correction: `zero` writes a position offset into the servo's own EEPROM, which is
a write to hardware that outlives the board's power and outlives a reflash. It
does not *move* the axis — a teach relabels the tick the axis is already sitting
at — so it is not motion in the sense the rest of this list is about, but it is
durable and it is deliberate, and the confirmation is where an operator should be
told so. The prompt says exactly that, rather than implying the axis will return
somewhere.

**This is a decision against precedent in this repository, and the precedent is
recorded.** `tools/gs_cli/main.go:190`:

> There is no arm gate. That was removed deliberately: it added a step between an
> operator and a command without making anything safer, because the one consumer
> that had it was a bench tool.

`tools/pico_bench` has an arm gate and still does.

The `gs_cli` reasoning is sound for a terminal: `jog 1 3000` is a typed,
deliberate act with a visible echo in the scrollback, and a typo is caught
before it is sent. None of that transfers to a window. A jog is a drag of a
slider, or a click on a button that also says "Stop" somewhere else on screen;
there is no echo; and there is no undo. The failure the gate prevents — motion
the operator did not intend, on an antenna — is not one an extra keystroke was
ever going to prevent anyway, but it *is* one that a two-step click reduces.

The rule is written down rather than left to a widget's behaviour because the
next person will find the missing gate and assume it was an oversight.

`Jog` is additionally bounded: ticks outside 0–4095 are refused client-side, as
in `tools/gs_cli/client.go:313`. The firmware refuses them too, but refusing in
the console means nothing travels to the flight controller at all.

---

## 11. Configuration

`internal/config` resolves in the order `ARCHITECTURE.md` §11 mandates, and the
console uses the same `rocsar.toml`. One file, one resolution order, no second
parser.

### 11.1 `[client]` becomes live

`[client]` exists today and is consumed by **nothing** — `internal/config/config.go:68`
parses it and no code reads it. `tools/gs_cli` hardcodes
`tcp://192.168.1.50:5555` in its flags instead, which is the gap this closes.
`cmd/gs` reads it, and it is the first consumer the section was written for.

`client.http_endpoint` is **new**: the console needs the third address and there
is no key for it. Default `http://127.0.0.1:5557`.

### 11.2 `[gui]` is new

| Key | Default | Why |
| :--- | :--- | :--- |
| `window_width` | `1400` | |
| `window_height` | `900` | |
| `stale_after` | `3s` | §6.7. Three telemetry intervals. |
| `download_dir` | `$HOME/rocsar` | where fetches land |
| `confirm_motion` | `true` | §10. Configurable, defaulting to safe. |

Every key here goes into `config.KnownKeys` and gets a `case` in
`internal/config/parse.go`, or `test/config_test.go` fails — it parses the
shipped `rocsar.toml` and rejects any key not in `KnownKeys`.

### 11.3 No fifth source of precedence

The console does **not** add a UI-level override above flags. `ARCHITECTURE.md`
§11 fixes four levels and a fifth is a change to that document, not to this one.

A laptop that moves between aircraft gets a "Save connection" button that
**writes `rocsar.local.toml`** — the gitignored per-machine overlay that already
exists for exactly this. Configuration stays file-owned; the operator changes it
the same way for the OBC. A value typed into a text box and held in memory is a
second home for a fact, and `rocsar.toml`'s own header says why that is bad.

---

## 12. Build and toolchain

Verified on this machine, 2026-10-05, NixOS 26.11, before anything in this
document was written. The reasoning is here because two of the four findings
look like mistakes.

### 12.1 What is in the shell

`shell.nix` adds, in `nativeBuildInputs`:

| Package | Why |
| :--- | :--- |
| `nodejs` | Vite build of the frontend |
| `pkg-config` | cgo needs it to find libzmq |
| `gcc` | cgo for `go-zeromq/zmq4` |

And in `buildInputs`:

| Package | Why |
| :--- | :--- |
| `zeromq` | cgo for `go-zeromq/zmq4`, OBC **and** console |

There is no browser package. The operator's browser is not a build-time
dependency — it is a runtime fact of the laptop, like the display server.

### 12.2 No build tags

The `webkit2_41` build tag is gone with Wails. There is no webview package, no
cgo webview, and no tag to forget. `go build ./...` and `go test ./...` at the
repo root need only `libzmq` (for `zmq4` cgo) and nothing else.

This is load-bearing: **the OBC's build and test path stays free of the GUI's
dependencies.** A laptop that only needs to compile and flash the OBC does not
install a browser, a webkit or a node.

### 12.3 Commands

```
nix-shell                                  # the toolchain
buf generate                               # Go + Python + nanopb, §14
go build ./... && go test ./...            # the OBC and the shared packages
cd cmd/gs/frontend && npm run build        # frontend (tsc + vite)
go run ./cmd/gs                            # console: prints a URL, open it
./obc --mock-pico --mock-camera --mock-sdr # degraded OBC to point the console at
./tools/gs_cli watch                       # the terminal console, same client
```

For frontend development with hot reload:

```
go run ./cmd/gs &                          # Go server on :PORT
cd cmd/gs/frontend && npm run dev          # vite on :5173, proxies /ws to Go
```

---

## 13. How this is kept true

Additions to `test/layering_test.go` and new files in `test/`.

| Check | Guards against |
| :--- | :--- |
| `internal/client` added to the protobuf allowlist (§5.1) | the client codec moving somewhere without a test |
| `internal/gsview` added to the **forbidden** list (§5.1) | presentation logic acquiring a socket or a dependency on protobuf |
| `TestFrontendHasNoProtobuf` — greps `cmd/gs/frontend` for `.proto`, `protobuf`, `pbjs` | §9.1 quietly being undone by a well-meaning `npm i protobufjs` |
| `TestViewNeverInventsAZero` — feeds `internal/gsview` frames with every optional field absent and asserts each renders as absent | §7.1 regressing to proto3 defaults |
| `TestCommandSurfaceIsBound` — every `CommandRequest` variant is either a bound method or documented absent (§8) | a command existing on the wire and in no window |
| `test/client_test.go` — slow-joiner discard, request-id correlation, single-in-flight refusal, sequence gap and restart, independent SUB/DEALER reconnect | §6 |
| `test/gsview_test.go` — `feedback_state` 0/1/2 and an unknown value; fix suppressed when `fix_ok` false; degrees not converted | §7 |

Two gaps in `ARCHITECTURE.md` §12 are worth closing while the boundary test is
already open:

- **`TestFirmwareExcludesGroundStationTypes`** is named in `ARCHITECTURE.md` §12
  and in `scripts/generate.sh:31` as living in `test/layering_test.go`. **It does
  not exist.** The equivalent check is a `grep` inside `scripts/generate.sh`,
  which runs but is not a test.
- **The config-completeness test** (`ARCHITECTURE.md` §12, check 3) does not
  exist either. It becomes relevant here, because §11 adds keys.

---

## 14. Migration

### 14.1 Changes to existing files

| File | Change |
| :--- | :--- |
| `test/layering_test.go` | +`internal/client` in the protobuf allowlist; +`internal/gsview` in the forbidden list; +two new tests |
| `internal/config/config.go` | `KnownKeys` += `client.http_endpoint`, `gui.*`; defaults |
| `internal/config/parse.go` | a `case` per new key |
| `rocsar.toml` | `[client] http_endpoint`, new `[gui]` section |
| `tools/gs_cli/*` | reduced to a `main` over `internal/client` |
| `shell.nix` | GS toolchain (done, §12) |
| `ARCHITECTURE.md` | the eight amendments in §0 |

### 14.2 Deletion of the Python ground station

`gs/`, the `buf.gen.yaml` python plugin, `tools/gs_probe.py`, the python
packages in `shell.nix`, the `.gitignore` python entries, and the
`python -m pytest gs/` line in `ARCHITECTURE.md` §13.

Only stubs and a probe exist — there is no Python client, no view model and no
Qt application. `tools/gs_probe.py` was written to answer a real question
(`ARCHITECTURE.md` §7.2): could pyzmq, a completely different binding, speak to
this ZeroMQ implementation? It found the frame-envelope bug, and it earned its
place.

### 14.3 What deleting it costs

**Recorded because it is a real reduction, not a cleanup.**

Once both ends of the link are `go-zeromq/zmq4`, there is no second
implementation of the ZeroMQ frame layout in the repository. `test/` is Go
talking to Go, which proves our side is correct and says nothing about whether
any other binding agrees. That question is now unasked, and it is the question
that caught a shipped bug.

What replaces it: `internal/client` is exercised against a real ROUTER and PUB
in `test/`, and `tools/gs_cli` — same client, no webkit — remains a way to check
the wire by hand at any time.

### 14.4 Order of work

1. `shell.nix` — **done**.
2. Promote `tools/gs_cli` to `internal/client`; `gs_cli` becomes a thin main.
   No behaviour change; the suite stays green throughout.
3. Add the reconnect, slow-joiner, sequence-gap, single-in-flight and
   link-state work, with tests, in the client package.
4. `internal/gsview`, with tests.
5. `cmd/gs` skeleton: `wails.json`, `main.go`, `app.go`, an empty frontend.
   First `wails build` that produces a window.
6. Panels, then commands, then the artefact browser.
7. Config keys, then the `layering_test.go` changes, then the deletions.
8. The two missing tests from §13.

Steps 2–4 are ordinary Go with no GUI in them, and they are the ones that make
step 6 cheap. That ordering is deliberate: the interesting decisions in this
document are about what the console *refuses to claim*, and none of them need a
window to get right.

---

## 15. What is not covered

Stated so nobody mistakes silence for coverage.

- **Nothing here has been run against the live system.** The toolchain is
  verified (§12); `cmd/gs` has been pointed at a local mock OBC repeatedly,
  never at the aircraft. `tools/gs_cli` exists and is trusted, but a window
  has never commanded a servo that moves.
- **No hardware in the loop.** As `ARCHITECTURE.md` §14 says, for the OBC. It
  applies to the console, which has never seen a servo move.
- **The v2→v3 migration is unpriced.** If Wails v3 is wanted later, the work is
  confined to `cmd/gs` — `internal/client` and `internal/gsview` do not import
  Wails. That containment is the reason for §4's split, and it is a design
  intention, not a tested claim.
- **No accessibility or operator-ergonomics work.** Dark/light, fonts, colour
  coding for state, and the like are unaddressed. The one exception is §7's
  absence discipline, which is an accessibility property: "no data" must not be
  indistinguishable from a zero, by colour or by anything else.
- **Artefact integrity is still unverified.** No catalogue, no checksums — the
  same gap `ARCHITECTURE.md` §6.7 records for the OBC. A resumed download is
  checked for a plausible length and nothing more.
- **`decoded` does not mean `trusted`.** `internal/gsview` renders what the OBC
  sent. It cannot tell a correct reading from a plausible one, and nothing here
  claims otherwise.
- **The map is now in scope (§1.2) but its implementation is not built.** The
  requirements are stated and the toolkit question is open (§1.3–§1.4). The
  `telemetry:frame` event shape (`{view, gaps, restart}` with `view` per §7)
  is the contract the map builds against and will not move under it. Open
  questions -- tile sourcing for offline field use, projection of degrees,
  marker assets, click-to-point wiring -- belong to the map track, not to the
  console shell.
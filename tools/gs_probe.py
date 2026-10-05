#!/usr/bin/env python3
"""gs_probe -- the cross-language check on the OBC's ZMQ interface.

Everything else in tools/ is Go. This one is Python on purpose: it is the only
piece of code in the repository that talks to the OBC the way the Ground Station
will, and the only thing that can prove the two implementations actually agree.

The Go tests use go-zeromq/zmq4. This uses pyzmq, which is libzmq through a
completely different binding, with its own frame handling and its own idea of
what a DEALER sends. If the two disagree, the GS -- the only real consumer --
breaks, and no amount of Go testing finds it.

Usage:

    gs_probe.py                      listen for telemetry, then send a command
    gs_probe.py --listen             telemetry only, print and exit on Ctrl-C
    gs_probe.py --http http://host:5557
    gs_probe.py --take-photo         fetch the bytes and check they are a JPEG

Run it against the real OBC with mock hardware:

    ./obc --mock-pico --mock-camera --mock-sdr --config config/rocsar.toml
    ./tools/gs_probe.py
"""

from __future__ import annotations

import argparse
import os
import signal
import struct
import sys
import time
import urllib.error
import urllib.request

# `gs/` goes on the path, not the repo root. The generated modules import each
# other as `from rocsar.v1 import common_pb2` -- the package name comes from the
# .proto, not from the directory buf happened to write them into -- so the
# directory that has to be importable is the one CONTAINING `rocsar/`.
#
# Done here rather than by asking the operator to set PYTHONPATH, because a tool
# that only runs with the right environment is a tool that does not get run.
sys.path.insert(0, os.path.join(
    os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "gs"))

import zmq  # noqa: E402

from rocsar.v1 import command_pb2, common_pb2, telemetry_pb2  # noqa: E402

TOPIC_TELEMETRY = "telemetry"

# How long to wait for telemetry before saying so. A SUB socket that connects to
# a PUB that is not there does not error -- it just never delivers -- so silence
# is the failure mode and it needs a deadline or the tool hangs looking healthy.
LISTEN_TIMEOUT_S = 10.0
COMMAND_TIMEOUT_S = 10.0


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--control", default="tcp://127.0.0.1:5555", help="OBC ROUTER")
    ap.add_argument("--telemetry", default="tcp://127.0.0.1:5556", help="OBC PUB")
    ap.add_argument("--http", default="http://127.0.0.1:5557", help="OBC HTTP")
    ap.add_argument("--listen", action="store_true",
                    help="only read telemetry, do not send a command")
    ap.add_argument("--take-photo", action="store_true",
                    help="ask for a photograph and fetch the bytes")
    ap.add_argument("--timeout", type=float, default=COMMAND_TIMEOUT_S,
                    help="seconds to wait for a command reply")
    args = ap.parse_args()

    ctx = zmq.Context()
    # A poller rather than a bare recv: two sockets, and blocking on one means
    # silently not servicing the other.
    poller = zmq.Poller()

    sub = ctx.socket(zmq.SUB)
    sub.setsockopt_string(zmq.SUBSCRIBE, TOPIC_TELEMETRY)
    sub.connect(args.telemetry)

    dealer = None
    if not args.listen:
        dealer = ctx.socket(zmq.DEALER)
        dealer.connect(args.control)
        poller.register(dealer, zmq.POLLIN)

    poller.register(sub, zmq.POLLIN)

    print(f"telemetry {args.telemetry} topic '{TOPIC_TELEMETRY}'")
    print(f"control   {args.control}")
    print(f"artefacts {args.http}")
    print()

    try:
        telemetry = wait_for_telemetry(ctx, poller, sub)
        if telemetry is None:
            return 1

        if args.listen:
            return 0

        if args.take_photo:
            ok = do_take_photo(ctx, dealer, poller, args)
        else:
            ok = do_gnss_select(ctx, dealer, poller, args)
        return 0 if ok else 1
    finally:
        for s in (sub, dealer):
            if s is not None:
                s.close(linger=0)
        ctx.term()


# --------------------------------------------------------------------------
# Telemetry
# --------------------------------------------------------------------------

def wait_for_telemetry(ctx, poller, sub, timeout=LISTEN_TIMEOUT_S):
    """Wait for one telemetry frame and print it.

    Also proves the topic filter matches. SUB filtering is silent when it does
    not: the socket connects, delivers nothing, and reports nothing wrong. So a
    wrong topic string and a dead OBC look identical from the client, which is
    why the failure message below has to mention both.
    """
    deadline = time.monotonic() + timeout
    frames = 0

    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            print(f"no telemetry within {timeout:.0f}s", file=sys.stderr)
            print("  either the OBC is not running, or the topic filter is wrong.", file=sys.stderr)
            print(f"  expected topic '{TOPIC_TELEMETRY}'; a SUB that matches nothing", file=sys.stderr)
            print("  fails silently rather than reporting a mismatch.", file=sys.stderr)
            return None

        socks = dict(poller.poll(remaining * 1000))
        if sub not in socks:
            continue

        topic, body = sub.recv_multipart()
        frames += 1

        if topic.decode() != TOPIC_TELEMETRY:
            print(f"unexpected topic {topic!r}", file=sys.stderr)
            return None

        msg = telemetry_pb2.TelemetryFrame()
        try:
            msg.ParseFromString(body)
        except Exception as exc:  # noqa: BLE001
            # A parse failure here is the single most valuable thing this tool
            # can report: it means the two protobuf schemas disagree.
            print(f"telemetry is not a valid TelemetryFrame: {exc}", file=sys.stderr)
            print(f"  {len(body)} bytes arrived; the Python and Go schemas differ,", file=sys.stderr)
            print("  or this is not ROCSAR telemetry on this port.", file=sys.stderr)
            return None

        print_telemetry(msg, frames)
        return msg


def print_telemetry(f, frames):
    print(f"telemetry frame {frames}: sequence={f.sequence} "
          f"generated_at={f.generated_at_unix:.3f} bytes={f.ByteSize()}")

    sys_ = f.system
    up = int(sys_.uptime_s)
    print(f"  system       {state_name(sys_.state)}"
          f"  uptime {up // 3600}h{(up % 3600) // 60:02d}m{up % 60:02d}s"
          f"  cpu {sys_.cpu_temp_c:.1f} C")

    # Mocked subsystems are named ON THE WIRE. A Ground Station that renders a
    # fabricated reading as a real one is the failure this whole project is
    # organised around avoiding, so the probe says so loudly.
    if sys_.mocked_subsystems:
        print(f"  SIMULATED    {', '.join(sys_.mocked_subsystems)}"
              "   <- fabricated, not measured")

    for g in f.gnss:
        sel = "selected" if g.selected else "        "
        # Degrees. If this ever prints metres the conversion is applied twice;
        # it was, once, and the number was 111000x too large.
        if g.fix_ok:
            print(f"  gnss {g.receiver_id} {sel} {g.latitude_deg:11.7f}, "
                  f"{g.longitude_deg:11.7f}  alt {g.altitude_m:7.1f} m"
                  f"  {g.ground_speed_mps:5.1f} m/s  course {g.course_deg:5.1f}")
        else:
            print(f"  gnss {g.receiver_id} {sel} no fix"
                  "   <- stale or never received; not a position")

    if f.HasField("pico"):
        p = f.pico
        conn = "connected" if f.pico_connected else "NOT connected"
        print(f"  pico         {conn}")
        a = p.imu_acceleration
        o = p.imu_absolute_orientation
        m = p.imu_magnetic_field
        w = p.imu_angular_velocity
        print(f"    imu        acceleration ({a.x:.3f}, {a.y:.3f}, {a.z:.3f})"
              f"  orientation ({o.x:.3f}, {o.y:.3f}, {o.z:.3f})")
        print(f"               magnetic ({m.x:.3f}, {m.y:.3f}, {m.z:.3f})"
              f"  angular ({w.x:.3f}, {w.y:.3f}, {w.z:.3f})"
              f"  temperature {p.imu_temperature_c} C")
        if f.HasField("pico_last_ack"):
            a = f.pico_last_ack
            print(f"    last ack   seq {a.command_sequence}"
                  f"  {'accepted' if a.success else 'REFUSED: ' + error_name(a.error)}")
    else:
        print(f"  pico         {state_name(f.system.state)}"
              f"  (no telemetry yet)")

    if f.HasField("camera"):
        c = f.camera
        print(f"  camera       {state_name(c.state)}"
              + (f"  {c.photos_taken} photo(s)"
                 if c.HasField("photos_taken") else "  no photos yet"))

    if f.HasField("sdr"):
        d = f.sdr
        line = f"  sdr          {state_name(d.state)}"
        if d.running:
            line += f"  running pid {d.pid}"
        elif d.HasField("last_error"):
            line += f"  error: {d.last_error}"
        print(line)

    if f.HasField("link"):
        k = f.link
        shaping = "shaping on" if k.shaping_active else f"shaping OFF ({k.inactive_reason})"
        print(f"  link         {state_name(k.state)}  {k.device}"
              f"  {k.rate_kbps} kbit/s  {shaping}")


# --------------------------------------------------------------------------
# Commands
# --------------------------------------------------------------------------

def send_and_wait(ctx, dealer, poller, req, timeout):
    """Send a CommandRequest and return its CommandResponse.

    DEALER, not REQ. The OBC side is a ROUTER and never sends an empty
    delimiter frame; REQ/REP always does, and the extra frame means the reply is
    read one frame late. This is the same asymmetry documented in
    ARCHITECTURE.md section 7.2, seen from the other end.
    """
    dealer.send(req.SerializeToString())

    deadline = time.monotonic() + timeout
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            print(f"  no reply within {timeout:.0f}s", file=sys.stderr)
            return None

        socks = dict(poller.poll(remaining * 1000))
        if dealer not in socks:
            continue

        frames = dealer.recv_multipart()
        if len(frames) != 1:
            # The OBC replies with the payload alone. More than one frame means
            # somebody reintroduced a delimiter.
            print(f"  reply has {len(frames)} frames, want 1: "
                  f"{[f[:16] for f in frames]}", file=sys.stderr)
            return None

        resp = command_pb2.CommandResponse()
        try:
            resp.ParseFromString(frames[0])
        except Exception as exc:  # noqa: BLE001
            print(f"  reply is not a CommandResponse: {exc}", file=sys.stderr)
            return None

        if resp.request_id != req.request_id:
            # Should not happen on a DEALER pair, but if it does the reply does
            # not belong to this request and returning it would be a lie.
            print(f"  reply is for {resp.request_id!r}, asked for {req.request_id!r}",
                  file=sys.stderr)
            continue
        return resp


def do_gnss_select(ctx, dealer, poller, args):
    req = command_pb2.CommandRequest(
        request_id="gs-probe-gnss-1",
        gnss_select=command_pb2.GnssSelectCommand(receiver_id=1, rotate=True),
    )
    print(f"-> gnss_select receiver_id=1 rotate=True  (request_id {req.request_id!r})")
    resp = send_and_wait(ctx, dealer, poller, req, args.timeout)
    if resp is None:
        return False

    print(f"<- success={resp.success} error={error_name(resp.error)}"
          + (f" message={resp.message!r}" if resp.message else ""))
    # Selecting a receiver that is not answering is still a legitimate selection:
    # the bank is a configuration choice, and the operator selects a receiver
    # before plugging it in. What must NOT happen is a refusal reported as a
    # success, so the state after the switch is worth watching.
    print("  a refusal here means the command layer rejected the request")
    print("  (expected only if no GNSS receivers are bound at all)")
    return True


def do_take_photo(ctx, dealer, poller, args):
    req = command_pb2.CommandRequest(
        request_id="gs-probe-photo-1",
        take_photo=command_pb2.TakePhotoCommand(),
    )
    print(f"-> take_photo  (request_id {req.request_id!r})")
    resp = send_and_wait(ctx, dealer, poller, req, args.timeout)
    if resp is None:
        return False

    print(f"<- success={resp.success} error={error_name(resp.error)}"
          + (f" message={resp.message!r}" if resp.message else ""))
    if not resp.success:
        return False

    # The response names the artefact; the bytes are over HTTP. Carrying an image
    # in the ACK would put megabytes on a command channel sized for kilobytes.
    if not resp.HasField("artefact_name"):
        print("  the reply names no artefact, so there is nothing to fetch", file=sys.stderr)
        return False

    name = resp.artefact_name
    # The HTTP root IS the data directory: the handler trims a leading "/" and
    # resolves the rest against it, so the URL is /<artefact_name> with no
    # /artefacts prefix. It also refuses to escape the root, which is why the
    # name in the ACK is safe to paste straight in.
    url = f"{args.http}/{name}"
    print(f"   artefact {name}  {resp.artefact_size_bytes} bytes")
    print(f"   fetching {url}")

    try:
        with urllib.request.urlopen(url, timeout=args.timeout) as r:
            ctype = r.headers.get("Content-Type", "")
            body = r.read()
    except urllib.error.HTTPError as exc:
        print(f"  HTTP {exc.code} for {url}", file=sys.stderr)
        return False
    except urllib.error.URLError as exc:
        print(f"  cannot reach {url}: {exc.reason}", file=sys.stderr)
        return False

    print(f"   got {len(body)} bytes, Content-Type: {ctype}")

    # An HTTP 200 with the wrong bytes is the failure that matters. The server
    # claims image/jpeg; check that it is one.
    if not body.startswith(b"\xff\xd8\xff"):
        print("   NOT a JPEG -- the server sent something else", file=sys.stderr)
        return False
    if not body.rstrip(b"\x00").endswith(b"\xff\xd9"):
        print("   no JPEG end-of-image marker; the file is truncated", file=sys.stderr)
        return False

    # Dimensions from the SOF marker, so no image library is needed.
    w, h = jpeg_size(body)
    if w and h:
        print(f"   JPEG {w}x{h}  OK")
    else:
        print("   JPEG markers intact, dimensions not found")

    # Range support, because the GS will use it and a 200 for a Range request
    # means every resume re-downloads the whole file.
    try:
        req = urllib.request.Request(url, headers={"Range": "bytes=0-1"})
        with urllib.request.urlopen(req, timeout=args.timeout) as r:
            code, chunk = r.status, r.read()
        if code == 206 and len(chunk) == 2:
            print(f"   Range honoured: {code}, {len(chunk)} bytes")
        else:
            print(f"   Range returned {code} with {len(chunk)} bytes, want 206 and 2",
                  file=sys.stderr)
            return False
    except urllib.error.HTTPError as exc:
        print(f"   Range request failed: HTTP {exc.code}", file=sys.stderr)
        return False

    print("\nOK: command, artefact fetch and Range all behave")
    return True


def jpeg_size(body):
    """Width and height from a JPEG's SOF marker, or (None, None).

    Only SOF0/1/2/4/9/10 are frame headers. SOF4 (DHT) and SOF8 (JPG) and SOF12
    (DAC) share the marker prefix and are NOT sizes, which is why this checks the
    exact byte rather than the high nibble alone.
    """
    sof = {0xC0, 0xC1, 0xC2, 0xC3, 0xC5, 0xC6, 0xC7,
           0xC9, 0xCA, 0xCB, 0xCD, 0xCE, 0xCF}
    i = 2
    while i + 9 < len(body):
        if body[i] != 0xFF:
            i += 1
            continue
        marker = body[i + 1]
        if marker in sof:
            h, w = struct.unpack(">HH", body[i + 5:i + 9])
            return w, h
        if marker in (0xD8, 0x01) or 0xD0 <= marker <= 0xD7:
            i += 2
            continue
        seg = struct.unpack(">H", body[i + 2:i + 4])[0]
        i += 2 + seg
    return None, None


# --------------------------------------------------------------------------
# Enum names
# --------------------------------------------------------------------------
# Printed as names, not integers: `health=2` tells an operator nothing at 3am.

def state_name(v):
    try:
        return common_pb2.SubsystemState.Name(v)
    except ValueError:
        # An enum value this build does not know means the OBC is NEWER than the
        # probe. That is worth saying out loud -- it is the signature of a schema
        # skew, and guessing at it would be worse than naming it.
        return f"UNKNOWN({v}) -- the OBC is newer than this probe"


def error_name(v):
    try:
        return common_pb2.ErrorCode.Name(v)
    except ValueError:
        return f"UNKNOWN({v}) -- the OBC is newer than this probe"


if __name__ == "__main__":
    signal.signal(signal.SIGINT, lambda *_: (_ for _ in ()).throw(KeyboardInterrupt))
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        print()
        sys.exit(130)
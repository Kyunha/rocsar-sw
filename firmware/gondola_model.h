#pragma once

// Host-testable model of the gondola's kinematics and command handling.
//
// Nothing here touches hardware, which is the only reason the split exists: the
// kinematics, the command rules and the ST3215 status parser are plain functions
// over plain structs, so they can be compiled and driven on a host, and the .ino
// keeps only what needs pins, timers and the UART.
//
// The split is still worth maintaining without a test suite attached to it. A
// policy written in the .ino is a policy that can only be checked by flashing a
// board and reading a serial console, and nothing about this project runs that
// often enough to rely on. Enforcement is the compile gate and review; this file
// is arranged so that those are enough.

#include <math.h>
#include <stddef.h>
#include <stdint.h>

#include "pico.pb.h"

#define GEAR_RATIO 5.0f
#define TICKS_PER_DEGREE (4096.0f / 360.0f)  // ST3215 12-bit Encoder
#define NUM_ANTENNAS 2
#define SERVO_TICK_MAX 4095

// NUM_ANTENNAS and the generated telemetry array must agree, and the generated
// array's size comes from `antennas max_count:2` in api/rocsar/v1/pico.options
// -- a file nothing in the firmware reads. Bumping one and not the other was
// silent: the mapping loop below wrote antennas[i] for i < NUM_ANTENNAS, so a
// NUM_ANTENNAS above max_count wrote past the end of a struct it had just filled
// with init_zero. That is memory corruption on the wire, not a compile error.
//
// This assert covers the dangerous direction. The other direction (max_count
// raised to 3 with NUM_ANTENNAS left at 2) makes the third antenna silently
// unreportable, which no assert in this header can see. The static_assert below
// catches it at build time instead, which is the only enforcement there is.
static_assert(NUM_ANTENNAS <=
                  sizeof(((rocsar_v1_PicoTelemetry*)0)->antennas) /
                      sizeof(((rocsar_v1_PicoTelemetry*)0)->antennas[0]),
              "NUM_ANTENNAS exceeds the generated antennas array: "
              "rocsar.v1.PicoTelemetry.antennas max_count in "
              "api/rocsar/v1/pico.options");

// EMA weight for the gondola heading. It lives here rather than in the sketch
// because applyImuHeading() below is where it is used. A second filter declared
// here would stack with that one: two EMAs give an effective weight of
// 1-(1-a)^2, which is 0.28 and not the 0.15 written down here, and nobody reading
// either file would see it.
#define IMU_ALPHA 0.15f

// How often the sketch re-probes for a BNO055 that was absent at boot. The
// sensor is optional on a bench, so "not fitted" is expected rather than fatal,
// and re-probing means fitting one later is picked up without a reflash.
#define IMU_REPROBE_MS 1000

// Consecutive getEvent() failures tolerated before a sensor that was answering
// is declared absent. 50 at the shipped 50 Hz control tick is about a second --
// long enough that one dropped read on a busy I2C bus changes nothing, short
// enough that a sensor unplugged mid-run stops claiming to be a source of
// heading within a second instead of holding its last bearing forever while the
// wire still says imu_present.
//
// Named, because it used to be the kind of number that would be written inline
// and then re-derived by whoever read the code. The re-probe (IMU_REPROBE_MS)
// runs once this is reached, so fitting or re-seating a sensor is picked up
// without a reflash.
#define IMU_MISSED_SAMPLES_MAX 50

// How long a heater stays on without a fresh command.
//
// This is the firmware half of a dead-man. The host (internal/command/
// heater_keeper.go) re-sends an acknowledged heater-on every
// HeaterRefreshInterval (10 s), which restarts this clock; anything else --
// a dead host, a cut cable, a reboot -- stops the refreshes and the heater
// goes off within one window of itself. It is here rather than in the sketch
// because it is command-handling policy, and policy in the .ino can only be
// checked by flashing a board.
//
// The two constants are a pair: 10 s refreshes fit twice into 20 s, so one
// lost refresh does not flicker the heater. TestFirmwareHeater asserts that
// relationship so neither side can be halved alone.
#define HEATER_AUTO_OFF_MS 20000UL

// The servo ids each antenna axis speaks to on the ST3215 bus. Overridable at
// build time (arduino-cli --build-property compiler.cpp.extra_flags=-DANTENNA_0_SERVO_ID=3)
// so a bench rig with different servo ids needs no source change; the defaults
// are the flight configuration. They are 1 and 2: the servos are the only two on
// the bus, and a firmware that addresses 5/10 would be talking to nobody.
//
// Overridable at build time for a bench with different addressing:
//   arduino-cli compile --build-property
//     compiler.cpp.extra_flags=-DANTENNA_0_SERVO_ID=3 firmware/
// Check what the board actually reports with `pico_bench status` before
// assuming these are right -- an axis that never appears in telemetry with
// FEEDBACK_MEASURED is almost always an id mismatch rather than a dead servo.
#ifndef ANTENNA_0_SERVO_ID
#define ANTENNA_0_SERVO_ID 1
#endif
#ifndef ANTENNA_1_SERVO_ID
#define ANTENNA_1_SERVO_ID 2
#endif

#ifndef DEG_TO_RAD
#define DEG_TO_RAD 0.017453292519943295769
#endif
#ifndef RAD_TO_DEG
#define RAD_TO_DEG 57.29577951308232087680
#endif

inline float wrap360(float deg) {
  deg = fmodf(deg, 360.0f);
  return (deg < 0.0f) ? deg + 360.0f : deg;
}

inline float wrap180(float deg) {
  float rad = deg * DEG_TO_RAD;
  return atan2f(sinf(rad), cosf(rad)) * RAD_TO_DEG;
}

inline uint16_t clampTick(int32_t tick) {
  if (tick < 0) return 0;
  if (tick > SERVO_TICK_MAX) return SERVO_TICK_MAX;
  return (uint16_t)tick;
}

// ============================================================================
// ST3215 SERVO BUS
// ============================================================================
// Length of one STS3215 write-position packet: 2 header bytes, id, length,
// control, address, the 16-bit position little-endian, 4 reserved bytes, and
// the checksum.
#define SERVO_PACKET_LEN 13

// Builds the STS3215 "write goal position" packet (address 42) into `out` and
// returns its length.
//
// It is here rather than in the sketch for the same reason as everything else
// in this file: the sketch can only be read, not driven. This function *was*
// inline in the .ino, which meant the checksum loop, the little-endian position
// and the 0xFF 0xFF header were covered by nothing but an eyeball -- and a
// checksum that ignored one byte, or a position sent big-endian, is a servo
// that silently does not move on a bench with no test able to notice.
//
// `position` is clamped on the way in, so the caller cannot send an out-of-range
// tick even by accident.
inline size_t buildServoPacket(uint8_t id, int32_t position, uint8_t* out,
                               size_t outLen) {
  if (out == nullptr || outLen < SERVO_PACKET_LEN) {
    return 0;
  }

  uint16_t tick = clampTick(position);

  out[0] = 0xFF;
  out[1] = 0xFF;
  out[2] = id;
  out[3] = 0x09;  // packet length
  out[4] = 0x03;  // control bit (write)
  out[5] = 42;    // STS goal position register
  out[6] = (uint8_t)(tick & 0xFF);
  out[7] = (uint8_t)((tick >> 8) & 0xFF);
  out[8] = 0x00;  // reserved
  out[9] = 0x00;
  out[10] = 0x00;
  out[11] = 0x00;

  // The checksum is the ones-complement of the sum from the id through the last
  // reserved byte: bytes [2, 11]. The two 0xFF header bytes are excluded.
  uint16_t sum = 0;
  for (int i = 2; i < SERVO_PACKET_LEN - 1; i++) {
    sum += out[i];
  }
  out[SERVO_PACKET_LEN - 1] = (uint8_t)(~sum & 0xFF);

  return SERVO_PACKET_LEN;
}

// How far a target tick must move before it is worth waking the servo bus for.
// One tick is 0.088 degrees, so re-sending every 20 ms would put 50 identical
// frames/s on a 115200 bus for no mechanical gain.
#define SERVO_DEADBAND_TICKS 2

inline bool shouldSendServoTick(uint16_t lastSent, uint16_t target) {
  int32_t delta = (int32_t)target - (int32_t)lastSent;
  if (delta < 0) delta = -delta;
  return delta > SERVO_DEADBAND_TICKS;
}

// ============================================================================
// ST3215 SERVO STATUS READ (P6)
// ============================================================================
// The receive half of the servo bus, which did not exist in any file or language
// before this: `Serial1` was only ever `write`/`flush`/`setTX`/`setRX`/`begin`,
// so byte accumulation, resynchronisation, checksum verification and per-servo
// correlation all had to be written. It is here rather than in the sketch for
// the reason `applyCommand()` is here -- the sketch can only be read, not
// driven, and the same "logic in hardware, no coverage" shape is what let the
// uninitialised-IMU defect survive a green suite.
//
// Framing, from `tools/ST3215_Configure/ST3215_Configure.ino` on the bench
// (2026-10-02): INST_READ (0x02) on register 0x38 for 8 bytes, answered by a
// 14-byte frame carrying the status block.
//
//     FF FF | ID | LEN | ERR | POS_L POS_H | SPD_L SPD_H | LOAD_L LOAD_H | VOLT | TEMP | CHK
//            0    1    2    3    4         5-6         7-8          9-10       11    12    13
//
// The bus is half-duplex -- TX and RX are the same conductor -- so the Pico
// hears itself and every request is followed by an 8-byte echo of itself. That
// is a measured hardware fact, not an assumption: the reference sketch depends
// on it and reads telemetry correctly.
//
// The parser below is built so that fact stops mattering. Echo and reply differ
// in their LEN byte -- the 8-byte request echoes with LEN=0x04, the 14-byte
// status reply carries LEN=0x0A -- so a scanner that requires LEN=0x0A skips
// the echo structurally instead of counting bytes and hoping. That also makes it
// immune to a write ACK landing in the same window, which a fixed
// "drain N bytes, then read M" sequence cannot be: the drain discards whatever
// happens to be in flight, real data included.
#define SERVO_INST_READ 0x02
#define SERVO_REG_TELEMETRY 0x38
#define SERVO_STATUS_REQUEST_LEN 8
#define SERVO_STATUS_REPLY_LEN 14
#define SERVO_STATUS_REPLY_LEN_FIELD 0x0A
#define SERVO_STATUS_PARAM_LEN 8

// How long the sketch waits for a status reply before giving up on this poll,
// and how much RX it will hold while waiting.
//
// Sized from the wire, not guessed: a 14-byte reply is about 1.2 ms at
// SERVO_BAUD, so 5 ms is roughly four times the answer and is reached only by a
// servo that has stopped answering.
//
// The buffer is sized for a burst, not for one poll. One poll arrives as an
// 8-byte echo of the request plus a 14-byte reply (22 bytes), and the previous
// poll's pair can still be in the RX FIFO when the next request goes out --
// two polls in flight is 44 bytes, which is more than the 40 this used to be.
// A full buffer stops being read (`filled < sizeof(buffer)`), so the bytes pile
// up in the UART's FIFO and the next poll reads a window that starts mid-frame.
// 64 is two full bursts with room for a third.
//
// These live here rather than in the sketch because they are part of the framing
// contract, and a #define inside the .ino can only be read, not checked against
// the code that uses it.
#define SERVO_STATUS_WAIT_MS 5
#define SERVO_STATUS_BUFFER_LEN 64

// The longest frame this bus produces, in bytes, used to tell a slow arrival from
// a corrupt length byte. The ST3215 instruction set tops out well below this: the
// longest real packet is a block read of a few registers, around 14 bytes.
#define SERVO_MAX_FRAME_LEN 32

// Bytes in a frame that the LEN field does not count: the two 0xFF header bytes
// and the LEN byte itself. A frame is therefore LEN + 4 bytes. Pinned here
// because getting it wrong does not crash -- the scanner simply lands mid-frame
// and resynchronises on the next header, which looks like it working.
#define SERVO_FRAME_OVERHEAD 4

// Byte offsets in the status reply. Named because every one of these was
// previously a bare literal, and the ERR index in particular was the byte the
// reference sketches parsed past.
#define SERVO_RX_ID 2
#define SERVO_RX_LEN 3
#define SERVO_RX_ERR 4
#define SERVO_RX_POS_LO 5
#define SERVO_RX_POS_HI 6
#define SERVO_RX_LOAD_LO 9
#define SERVO_RX_LOAD_HI 10
#define SERVO_RX_TEMP 12
#define SERVO_RX_CHECKSUM 13

// Load: the status register is a signed 16-bit count over +/-1000, scaled here
// to percent of rated torque.
//
// UNVERIFIED, and the reason is worth recording rather than burying. The manual
// this was supposed to come from is not in the tree: Docs/ST3215_ProtocolManual.pdf
// is a 5-page Joy-IT RB-Heatsink5 cooling-unit manual for a Raspberry Pi 5 and
// does not mention servos. This is the widely-published STS3215 control-table
// value. The bench settles it in one step -- READ <id> at rest (expect ~0) and
// against a mechanical stop (expect the rail, +/-100) -- and if it disagrees,
// this constant is the only thing that changes.
#define SERVO_LOAD_PERCENT_SCALE 10.0f

// Whether an axis's reported position/load/temperature are a measurement is
// rocsar_v1_FeedbackState, from common.pb.h -- not a local enum.
//
// This used to declare its own ServoFeedbackState with the same three values. It
// was the same definition in two places, which is a drift waiting to happen: the
// firmware would report MEASURED as 1 and the Ground Station would read the
// schema's 1, and the day someone reordered the local copy every axis would
// silently claim to be a measurement or a held value with nothing on the wire
// saying otherwise.
//
// The reason the wire field is int32 rather than this enum is unchanged and is
// set out in common.proto: an unrecognised value from a future firmware must
// decode to a plain integer on an old host instead of failing the whole frame.
// So the cast at the point of assignment stays, and the enum names come from
// the schema.
typedef rocsar_v1_FeedbackState ServoFeedbackState;

// One parsed status reply.
struct ServoStatus {
  uint16_t positionTicks;
  float loadPercent;
  int32_t temperatureC;
  uint8_t error;
};

// The ST3215 checksum: ones' complement of bytes 2 through len-2. The two 0xFF
// header bytes are excluded, which is the part that is easy to get wrong --
// including them yields a checksum that validates nothing.
inline uint8_t servoChecksum(const uint8_t* frame, size_t len) {
  uint16_t sum = 0;
  for (size_t i = 2; i + 1 < len; i++) {
    sum += frame[i];
  }
  return (uint8_t)(~sum & 0xFF);
}

// Builds the status-read request for one servo into `out`, returning its length.
//
// The mirror of `buildServoPacket()`, and for the same reason: the sketch cannot
// be unit tested, and a status request with a wrong length byte reads back
// whatever the servo felt like sending.
inline size_t buildServoStatusRequest(uint8_t id, uint8_t* out, size_t outLen) {
  if (out == nullptr || outLen < SERVO_STATUS_REQUEST_LEN) {
    return 0;
  }
  out[0] = 0xFF;
  out[1] = 0xFF;
  out[2] = id;
  out[3] = 0x04;
  out[4] = SERVO_INST_READ;
  out[5] = SERVO_REG_TELEMETRY;
  out[6] = SERVO_STATUS_PARAM_LEN;
  out[7] = servoChecksum(out, SERVO_STATUS_REQUEST_LEN);
  return SERVO_STATUS_REQUEST_LEN;
}

// What one `scanServoStatus()` call did.
struct ServoScan {
  // Bytes to drop from the front of the buffer before the next call. Always
  // advances past anything consumed, including discarded frames, so a caller
  // that only ever reads this can never wedge on a byte it refuses to accept.
  size_t consumed;
  // A complete, checksum-valid reply for the requested servo was parsed.
  bool matched;
  ServoStatus status;
};

// Scans forward in a byte stream for one servo's status reply.
//
// Resynchronises on the 0xFF 0xFF header, so it tolerates line noise, a
// half-received frame, and a stale echo all in the same buffer. It requires
// LEN=0x0A, which is what makes it immune to the echo (LEN=0x04) and to write
// ACKs (LEN=0x03) without needing to know how many bytes to discard.
//
// A reply for a *different* servo is consumed and skipped rather than treated
// as an error: both antennas share one bus, so the other axis's reply arrives
// interleaved with this one's and throwing it away would desynchronise nothing
// useful while losing the frame we did want.
inline ServoScan scanServoStatus(const uint8_t* buf, size_t len,
                                 uint8_t expectedId) {
  ServoScan scan;
  scan.consumed = 0;
  scan.matched = false;
  scan.status.positionTicks = 0;
  scan.status.loadPercent = 0.0f;
  scan.status.temperatureC = 0;
  scan.status.error = 0;

  if (buf == nullptr) return scan;

  size_t i = 0;
  while (i < len) {
    // Resynchronise: hunt for a header.
    if (buf[i] != 0xFF || (i + 1) >= len || buf[i + 1] != 0xFF) {
      i++;
      continue;
    }

    // A header plus a length byte is the minimum needed to know what this frame
    // claims to be. Anything less is a header still arriving.
    if (i + 4 > len) {
      scan.consumed = i;
      return scan;
    }

    uint8_t declared = buf[i + SERVO_RX_LEN];

    if (declared != SERVO_STATUS_REPLY_LEN_FIELD) {
      // Not a status reply: our own request echoed back (LEN=0x04), a write ACK
      // (LEN=0x03), or another instruction's reply. Skip the whole frame by its
      // declared length, which is what makes this immune to the echo -- there is
      // no byte count to be wrong about.
      //
      // The order here matters and the obvious version of this function gets it
      // wrong. Checking "do I have 14 bytes yet?" *first* treats a complete
      // 8-byte echo as a half-arrived status frame and holds it forever, so a
      // servo that has gone offline accumulates an echo per poll that nothing
      // ever consumes until the buffer is full and the RX FIFO starts backing up
      // behind it. Reading the length byte first means a short frame is
      // recognised as short and retired on the first pass.
      //
      // The frame length is LEN + 4, not LEN + 2: the LEN field counts the bytes
      // that follow *itself*, so a frame is the two header bytes, the length
      // byte, and then LEN more. Getting this wrong lands the scanner in the
      // middle of every frame it skips, which still tends to recover by hunting
      // for the next header -- so it looks like it works -- but it will happily
      // resynchronise onto a 0xFF 0xFF that happens to occur inside a payload.
      size_t frameLen = (size_t)declared + SERVO_FRAME_OVERHEAD;

      // A declared length beyond the longest frame this bus produces is
      // corruption, not a slow arrival. Step one byte to resynchronise instead of
      // holding a frame that will never complete.
      if (frameLen > SERVO_MAX_FRAME_LEN) {
        i++;
        continue;
      }
      if (i + frameLen > len) {
        // Genuinely incomplete; the rest may still be on the wire.
        scan.consumed = i;
        return scan;
      }
      i += frameLen;
      continue;
    }

    // From here the frame claims to be a status reply, so it is exactly 14 bytes.
    if (len - i < SERVO_STATUS_REPLY_LEN) {
      scan.consumed = i;
      return scan;
    }

    if (buf[i + SERVO_RX_CHECKSUM] != servoChecksum(buf + i, SERVO_STATUS_REPLY_LEN)) {
      // Checksum-valid framing that is not a valid frame: a collision inside
      // the payload, or noise. Step past the header so the scan continues from
      // inside this frame rather than re-testing these bytes forever.
      i += 2;
      continue;
    }

    uint8_t id = buf[i + SERVO_RX_ID];
    if (id != expectedId) {
      // A good frame for the other antenna. Consume it and keep looking.
      i += SERVO_STATUS_REPLY_LEN;
      continue;
    }

    scan.matched = true;
    scan.status.positionTicks =
        (uint16_t)(buf[i + SERVO_RX_POS_LO] | (buf[i + SERVO_RX_POS_HI] << 8));
    int16_t loadRaw =
        (int16_t)(buf[i + SERVO_RX_LOAD_LO] | (buf[i + SERVO_RX_LOAD_HI] << 8));
    scan.status.loadPercent = (float)loadRaw / SERVO_LOAD_PERCENT_SCALE;
    scan.status.temperatureC = buf[i + SERVO_RX_TEMP];
    scan.status.error = buf[i + SERVO_RX_ERR];
    scan.consumed = i + SERVO_STATUS_REPLY_LEN;
    return scan;
  }

  scan.consumed = len;
  return scan;
}

struct AntennaAxis {
  uint8_t id;
  uint16_t centerTick;
  float mountOffsetDeg;
  float dirMultiplier;

  bool manualMode;
  uint16_t manualTick;
  uint16_t lastSentTick;

  // The boot interlock: set at power-up and cleared by the first command that
  // expresses an intent for *this* axis -- set_target (both axes) or jog. While
  // it is set, shouldDriveServo() transmits nothing, so a board that boots
  // holds its mechanical position instead of driving to whatever the default
  // geometry computes.
  //
  // That default is not a neutral one. target_heading_deg boots at 0, the
  // mount offset is 270 degrees and the gear ratio is 5:1, so for four bearings
  // out of five the first thing the firmware does on power-up is run an axis to
  // the clamp at the rail. stop/zero/mount/dir deliberately do NOT clear this:
  // none of them asks the servo to go anywhere, so none of them should be able
  // to move an axis that has never been commanded.
  bool awaitingCommand;

  // Telemetry Feedback Fields
  uint16_t currentTick;
  float currentAngleDeg;
  float temperatureC;
  float load;

// Whether the four fields above are a measurement. See FeedbackState in
// common.proto for why there are three states and not a bool.
  //
  // This is the state P6 exists to add. Before it, `currentTick` was the
  // last-commanded tick and `load`/`temperatureC` were constants written once at
  // init, so "the servo is stalled at zero load" and "nobody has ever asked the
  // servo anything" were the same three numbers on screen.
  ServoFeedbackState feedbackState;
  // The ST3215's own status byte from the last good read. Independent of
  // `feedbackState`: a servo can answer perfectly while reporting overheat.
  uint8_t feedbackError;

  void init(uint8_t servoId, uint16_t center, float mountOffset, float dir) {
    id = servoId;
    centerTick = center;
    mountOffsetDeg = mountOffset;
    dirMultiplier = dir;
    // Conservative boot: held in manual mode at centre, interlocked, and never
    // sent -- so the first loop tick computes a target, declines to transmit
    // it, and reports the posture rather than a position. See awaitingCommand.
    manualMode = true;
    manualTick = center;
    lastSentTick = 0xFFFF;
    awaitingCommand = true;
    currentTick = center;
    currentAngleDeg = 0.0f;
    temperatureC = 0.0f;
    load = 0.0f;
    feedbackState = rocsar_v1_FeedbackState_FEEDBACK_UNKNOWN;
    feedbackError = 0;
  }

  uint16_t calculateTargetTick(float gondolaHeading, float targetHeading) const {
    if (manualMode) return manualTick;

    // Seam guard. applyImuHeading() below refuses to integrate a non-finite
    // sample, so the heading cannot go NaN from the sensor -- but a target
    // arrives over the wire and a NaN there would reach the (int32_t) cast,
    // which is undefined behaviour on this target and produced a garbage tick.
    // Centring is a defined, in-range answer to "the commanded geometry is
    // meaningless"; holding the last position would imply a confidence about
    // where the antenna points that we do not have.
    if (!isfinite(gondolaHeading) || !isfinite(targetHeading)) {
      return centerTick;
    }

    float antennaBaseWorld = wrap360(gondolaHeading + mountOffsetDeg);
    float antennaRelativeAngle = wrap180(targetHeading - antennaBaseWorld);
    float servoAngleOffset = antennaRelativeAngle * GEAR_RATIO * dirMultiplier;

    int32_t tickOffset = (int32_t)(servoAngleOffset * TICKS_PER_DEGREE);
    return clampTick((int32_t)centerTick + tickOffset);
  }
};

// Whether an axis may put a position on the bus this control tick.
//
// Two conditions in one predicate, so the sketch cannot apply one and forget
// the other. The boot interlock first: an axis that has never been commanded
// transmits nothing, which is what makes power-up silent (see
// AntennaAxis::awaitingCommand). Then the deadband, which is bus courtesy --
// one tick is 0.088 degrees, so re-sending every 20 ms would put 50 identical
// frames/s on a 115200 bus for no mechanical gain.
//
// Note the interlock does not swallow the first command. lastSentTick is still
// 0xFFFF (never sent) when it clears, and every tick differs from 0xFFFF by
// more than the deadband -- so the first commanded position is transmitted even
// when it happens to equal the position the model believes it holds, which it
// may well not: the servo's real position at boot is unknown.
inline bool shouldDriveServo(const AntennaAxis& axis, uint16_t targetTick) {
  return !axis.awaitingCommand &&
         shouldSendServoTick(axis.lastSentTick, targetTick);
}

// Derives the position fields an operator reads, from what was last commanded.
//
// This is the *fallback*, and it only runs while there is no measurement.
//
// Once `applyServoFeedback()` has landed a real reading, `feedbackState` is no
// longer FEEDBACK_UNKNOWN and this function does nothing, because overwriting a
// measurement with the command that asked for it is the bug this whole path
// exists to remove: the
// display would show "the servo is exactly where I told it to be" for a motor
// that has been stalled against a stop for a minute. The derivation stayed here
// rather than moving into the sketch so that both the fallback and the real
// mapping are in one testable place, and so this file remains the single answer
// to "what does current_tick mean".
//
// Called after `lastSentTick` is updated, so the fallback reflects the tick the
// servo was actually asked for, including one held back by the deadband.
inline void updateAxisFeedback(AntennaAxis& axis) {
  // Stand down for both MEASURED and HELD: a held reading is still a real
  // reading, and overwriting it with the command that asked for it is the bug this
  // whole path exists to remove.
  if (axis.feedbackState != rocsar_v1_FeedbackState_FEEDBACK_UNKNOWN) {
    return;
  }
  // 0xFFFF is "never sent", not a position: the servo's range tops out at 4095,
  // so publishing it would read as 65535 ticks -- about 5580 degrees off centre,
  // with feedback_state=UNKNOWN as the only hint that it is fiction. The axis
  // that has never been commanded is held at centre (that is the boot posture),
  // so centre is what the fallback reports until a real reading lands.
  axis.currentTick =
      (axis.lastSentTick == 0xFFFF) ? axis.centerTick : axis.lastSentTick;
  axis.currentAngleDeg =
      ((float)axis.currentTick - (float)axis.centerTick) / TICKS_PER_DEGREE;
}

// Folds one parsed status reply into the axis an operator reads.
//
// This is what turns `currentTick` from an echo into a measurement, and it is
// deliberately the only writer of FEEDBACK_MEASURED.
inline void applyServoFeedback(AntennaAxis& axis, const ServoStatus& status) {
  axis.feedbackState = rocsar_v1_FeedbackState_FEEDBACK_MEASURED;
  axis.feedbackError = status.error;
  axis.currentTick = status.positionTicks;
  axis.currentAngleDeg =
      ((float)axis.currentTick - (float)axis.centerTick) / TICKS_PER_DEGREE;
  axis.load = status.loadPercent;
  axis.temperatureC = (float)status.temperatureC;
}

// Marks the axis's readings as held rather than measured.
//
// Called when a status read does not come back. Note what it does *not* do: it
// does not zero `load`, `temperatureC` or `currentTick`. Writing a zero would
// be a lie of exactly the kind AGENTS.md is about -- a constant carries no
// information, so a servo that stopped answering would look identical to a servo
// reading a genuine zero load, and the axis would report a healthy stall it
// cannot have measured. The last real reading is kept and the bit is cleared, so
// the wire says "held" and the operator decides what to do about it.
inline void invalidateAxisFeedback(AntennaAxis& axis) {
  // HELD only if something was actually measured before. An axis that has never
  // been spoken to stays UNKNOWN, because "we asked for 3000 and never found out
  // where it went" is not the same claim as "the encoder was last seen at 3000",
  // and this is the one place the difference is still knowable -- by the time the
  // state reaches the wire, it does not exist any more.
  if (axis.feedbackState == rocsar_v1_FeedbackState_FEEDBACK_MEASURED) {
    axis.feedbackState = rocsar_v1_FeedbackState_FEEDBACK_HELD;
  }
}

struct GondolaState {
  AntennaAxis antennas[NUM_ANTENNAS];
  float gondolaHeading;
  float targetHeading;
  bool heater1;
  bool heater2;

  // When each heater last received an "on" command, in millis(). The dead-man
  // clock behind HEATER_AUTO_OFF_MS; see noteHeaterCommand() and
  // expireHeaters() below. Unused while the heater is off.
  unsigned long heater1LastOnMs;
  unsigned long heater2LastOnMs;

  // Whether the heading source is currently real. It is false for a Pico with
  // no BNO055 fitted, which is the actual state of every bench unit so far --
  // so `gondolaHeading` is then a *held* bearing, not a measurement, and this
  // bit is what tells the operator the difference. It rides the wire as
  // PicoTelemetry.imu_present; see sendTelemetryMessage() in the sketch.
  bool imuPresent;

  // Consecutive getEvent() failures since the sensor last answered. Drives the
  // IMU_MISSED_SAMPLES_MAX declaration in noteImuMiss(); reset by any answered
  // read (clearImuMisses) and by reaching the threshold itself.
  uint16_t imuMissCount;
};

// Projects the model onto the wire message. The whole mapping lives here so the
// this function and the OBC's decoder are the two ends of the same contract, and a
// field that exists in one and is filled differently in the other is visible only
// as a plausible-looking number in the console.
//
// The caller owns the envelope (sequence, timestamp, which_payload) -- those
// come from the sketch's clock and transport -- and is responsible for sending
// the result. `out` is assumed to be init_zero, which is what nanopb does when
// the enclosing PicoMessage is.
inline void fillTelemetryMessage(const GondolaState& state,
                                 rocsar_v1_PicoTelemetry& out) {
  out.gondola_heading_deg = state.gondolaHeading;
  out.target_heading_deg = state.targetHeading;
  out.heater1_state = state.heater1;
  out.heater2_state = state.heater2;
  // Report the measurement, not the value: gondola_heading_deg is a *held*
  // bearing whenever imu_present is false, and the Pi and the GUI both need to
  // say so rather than present a frozen heading as a live one. This is the same
  // reason a blank frame must not be read as real zeros.
  out.imu_present = state.imuPresent;

  out.antennas_count = NUM_ANTENNAS;
  for (int i = 0; i < NUM_ANTENNAS; i++) {
    const AntennaAxis& axis = state.antennas[i];
    rocsar_v1_AntennaTelemetry& entry = out.antennas[i];

    entry.servo_id = axis.id;
    entry.manual_mode = axis.manualMode;
    entry.current_tick = axis.currentTick;
    entry.current_angle_deg = axis.currentAngleDeg;
    // Truncated to whole degrees because the wire field is an integer: the
    // ST3215 reports 0.1 degrees, and a tenth of a degree is far finer than
    // anyone acts on. The load field is likewise an integer count.
    entry.temperature_c = (int32_t)axis.temperatureC;
    entry.load = (int32_t)axis.load;
    entry.center_tick = axis.centerTick;
    entry.mount_offset_deg = axis.mountOffsetDeg;
    entry.dir_multiplier = axis.dirMultiplier;

    // The measurement flags, and they travel with the numbers rather than beside
    // them. `current_tick` above is either the encoder or the last command, and
    // `load`/`temperature_c` are either readings or values held since the last
    // time the servo answered -- so a consumer that renders them without reading
    // these two bits renders a held value as a live one. That is the exact
    // defect this flag was added to close, and it is why they are set here rather
    // than left to the caller.
    entry.feedback_state = (int32_t)axis.feedbackState;
    entry.feedback_error = (int32_t)axis.feedbackError;
  }
}

// Outcome of one heading sample, so a caller can tell a real reading from a
// held one without inspecting the state.
enum ImuSample {
  IMU_SAMPLE_APPLIED = 0,   // a real sample was integrated
  IMU_SAMPLE_NO_SENSOR = 1, // no IMU; the bearing was held
  IMU_SAMPLE_REJECTED = 2,  // sensor present but the sample was NaN/inf
};

inline void initGondolaState(GondolaState& state) {
  state.antennas[0].init(ANTENNA_0_SERVO_ID, 2048, 270.0f, -1.0f);
  state.antennas[1].init(ANTENNA_1_SERVO_ID, 2048, 270.0f, -1.0f);
  state.gondolaHeading = 0.0f;
  state.targetHeading = 0.0f;
  state.heater1 = false;
  state.heater2 = false;
  state.heater1LastOnMs = 0;
  state.heater2LastOnMs = 0;
  state.imuPresent = false;
  state.imuMissCount = 0;
}

// Folds one heading sample into the model and reports what it did.
//
// This was inline in the sketch's control loop, unguarded, which is a bug: with
// no BNO055 fitted -- the real condition of every bench unit -- `bno.getEvent()`
// failed and left an uninitialised `sensors_event_t` untouched, and the loop
// integrated that stack memory into gondolaHeading every 20 ms and then drove
// the servos from it. The recorded "heading stayed 0" was a stack that happened
// to be zero, not a guard.
//
// The policy when there is no sensor is to hold the last bearing, which is
// exactly what this firmware already does for a lost Pi link: the Pi was never
// load-bearing for motion, so an absent IMU holds rather than stops. What was
// missing was the *visibility* that a held bearing is not a reading, and that is
// what `imuPresent` now carries.
//
// It lives in this header rather than the sketch so the policy is one function
// rather than something split across two files. See the note at the top: this
// file is arranged to be compilable and drivable on a host, and a policy written
// into the .ino forfeits that.
inline ImuSample applyImuHeading(GondolaState& state, float sampleDeg, bool sensorPresent) {
  state.imuPresent = sensorPresent;

  if (!sensorPresent) {
    return IMU_SAMPLE_NO_SENSOR;
  }

  // A present sensor that reports a non-finite sample is still not a reading.
  // Integrating it would poison the heading for every subsequent loop, and
  // there is no recovery short of re-initialising the state.
  if (!isfinite(sampleDeg)) {
    return IMU_SAMPLE_REJECTED;
  }

  float deltaHeading = wrap180(sampleDeg - state.gondolaHeading);
  state.gondolaHeading = wrap360(state.gondolaHeading + (deltaHeading * IMU_ALPHA));
  return IMU_SAMPLE_APPLIED;
}

// One getEvent() failure from a sensor still believed present.
//
// Returns true when IMU_MISSED_SAMPLES_MAX consecutive failures have landed,
// which is the sketch's cue to declare the sensor absent -- through
// applyImuHeading(gondola, 0.0f, false), the same call the no-sensor branch
// makes, so presence still has exactly one writer. The counter restarts on that
// declaration, so the next fitted session gets the full budget rather than
// being one flapping sample away from declared-absent forever.
//
// A getEvent() that *answers* is not a miss, even when the value it carried is
// NaN and applyImuHeading() rejects it: the question this counts is "did the
// sensor speak", not "was the number usable".
inline bool noteImuMiss(GondolaState& state) {
  state.imuMissCount++;
  if (state.imuMissCount < IMU_MISSED_SAMPLES_MAX) {
    return false;
  }
  state.imuMissCount = 0;
  return true;
}

// A getEvent() that answered, whatever it answered with.
inline void clearImuMisses(GondolaState& state) {
  state.imuMissCount = 0;
}

inline AntennaAxis* findAntenna(GondolaState& state, uint32_t servoId) {
  for (int i = 0; i < NUM_ANTENNAS; i++) {
    if (state.antennas[i].id == servoId) {
      return &state.antennas[i];
    }
  }
  return nullptr;
}

// Latches one heater command and, for an "on", starts (or restarts) the
// dead-man clock. `heaterId` has already been validated by applyCommand().
//
// An "off" is immediate and does not touch the stamp: expireHeaters() only ever
// clears a flag that is on, so there is no timestamp to keep honest.
inline void noteHeaterCommand(GondolaState& state, uint32_t heaterId, bool on,
                              unsigned long nowMs) {
  if (heaterId == 1) {
    state.heater1 = on;
    if (on) state.heater1LastOnMs = nowMs;
  } else {
    state.heater2 = on;
    if (on) state.heater2LastOnMs = nowMs;
  }
}

// Turns off any heater whose last "on" command is now more than
// HEATER_AUTO_OFF_MS old. The sketch calls this every control tick and mirrors
// the flags to the pins.
//
// The subtraction is unsigned on purpose: millis() wraps roughly every 49
// days, and `nowMs - stamp` stays correct across the wrap as long as both are
// the same width, where a signed `nowMs >= stamp + HEATER_AUTO_OFF_MS` would
// not be. The clock is only consulted for a heater that is on, so a heater
// turned off at t=0 cannot expire "again".
//
// This is the half of the dead-man that makes the system fail safe: the host
// half (HeaterRefreshInterval in internal/command/heater_keeper.go) exists
// only to keep pushing the window forward, and every way for it to fail --
// crash, unplug, reboot -- leaves this function to do the turning off.
inline void expireHeaters(GondolaState& state, unsigned long nowMs) {
  if (state.heater1 && (nowMs - state.heater1LastOnMs) >= HEATER_AUTO_OFF_MS) {
    state.heater1 = false;
  }
  if (state.heater2 && (nowMs - state.heater2LastOnMs) >= HEATER_AUTO_OFF_MS) {
    state.heater2 = false;
  }
}

// Applies one decoded command to the model and returns the firmware's reply
// code. Heater pins are driven by the sketch, so only the model flag changes
// here; the sketch mirrors it to GPIO after calling this.
//
// `nowMs` is the caller's clock (millis() in the sketch), and it is only read
// by the heater path: an "on" starts the HEATER_AUTO_OFF_MS dead-man, so the
// time the command arrived is part of what the command means. Every other
// command ignores it, and nothing in this header reads a clock itself.
inline rocsar_v1_ErrorCode applyCommand(
    GondolaState& state, const rocsar_v1_PicoCommand& cmd, unsigned long nowMs) {
  AntennaAxis* axis = nullptr;

  switch (cmd.which_payload) {
    case rocsar_v1_PicoCommand_set_target_tag: {
      // Rejected here rather than tolerated downstream. wrap360(NaN) is NaN, so
      // a non-finite target would be latched as the heading the antennas are
      // tracking and every calculateTargetTick() after it would take the
      // non-finite branch and centre -- the command would appear accepted (this
      // function returned ERROR_NONE) and silently do nothing anyone could see
      // until they read target_heading_deg off the wire.
      float targetHeading = cmd.payload.set_target.target_heading_deg;
      if (!isfinite(targetHeading)) {
        return rocsar_v1_ErrorCode_ERROR_INVALID_PARAMETER;
      }
      state.targetHeading = wrap360(targetHeading);
      for (int i = 0; i < NUM_ANTENNAS; i++) {
        state.antennas[i].manualMode = false;
        // Releasing the boot interlock for both axes: a target is exactly the
        // expressed intent it is waiting for.
        state.antennas[i].awaitingCommand = false;
      }
      return rocsar_v1_ErrorCode_ERROR_NONE;
    }

    case rocsar_v1_PicoCommand_jog_tag:
      axis = findAntenna(state, cmd.payload.jog.servo_id);
      if (axis == nullptr) return rocsar_v1_ErrorCode_ERROR_INVALID_SERVO;
      axis->manualTick = clampTick((int32_t)cmd.payload.jog.tick);
      axis->manualMode = true;
      axis->awaitingCommand = false;
      return rocsar_v1_ErrorCode_ERROR_NONE;

    case rocsar_v1_PicoCommand_stop_tag:
      axis = findAntenna(state, cmd.payload.stop.servo_id);
      if (axis == nullptr) return rocsar_v1_ErrorCode_ERROR_INVALID_SERVO;
      // Freeze at the position the axis is actually holding.
      //
      // Deliberately does NOT release awaitingCommand: on an axis that has
      // never been commanded, "stop" is a no-op that must stay one, because
      // releasing would make the next tick transmit lastSentTick=0xFFFF's
      // target -- a first move, made by a command that asks for none.
      axis->manualTick = axis->currentTick;
      axis->manualMode = true;
      return rocsar_v1_ErrorCode_ERROR_NONE;

    case rocsar_v1_PicoCommand_zero_tag:
      axis = findAntenna(state, cmd.payload.zero.servo_id);
      if (axis == nullptr) return rocsar_v1_ErrorCode_ERROR_INVALID_SERVO;
      axis->centerTick = axis->currentTick;
      axis->manualTick = axis->currentTick;
      // Also not an expressed intent to move: zero redefines centre from where
      // the axis already is, so it must not be able to start motion either.
      return rocsar_v1_ErrorCode_ERROR_NONE;

    case rocsar_v1_PicoCommand_mount_tag: {
      axis = findAntenna(state, cmd.payload.mount.servo_id);
      if (axis == nullptr) return rocsar_v1_ErrorCode_ERROR_INVALID_SERVO;
      // Same seam as the target above: mountOffsetDeg feeds wrap360() and then
      // the tick arithmetic, and a NaN there reaches the (int32_t) cast. A
      // mount offset that is not a number cannot be applied, so say so.
      float offsetDeg = cmd.payload.mount.offset_deg;
      if (!isfinite(offsetDeg)) {
        return rocsar_v1_ErrorCode_ERROR_INVALID_PARAMETER;
      }
      axis->mountOffsetDeg = wrap360(offsetDeg);
      return rocsar_v1_ErrorCode_ERROR_NONE;
    }

    case rocsar_v1_PicoCommand_dir_tag:
      axis = findAntenna(state, cmd.payload.dir.servo_id);
      if (axis == nullptr) return rocsar_v1_ErrorCode_ERROR_INVALID_SERVO;
      // NaN fails both comparisons, so this case already rejects it: a
      // multiplier that is not exactly +/-1 would send the axis the wrong way
      // or not at all, and 0 would park it permanently.
      if (cmd.payload.dir.multiplier != 1.0f && cmd.payload.dir.multiplier != -1.0f) {
        return rocsar_v1_ErrorCode_ERROR_INVALID_PARAMETER;
      }
      axis->dirMultiplier = cmd.payload.dir.multiplier;
      return rocsar_v1_ErrorCode_ERROR_NONE;

    case rocsar_v1_PicoCommand_heater_tag: {
      uint32_t heaterId = cmd.payload.heater.heater_id;
      if (heaterId != 1 && heaterId != 2) {
        return rocsar_v1_ErrorCode_ERROR_INVALID_HEATER;
      }
      noteHeaterCommand(state, heaterId, cmd.payload.heater.state, nowMs);
      return rocsar_v1_ErrorCode_ERROR_NONE;
    }

    case rocsar_v1_PicoCommand_status_request_tag:
      // The acknowledgement is the liveness proof; state rides the 1 Hz
      // telemetry stream, so there is nothing to change here.
      return rocsar_v1_ErrorCode_ERROR_NONE;

    default:
      return rocsar_v1_ErrorCode_ERROR_INVALID_COMMAND;
  }
}

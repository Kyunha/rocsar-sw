// Host-side harness around the firmware's own encoder/decoder.
//
// Prints COBS-framed PicoMessage hex the exact way firmware.ino would emit
// them, and decodes a command frame back. tests/test_firmware_wire_format.py
// drives this to prove the nanopb C encoder and the Python decoder agree.
//
// Usage:
//   firmware_encoder_probe                        -> prints sample TX frames as hex
//   firmware_encoder_probe <hex-with-delim>       -> prints "DECODED <seq> <tag> <detail>"
//   firmware_encoder_probe --response-for <hex> <ok|err>
//                                                     -> decodes, then prints the
//                                                        response frame the Pico
//                                                        would send back
//   firmware_encoder_probe --apply <hex> [<hex>...]
//                                                     -> runs the firmware's real
//                                                        command handling, printing
//                                                        one "REPLY/STATE/DRIVE"
//                                                        set per command; a `t=<ms>`
//                                                        argument sets the clock the
//                                                        commands arrive with
//   firmware_encoder_probe --apply-synth <kind> <args...]
//                                                     -> the same, for a command
//                                                        built here rather than
//                                                        decoded from a frame
//                                                        (the host has no encoder)
//   firmware_encoder_probe --heater-lifetime <step>...]
//                                                     -> drives the heater dead-man
//                                                        over a timeline of t=/on/off
//   firmware_encoder_probe --boot-state            -> initial state plus what the
//                                                      first control tick transmits
//   firmware_encoder_probe --heading <step> [<step>...]
//                                                   -> drives applyImuHeading() with a
//                                                      sequence of samples, printing one
//                                                      "HEADING ..." line per step
//   firmware_encoder_probe --target-tick <heading> <target>
//                                                   -> prints the tick the firmware
//                                                      would command, for the seam guard
//   firmware_encoder_probe --servo-packet <id> <tick>
//                                                   -> prints the 13 STS3215 wire bytes
//                                                      as hex, checksum included
//   firmware_encoder_probe --servo-status-request <id>
//                                                   -> prints the status-read request hex
//   firmware_encoder_probe --servo-status-scan <hex> <id>
//                                                   -> runs scanServoStatus() over raw
//                                                      bus bytes and prints the verdict
//   firmware_encoder_probe --apply-feedback <hex> <id> [<hex> <id>...]
//                                                   -> folds parsed status into the axes
//                                                      and prints the resulting state
//   firmware_encoder_probe --telemetry-from-state <heading> <target> <imu> <h1> <h2>
//                                                   -> fills a TelemetryMessage through
//                                                      the firmware's own mapping and
//                                                      prints it as hex
//
// A <step> for --heading is a number (a real sample), "none" (no sensor fitted),
// "nan"/"inf" (a non-finite sample), or "nodata" (a fitted sensor that delivered
// nothing this tick -- the sketch holds and counts the miss, declaring the
// sensor absent after IMU_MISSED_SAMPLES_MAX of them).

#include <stdio.h>
#include <string.h>
#include <stdlib.h>

#include "calibration.h"
#include "cobs.h"
#include "gondola_model.h"
#include "pb_decode.h"
#include "pb_encode.h"
#include "pico.pb.h"
#include "pico_wire.h"

static void printHex(const uint8_t* data, size_t len) {
  for (size_t i = 0; i < len; i++) {
    printf("%02x", data[i]);
  }
  printf("\n");
}

static void emitTelemetry() {
  rocsar_v1_PicoMessage message = rocsar_v1_PicoMessage_init_zero;
  message.sequence = 7;
  message.timestamp_us = 1234567890ULL;
  message.which_payload = rocsar_v1_PicoMessage_telemetry_tag;

  message.payload.telemetry.gondola_heading_deg = 123.5f;
  message.payload.telemetry.target_heading_deg = 45.25f;
  message.payload.telemetry.heater1_state = true;
  message.payload.telemetry.heater2_state = false;
  message.payload.telemetry.imu_present = true;
  message.payload.telemetry.antennas_count = 2;

  message.payload.telemetry.antennas[0].servo_id = 5;
  message.payload.telemetry.antennas[0].manual_mode = true;
  message.payload.telemetry.antennas[0].current_tick = 3000;
  message.payload.telemetry.antennas[0].current_angle_deg = 80.5f;
  message.payload.telemetry.antennas[0].temperature_c = 31;
  message.payload.telemetry.antennas[0].load = 12;
  message.payload.telemetry.antennas[0].center_tick = 2048;
  message.payload.telemetry.antennas[0].mount_offset_deg = 270.0f;
  message.payload.telemetry.antennas[0].dir_multiplier = -1.0f;
  message.payload.telemetry.antennas[0].feedback_state = rocsar_v1_FeedbackState_FEEDBACK_MEASURED;
  message.payload.telemetry.antennas[0].feedback_error = 0;

  message.payload.telemetry.antennas[1].servo_id = 10;
  message.payload.telemetry.antennas[1].current_tick = 100;
  message.payload.telemetry.antennas[1].dir_multiplier = 1.0f;
  // Deliberately left invalid/erroring: the emitted sample has to contain one
  // axis reporting a measurement and one reporting a held value, or a test that
  // only ever sees "valid" cannot tell a mapping from a constant.
  message.payload.telemetry.antennas[1].feedback_state = rocsar_v1_FeedbackState_FEEDBACK_HELD;
  message.payload.telemetry.antennas[1].feedback_error = 1;

  uint8_t frame[PICO_TX_FRAME_MAX];
  size_t len = encodePicoFrame(message, frame, sizeof(frame));
  if (len == 0) {
    fprintf(stderr, "telemetry encode failed\n");
    return;
  }
  printHex(frame, len);
}

static void emitResponse(uint32_t commandSequence, bool success,
                         rocsar_v1_ErrorCode error) {
  rocsar_v1_PicoMessage message = rocsar_v1_PicoMessage_init_zero;
  message.sequence = 3;
  message.timestamp_us = 42;
  message.which_payload = rocsar_v1_PicoMessage_ack_tag;
  message.payload.ack.command_sequence = commandSequence;
  message.payload.ack.success = success;
  message.payload.ack.error = error;

  uint8_t frame[PICO_TX_FRAME_MAX];
  size_t len = encodePicoFrame(message, frame, sizeof(frame));
  if (len == 0) {
    fprintf(stderr, "response encode failed\n");
    return;
  }
  printHex(frame, len);
}

static int decodeFrame(const char* hex, rocsar_v1_PicoCommand* out,
                       bool verbose = true) {
  size_t textLen = strlen(hex);
  if (textLen == 0 || textLen % 2 != 0) {
    fprintf(stderr, "bad hex length\n");
    return 2;
  }

  uint8_t raw[512];
  size_t rawLen = textLen / 2;
  if (rawLen >= sizeof(raw)) {
    fprintf(stderr, "frame too long\n");
    return 2;
  }
  for (size_t i = 0; i < rawLen; i++) {
    unsigned int byte = 0;
    if (sscanf(hex + 2 * i, "%2x", &byte) != 1) {
      fprintf(stderr, "bad hex digit\n");
      return 2;
    }
    raw[i] = (uint8_t)byte;
  }

  // Strip the trailing 0x00 delimiter the sender appends.
  if (rawLen == 0 || raw[rawLen - 1] != 0x00) {
    fprintf(stderr, "missing frame delimiter\n");
    return 2;
  }
  size_t frameLen = rawLen - 1;

  uint8_t decoded[512];
  size_t decodedLen = cobs_decode(raw, frameLen, decoded);
  if (decodedLen == 0 && frameLen != 0) {
    fprintf(stderr, "cobs decode failed\n");
    return 2;
  }

  rocsar_v1_PicoCommand command =
      rocsar_v1_PicoCommand_init_zero;
  pb_istream_t stream = pb_istream_from_buffer(decoded, decodedLen);
  if (!pb_decode(&stream, rocsar_v1_PicoCommand_fields, &command)) {
    fprintf(stderr, "pb decode failed\n");
    return 2;
  }

  const char* tag = "<none>";
  char detail[128] = "";
  switch (command.which_payload) {
    case rocsar_v1_PicoCommand_set_target_tag:
      tag = "set_target";
      snprintf(detail, sizeof(detail), "%.4f", command.payload.set_target.target_heading_deg);
      break;
    case rocsar_v1_PicoCommand_jog_tag:
      tag = "jog";
      snprintf(detail, sizeof(detail), "%u %u", command.payload.jog.servo_id,
               command.payload.jog.tick);
      break;
    case rocsar_v1_PicoCommand_zero_tag:
      tag = "zero";
      snprintf(detail, sizeof(detail), "%u", command.payload.zero.servo_id);
      break;
    case rocsar_v1_PicoCommand_mount_tag:
      tag = "mount";
      snprintf(detail, sizeof(detail), "%u %.4f", command.payload.mount.servo_id,
               command.payload.mount.offset_deg);
      break;
    case rocsar_v1_PicoCommand_dir_tag:
      tag = "dir";
      snprintf(detail, sizeof(detail), "%u %.4f", command.payload.dir.servo_id,
               command.payload.dir.multiplier);
      break;
    case rocsar_v1_PicoCommand_heater_tag:
      tag = "heater";
      snprintf(detail, sizeof(detail), "%u %d", command.payload.heater.heater_id,
               command.payload.heater.state ? 1 : 0);
      break;
    case rocsar_v1_PicoCommand_stop_tag:
      tag = "stop";
      snprintf(detail, sizeof(detail), "%u", command.payload.stop.servo_id);
      break;
    case rocsar_v1_PicoCommand_status_request_tag:
      tag = "status_request";
      break;
    default:
      break;
  }

  if (verbose) {
    printf("DECODED %u %s %s\n", command.sequence, tag, detail);
  }
  *out = command;
  return 0;
}

static void printState(const GondolaState& state) {
  printf("STATE heading=%.3f target=%.3f h1=%d h2=%d imu=%d miss=%u",
         state.gondolaHeading, state.targetHeading, state.heater1 ? 1 : 0,
         state.heater2 ? 1 : 0, state.imuPresent ? 1 : 0,
         (unsigned)state.imuMissCount);
  for (int i = 0; i < NUM_ANTENNAS; i++) {
    const AntennaAxis& axis = state.antennas[i];
    printf(" | a%d id=%u center=%u zeroed=%d manual=%d manualTick=%u tick=%u offset=%.3f dir=%.1f",
           i, axis.id, axis.centerTick, axis.centerZeroed ? 1 : 0,
           axis.manualMode ? 1 : 0, axis.manualTick, axis.currentTick,
           axis.mountOffsetDeg, axis.dirMultiplier);
    printf(" fb_state=%d fb_err=%u load=%.2f temp=%.2f await=%d",
           (int)axis.feedbackState, axis.feedbackError, axis.load,
           axis.temperatureC, axis.awaitingCommand ? 1 : 0);
  }
  printf("\n");
}

// The transmit decision the control loop would make for every axis right now.
//
// Printed next to STATE so a test can read posture and consequence from one
// run: manual_mode says what the firmware believes it is doing, DRIVE says
// whether a byte would actually go on the bus. They come apart exactly when
// the boot interlock is doing its job -- manual hold with DRIVE 0.
static void printDrive(const GondolaState& state) {
  printf("DRIVE");
  for (int i = 0; i < NUM_ANTENNAS; i++) {
    const AntennaAxis& axis = state.antennas[i];
    uint16_t target =
        axis.calculateTargetTick(state.gondolaHeading, state.targetHeading);
    printf(" a%d=%d", i, shouldDriveServo(axis, target) ? 1 : 0);
  }
  printf("\n");
}

// The firmware's reply code as its schema name, so a test can assert *which*
// refusal happened rather than that something was refused.
static const char* errorName(rocsar_v1_ErrorCode code) {
  switch (code) {
    case rocsar_v1_ErrorCode_ERROR_NONE:
      return "ERROR_NONE";
    case rocsar_v1_ErrorCode_ERROR_INVALID_COMMAND:
      return "ERROR_INVALID_COMMAND";
    case rocsar_v1_ErrorCode_ERROR_INVALID_PARAMETER:
      return "ERROR_INVALID_PARAMETER";
    case rocsar_v1_ErrorCode_ERROR_INVALID_SERVO:
      return "ERROR_INVALID_SERVO";
    case rocsar_v1_ErrorCode_ERROR_INVALID_HEATER:
      return "ERROR_INVALID_HEATER";
    default:
      return "ERROR_OTHER";
  }
}

static void printReply(const rocsar_v1_PicoCommand& command,
                       rocsar_v1_ErrorCode error) {
  printf("REPLY %u %d %s\n", command.sequence, (int)error, errorName(error));
}

// Reads raw hex bus bytes into a buffer. Shared by the two status entry points.
static int hexToBytes(const char* hex, uint8_t* out, size_t outCap, size_t* outLen) {
  size_t textLen = strlen(hex);
  if (textLen == 0 || textLen % 2 != 0) {
    fprintf(stderr, "bad hex length\n");
    return 2;
  }
  size_t rawLen = textLen / 2;
  if (rawLen > outCap) {
    fprintf(stderr, "hex too long\n");
    return 2;
  }
  for (size_t i = 0; i < rawLen; i++) {
    unsigned int byte = 0;
    if (sscanf(hex + 2 * i, "%2x", &byte) != 1) {
      fprintf(stderr, "bad hex digit\n");
      return 2;
    }
    out[i] = (uint8_t)byte;
  }
  *outLen = rawLen;
  return 0;
}

// Prints the status-read request the sketch would put on the bus. The framing
// lives in gondola_model.h, where a test can reach it; the sketch can only be
// read, and a wrong length byte reads back whatever the servo felt like sending.
static int servoStatusRequest(int argc, char** argv) {
  if (argc < 3) {
    fprintf(stderr, "usage: --servo-status-request <id>\n");
    return 2;
  }
  uint8_t packet[SERVO_STATUS_REQUEST_LEN];
  size_t len = buildServoStatusRequest((uint8_t)strtoul(argv[2], nullptr, 10), packet,
                                       sizeof(packet));
  if (len == 0) {
    fprintf(stderr, "request build failed\n");
    return 2;
  }
  printHex(packet, len);
  return 0;
}

// Runs scanServoStatus() over raw bus bytes and prints its verdict.
//
// This is the entry point that makes the echo question testable. The caller
// supplies whatever the wire actually produced -- an echo followed by a reply, a
// reply alone, noise, a truncated frame -- and the firmware's own scanner decides
// whether that is a usable status reading.
static int servoStatusScan(int argc, char** argv) {
  if (argc < 4) {
    fprintf(stderr, "usage: --servo-status-scan <hex> <id>\n");
    return 2;
  }
  uint8_t buffer[512];
  size_t len = 0;
  if (hexToBytes(argv[2], buffer, sizeof(buffer), &len) != 0) {
    return 2;
  }

  uint8_t expectedId = (uint8_t)strtoul(argv[3], nullptr, 10);
  ServoScan scan = scanServoStatus(buffer, len, expectedId);

  printf("SCAN matched=%d consumed=%zu", scan.matched ? 1 : 0, scan.consumed);
  if (scan.matched) {
    printf(" tick=%u load=%.2f temp=%d err=%u", scan.status.positionTicks,
           scan.status.loadPercent, (int)scan.status.temperatureC,
           (unsigned)scan.status.error);
  }
  printf("\n");
  return 0;
}

// Folds parsed status into the axes, exactly as the loop does, and prints the
// state after each step. A step whose scan does not match must leave the axis
// invalid and its last reading intact.
static int applyFeedback(int argc, char** argv) {
  if (argc < 4 || ((argc - 2) % 2) != 0) {
    fprintf(stderr, "usage: --apply-feedback <hex> <id> [<hex> <id>...]\n");
    return 2;
  }
  GondolaState state;
  initGondolaState(state);

  // Establish a commanded position first, so the fallback path has something to
  // report and a test can tell "held the command echo" from "replaced by a
  // measurement".
  for (int i = 0; i < NUM_ANTENNAS; i++) {
    state.antennas[i].lastSentTick = (uint16_t)(1000 + 100 * i);
    updateAxisFeedback(state.antennas[i]);
  }
  printState(state);

  for (int i = 2; i + 1 < argc; i += 2) {
    uint8_t buffer[512];
    size_t len = 0;
    if (hexToBytes(argv[i], buffer, sizeof(buffer), &len) != 0) {
      return 2;
    }
    uint8_t expectedId = (uint8_t)strtoul(argv[i + 1], nullptr, 10);

    AntennaAxis* axis = findAntenna(state, expectedId);
    if (axis == nullptr) {
      fprintf(stderr, "no such servo in the model: %u\n", (unsigned)expectedId);
      return 2;
    }

    ServoScan scan = scanServoStatus(buffer, len, expectedId);
    if (scan.matched) {
      applyServoFeedback(*axis, scan.status);
      printf("FEEDBACK applied id=%u\n", (unsigned)expectedId);
    } else {
      invalidateAxisFeedback(*axis);
      printf("FEEDBACK held id=%u\n", (unsigned)expectedId);
    }
    printState(state);
  }
  return 0;
}

// Applies encoded command frames through the firmware's real applyCommand().
//
// An argument of the form `t=<ms>` sets the clock the commands are applied
// with -- which is how the heater dead-man is driven from this entry point,
// since "on" records the time it arrived. Commands before any `t=` use 0.
static int applyFrames(int argc, char** argv) {
  GondolaState state;
  initGondolaState(state);

  unsigned long nowMs = 0;
  for (int i = 0; i < argc; i++) {
    if (strncmp(argv[i], "t=", 2) == 0) {
      nowMs = strtoul(argv[i] + 2, nullptr, 10);
      printf("TIME %lu\n", nowMs);
      continue;
    }
    rocsar_v1_PicoCommand command =
        rocsar_v1_PicoCommand_init_zero;
    // Suppress the per-command DECODED line; only the resulting state matters here.
    if (decodeFrame(argv[i], &command, false) != 0) {
      return 2;
    }
    rocsar_v1_ErrorCode error = applyCommand(state, command, nowMs);
    printReply(command, error);
    printState(state);
    printDrive(state);
  }
  return 0;
}

// One command built here rather than decoded from a frame, for the cases a
// host cannot encode: there is no Python or Go command encoder in this repo
// yet (see the note at the top of test_firmware_commands.py), and NaN is not
// something a hex frame carries in a readable way anyway.
//
// Output is the same REPLY/STATE/DRIVE triple --apply prints, so a test can
// treat "how the firmware was told" as an implementation detail.
static int applySynth(int argc, char** argv) {
  if (argc < 2) {
    fprintf(stderr,
            "usage: --apply-synth set_target <heading>\n"
            "          | jog <id> <tick> | stop <id> | zero <id>\n"
            "          | mount <id> <offset> | dir <id> <multiplier>\n"
            "          | heater <id> <0|1>\n");
    return 2;
  }

  rocsar_v1_PicoCommand command = rocsar_v1_PicoCommand_init_zero;
  command.sequence = 1;
  const char* kind = argv[0];

  if (strcmp(kind, "set_target") == 0 && argc == 2) {
    command.which_payload = rocsar_v1_PicoCommand_set_target_tag;
    command.payload.set_target.target_heading_deg = strtof(argv[1], nullptr);
  } else if (strcmp(kind, "jog") == 0 && argc == 3) {
    command.which_payload = rocsar_v1_PicoCommand_jog_tag;
    command.payload.jog.servo_id = (uint32_t)strtoul(argv[1], nullptr, 10);
    command.payload.jog.tick = (uint32_t)strtoul(argv[2], nullptr, 10);
  } else if (strcmp(kind, "stop") == 0 && argc == 2) {
    command.which_payload = rocsar_v1_PicoCommand_stop_tag;
    command.payload.stop.servo_id = (uint32_t)strtoul(argv[1], nullptr, 10);
  } else if (strcmp(kind, "zero") == 0 && argc == 2) {
    command.which_payload = rocsar_v1_PicoCommand_zero_tag;
    command.payload.zero.servo_id = (uint32_t)strtoul(argv[1], nullptr, 10);
  } else if (strcmp(kind, "mount") == 0 && argc == 3) {
    command.which_payload = rocsar_v1_PicoCommand_mount_tag;
    command.payload.mount.servo_id = (uint32_t)strtoul(argv[1], nullptr, 10);
    command.payload.mount.offset_deg = strtof(argv[2], nullptr);
  } else if (strcmp(kind, "dir") == 0 && argc == 3) {
    command.which_payload = rocsar_v1_PicoCommand_dir_tag;
    command.payload.dir.servo_id = (uint32_t)strtoul(argv[1], nullptr, 10);
    command.payload.dir.multiplier = strtof(argv[2], nullptr);
  } else if (strcmp(kind, "heater") == 0 && argc == 3) {
    command.which_payload = rocsar_v1_PicoCommand_heater_tag;
    command.payload.heater.heater_id = (uint32_t)strtoul(argv[1], nullptr, 10);
    command.payload.heater.state = strcmp(argv[2], "0") != 0;
  } else {
    fprintf(stderr, "unknown or wrong-arity --apply-synth command: %s\n", kind);
    return 2;
  }

  GondolaState state;
  initGondolaState(state);
  rocsar_v1_ErrorCode error = applyCommand(state, command, 0);
  printReply(command, error);
  printState(state);
  printDrive(state);
  return 0;
}

// Drives the heater dead-man over a timeline.
//
// Each step is `t=<ms>` (advance the clock, optionally followed by nothing),
// `on1`/`on2`, or `off1`/`off2`. Every step runs expireHeaters() at that time
// after its operation, which is the order the sketch runs in: commands are
// taken off the wire first, then the control tick expires what the keep-alive
// has let lapse. One HEATER line per step is all a test needs to see when a
// heater crossed the window.
static int heaterLifetime(int argc, char** argv) {
  GondolaState state;
  initGondolaState(state);

  unsigned long nowMs = 0;
  for (int i = 0; i < argc; i++) {
    const char* step = argv[i];
    if (strncmp(step, "t=", 2) == 0) {
      nowMs = strtoul(step + 2, nullptr, 10);
    } else if (strcmp(step, "on1") == 0 || strcmp(step, "on2") == 0 ||
               strcmp(step, "off1") == 0 || strcmp(step, "off2") == 0) {
      rocsar_v1_PicoCommand command = rocsar_v1_PicoCommand_init_zero;
      command.sequence = 1;
      command.which_payload = rocsar_v1_PicoCommand_heater_tag;
      // The id is the trailing digit, not a fixed offset: "on1" and "off1" put
      // it at different places, and reading step[2] of "off1" yields 'f'.
      command.payload.heater.heater_id =
          (uint32_t)(step[strlen(step) - 1] - '0');
      command.payload.heater.state = step[0] == 'o' && step[1] == 'n';
      if (applyCommand(state, command, nowMs) != rocsar_v1_ErrorCode_ERROR_NONE) {
        fprintf(stderr, "heater command refused: %s\n", step);
        return 2;
      }
    } else {
      fprintf(stderr, "bad --heater-lifetime step: %s\n", step);
      return 2;
    }
    expireHeaters(state, nowMs);
    printf("HEATER t=%lu h1=%d h2=%d\n", nowMs, state.heater1 ? 1 : 0,
           state.heater2 ? 1 : 0);
  }
  return 0;
}

// The boot posture: initial state, plus what the first control tick would
// transmit. Nothing has been commanded, so DRIVE must be all zeroes -- if it
// is not, a board has started moving on power-up.
static int bootState() {
  GondolaState state;
  initGondolaState(state);
  // The fallback runs on tick one, before any command exists; this is that tick
  // minus the hardware.
  for (int i = 0; i < NUM_ANTENNAS; i++) {
    updateAxisFeedback(state.antennas[i]);
  }
  printState(state);
  printDrive(state);
  return 0;
}

static const char* sampleName(ImuSample sample) {
  switch (sample) {
    case IMU_SAMPLE_APPLIED: return "APPLIED";
    case IMU_SAMPLE_NO_SENSOR: return "NO_SENSOR";
    case IMU_SAMPLE_REJECTED: return "REJECTED";
  }
  return "?";
}

// Drives the heading policy the way the sketch's control loop does. This exists
// because the policy used to live inline in the .ino, where the probe TU cannot
// reach it -- which is how an unguarded getEvent() call survived a green suite.
static int applyHeadings(int argc, char** argv) {
  GondolaState state;
  initGondolaState(state);

  for (int i = 0; i < argc; i++) {
    const char* step = argv[i];
    bool present = true;
    float sample = 0.0f;

    if (strcmp(step, "none") == 0) {
      present = false;
    } else if (strcmp(step, "nodata") == 0) {
      // The sketch's getEvent() failure path: hold, count the miss, and on the
      // IMU_MISSED_SAMPLES_MAX'th consecutive one declare the sensor absent
      // exactly the way readImuHeading() does -- through applyImuHeading(...,
      // false), so the flag and the counter stay the model's to reconcile.
      //
      // Counted only while the sensor is believed present, because that is the
      // branch the sketch is in: once absent, re-probing (not counting) is what
      // runs, and this step has no clock to model IMU_REPROBE_MS with.
      if (state.imuPresent && noteImuMiss(state)) {
        applyImuHeading(state, 0.0f, false);
      }
      printf("HEADING NO_DATA imu=%d heading=%.3f\n",
             state.imuPresent ? 1 : 0, state.gondolaHeading);
      continue;
    } else if (strcmp(step, "nan") == 0) {
      sample = NAN;
    } else if (strcmp(step, "inf") == 0) {
      sample = INFINITY;
    } else {
      sample = strtof(step, nullptr);
    }

    if (present) {
      // getEvent() answered, whatever it answered with: any earlier run of
      // misses is over. A non-finite sample below is still a reading that
      // arrived; applyImuHeading() decides whether it is one to use.
      clearImuMisses(state);
    }
    ImuSample result = applyImuHeading(state, sample, present);
    printf("HEADING %s imu=%d heading=%.3f\n", sampleName(result),
           state.imuPresent ? 1 : 0, state.gondolaHeading);
  }

  printState(state);
  return 0;
}

// The seam guard: a non-finite geometry must not reach the (int32_t) cast.
static int targetTick(int argc, char** argv) {
  if (argc < 4) {
    fprintf(stderr, "usage: --target-tick <heading> <target>\n");
    return 2;
  }
  GondolaState state;
  initGondolaState(state);
  state.antennas[0].manualMode = false;

  float heading = strtof(argv[2], nullptr);
  float target = strtof(argv[3], nullptr);
  printf("TICK %u\n", state.antennas[0].calculateTargetTick(heading, target));
  return 0;
}

// Prints the STS3215 packet the sketch would put on the bus. The checksum and
// the little-endian position were inline in the .ino, where no test could see
// them: a servo that does not move is indistinguishable from a servo nobody
// commanded.
static int servoPacket(int argc, char** argv) {
  if (argc < 4) {
    fprintf(stderr, "usage: --servo-packet <id> <tick>\n");
    return 2;
  }
  uint8_t packet[SERVO_PACKET_LEN];
  size_t len =
      buildServoPacket((uint8_t)strtoul(argv[2], nullptr, 10), (int32_t)strtol(argv[3], nullptr, 10),
                       packet, sizeof(packet));
  if (len == 0) {
    fprintf(stderr, "packet build failed\n");
    return 2;
  }
  printHex(packet, len);
  return 0;
}

// Fills a TelemetryMessage through the firmware's own mapping and prints the
// encoded frame, so the Python side can decode it with the codec it really uses
// and the two cannot disagree about a field. <imu> and <h1>/<h2> are 0/1.
static int telemetryFromState(int argc, char** argv) {
  if (argc < 7) {
    fprintf(stderr,
            "usage: --telemetry-from-state <heading> <target> <imu> <h1> <h2>\n");
    return 2;
  }
  GondolaState state;
  initGondolaState(state);

  applyImuHeading(state, strtof(argv[2], nullptr), strcmp(argv[4], "0") != 0);
  state.targetHeading = strtof(argv[3], nullptr);
  state.heater1 = strcmp(argv[5], "0") != 0;
  state.heater2 = strcmp(argv[6], "0") != 0;

  // Give the axes something distinguishable to report, so a mapping that
  // silently copies the wrong field is visible rather than plausible.
  for (int i = 0; i < NUM_ANTENNAS; i++) {
    AntennaAxis& axis = state.antennas[i];
    axis.lastSentTick = (uint16_t)(1000 + 100 * i);
    axis.manualMode = (i == 1);
    axis.mountOffsetDeg = 270.0f + (float)i;
    axis.dirMultiplier = -1.0f;
    updateAxisFeedback(axis);
  }

  rocsar_v1_PicoMessage message = rocsar_v1_PicoMessage_init_zero;
  message.sequence = 11;
  message.timestamp_us = 99ULL;
  message.which_payload = rocsar_v1_PicoMessage_telemetry_tag;
  fillTelemetryMessage(state, message.payload.telemetry);

  uint8_t frame[PICO_TX_FRAME_MAX];
  size_t len = encodePicoFrame(message, frame, sizeof(frame));
  if (len == 0) {
    fprintf(stderr, "telemetry encode failed\n");
    return 2;
  }
  printHex(frame, len);
  return 0;
}

// ===========================================================================
// Teaching a servo its centre
// ===========================================================================
// The bus half of `zero` is in the sketch and needs hardware; everything that
// decides whether the bus half worked is in the model and is here. That split is
// the point -- it means the failure modes, which are the part worth testing, are
// all reachable from a host.
//
// The outcome strings stand in for what the hardware reported. `after` is either
// ST3215_SERVO_CENTRE_TICK or the unchanged starting position, and that is not a
// simplification: a teach that stored its offset but did not move the reported
// position is exactly the hypothesis that Position Offset might not affect
// feedback at all, and modelling it that way is what makes `position-odd` the
// case that catches it.
static const char* outcomeName(ServoZeroOutcome outcome) {
  switch (outcome) {
    case ZERO_OK: return "ok";
    case ZERO_NO_SERVO_REPLY: return "no-servo-reply";
    case ZERO_OFFSET_MISMATCH: return "offset-mismatch";
    case ZERO_POSITION_MISMATCH: return "position-mismatch";
    case ZERO_ANGULAR_RESOLUTION: return "angular-resolution";
  }
  return "?";
}

static ServoZeroOutcome outcomeFromName(const char* name) {
  if (strcmp(name, "ok") == 0) return ZERO_OK;
  if (strcmp(name, "no-reply") == 0) return ZERO_NO_SERVO_REPLY;
  if (strcmp(name, "offset-lost") == 0) return ZERO_OFFSET_MISMATCH;
  if (strcmp(name, "position-odd") == 0) return ZERO_POSITION_MISMATCH;
  if (strcmp(name, "resolution") == 0) return ZERO_ANGULAR_RESOLUTION;
  fprintf(stderr, "unknown outcome '%s'\n", name);
  exit(2);
}

// --zero-seq <servo-id> <before> <outcome> [<before> <outcome>...]
//
// Runs whole teach attempts through the model's three functions, in the order
// handleZeroCommand() calls them, printing one ZERO line per attempt. Several
// attempts in one run is how idempotence gets tested: a run that fails then
// succeeds is the recovery path, and it can only be exercised if the state
// carries across attempts.
static int zeroSeq(int argc, char** argv) {
  if (argc < 5 || (argc - 3) % 2 != 0) {
    fprintf(stderr,
            "usage: --zero-seq <servo-id> <before> <outcome> [<before> <outcome>...]\n");
    return 2;
  }

  GondolaState state;
  initGondolaState(state);

  rocsar_v1_PicoCommand command = rocsar_v1_PicoCommand_init_zero;
  command.which_payload = rocsar_v1_PicoCommand_zero_tag;
  command.payload.zero.servo_id = (uint32_t)strtoul(argv[2], nullptr, 10);

  int axisIndex = validateZeroCommand(state, command);
  printf("VALIDATE axis=%d\n", axisIndex);
  if (axisIndex < 0) {
    return 0;
  }

  for (int i = 3; i + 1 < argc; i += 2) {
    uint16_t before = (uint16_t)strtoul(argv[i], nullptr, 10);
    ServoZeroOutcome simulated = outcomeFromName(argv[i + 1]);

    ServoZeroOutcome outcome = ZERO_NO_SERVO_REPLY;
    uint16_t verified = ST3215_SERVO_CENTRE_TICK;

    if (simulated != ZERO_NO_SERVO_REPLY) {
      uint16_t offset = servoZeroOffset(before, ST3215_SERVO_CENTRE_TICK);
      // Read-back is the offset, except when the write was lost. Position is
      // centre except when the offset stored without moving it.
      uint16_t readBack = (simulated == ZERO_OFFSET_MISMATCH) ? (uint16_t)(offset + 1)
                                                              : offset;
      uint16_t after = (simulated == ZERO_POSITION_MISMATCH) ? before
                      : ST3215_SERVO_CENTRE_TICK;
      uint8_t resolution = (simulated == ZERO_ANGULAR_RESOLUTION) ? 2 : 1;

      outcome = judgeServoZero(offset, readBack, resolution, before, after);
      if (outcome == ZERO_OK) {
        verified = after;
      }
    }

    if (outcome == ZERO_OK) {
      commitServoZero(state, axisIndex, verified);
    }

    printf("ZERO outcome=%s before=%u offset=%u committed=%d\n",
           outcomeName(outcome), before,
           servoZeroOffset(before, ST3215_SERVO_CENTRE_TICK),
           outcome == ZERO_OK ? 1 : 0);
    printState(state);
  }

  return 0;
}

// --judge-zero <wrote> <readback> <resolution> <before> <after>
//
// The verification decision on its own, so every branch is reachable without
// having to construct a whole teach around it.
static int judgeZero(int argc, char** argv) {
  if (argc < 7) {
    fprintf(stderr,
            "usage: --judge-zero <wrote> <readback> <resolution> <before> <after>\n");
    return 2;
  }
  ServoZeroOutcome outcome = judgeServoZero(
      (uint16_t)strtoul(argv[2], nullptr, 10),
      (uint16_t)strtoul(argv[3], nullptr, 10),
      (uint8_t)strtoul(argv[4], nullptr, 10),
      (uint16_t)strtoul(argv[5], nullptr, 10),
      (uint16_t)strtoul(argv[6], nullptr, 10));
  printf("JUDGE %s\n", outcomeName(outcome));
  return 0;
}

// --servo-calib-scan <hex> <expected-id>
//
// The scanner for the four-byte calibration read-back, over whatever bytes the
// test builds. The reason it needs its own scanner rather than a widened
// scanServoStatus is the length: a 2-byte read would come back as an 8-byte
// frame at LEN=0x04, indistinguishable from our own request echo.
static int servoCalibScan(int argc, char** argv) {
  if (argc < 4) {
    fprintf(stderr, "usage: --servo-calib-scan <hex> <expected-id>\n");
    return 2;
  }
  uint8_t buffer[256];
  size_t len = 0;
  if (hexToBytes(argv[2], buffer, sizeof(buffer), &len) != 0) {
    return 2;
  }

  ServoCalibRead scan = scanServoCalibRead(
      buffer, len, (uint8_t)strtoul(argv[3], nullptr, 10));
  printf("CALIB matched=%d resolution=%u offset=%u mode=%u consumed=%zu of %zu\n",
         scan.matched ? 1 : 0, scan.angularResolution, scan.positionOffset,
         scan.mode, scan.consumed, len);
  return 0;
}

// --calib <zeroed0> <zeroed1>
//
// Encode a record from the flags, then decode it straight back. Printed as
// CALIB <result> <zeroed0> <zeroed1>, so a test can assert on a round trip and
// on the rejection reasons without decoding the bytes itself.
static int calibRoundTrip(int argc, char** argv) {
  if (argc < 4) {
    fprintf(stderr, "usage: --calib <zeroed0> <zeroed1>\n");
    return 2;
  }

  CalibrationRecord record;
  record.schemaVersion = CALIBRATION_SCHEMA_VERSION;
  record.axisCount = NUM_ANTENNAS;
  for (int i = 0; i < NUM_ANTENNAS; i++) {
    record.axisZeroed[i] = strtoul(argv[2 + i], nullptr, 10) != 0;
  }

  uint8_t bytes[CALIBRATION_RECORD_SIZE];
  if (!encodeCalibration(record, bytes, sizeof(bytes))) {
    fprintf(stderr, "encode failed\n");
    return 2;
  }
  printHex(bytes, sizeof(bytes));

  // Pre-filled with a sentinel so "untouched" is visible in the output rather
  // than merely asserted in a comment. decodeCalibration() promises to leave the
  // record alone unless it succeeded, and that promise is what keeps a rejected
  // record from being half-applied.
  CalibrationRecord back;
  memset(&back, 0xFF, sizeof(back));
  CalibrationResult result = decodeCalibration(bytes, sizeof(bytes), back);
  printf("CALIB %s %d %d\n", calibrationResultName(result),
         result == CALIBRATION_OK ? (back.axisZeroed[0] ? 1 : 0) : -1,
         result == CALIBRATION_OK ? (back.axisZeroed[1] ? 1 : 0) : -1);
  return 0;
}

// --calib-decode <hex>
//
// Decode bytes the test has corrupted itself, so each rejection reason can be
// provoked on purpose instead of hoping the encoder produces one.
static int calibDecode(int argc, char** argv) {
  if (argc < 3) {
    fprintf(stderr, "usage: --calib-decode <hex>\n");
    return 2;
  }
  uint8_t bytes[256];
  size_t len = 0;
  if (hexToBytes(argv[2], bytes, sizeof(bytes), &len) != 0) {
    return 2;
  }

  CalibrationRecord back;
  memset(&back, 0xFF, sizeof(back));
  CalibrationResult result = decodeCalibration(bytes, len, back);
  // -1 for the flags means the record was left untouched, which is the contract
  // on every rejection path: a caller that carried on with a partially-filled
  // record would be aiming an antenna using half a calibration.
  printf("CALIB %s %d %d\n", calibrationResultName(result),
         result == CALIBRATION_OK ? (back.axisZeroed[0] ? 1 : 0) : -1,
         result == CALIBRATION_OK ? (back.axisZeroed[1] ? 1 : 0) : -1);
  return 0;
}

int main(int argc, char** argv) {
  if (argc > 2 && strcmp(argv[1], "--apply") == 0) {
    return applyFrames(argc - 2, argv + 2);
  }

  if (argc > 2 && strcmp(argv[1], "--apply-synth") == 0) {
    return applySynth(argc - 2, argv + 2);
  }

  if (argc > 2 && strcmp(argv[1], "--heater-lifetime") == 0) {
    return heaterLifetime(argc - 2, argv + 2);
  }

  if (argc == 2 && strcmp(argv[1], "--boot-state") == 0) {
    return bootState();
  }

  if (argc > 2 && strcmp(argv[1], "--heading") == 0) {
    return applyHeadings(argc - 2, argv + 2);
  }

  if (argc > 3 && strcmp(argv[1], "--target-tick") == 0) {
    return targetTick(argc, argv);
  }

  if (argc > 3 && strcmp(argv[1], "--servo-packet") == 0) {
    return servoPacket(argc, argv);
  }

  if (argc > 2 && strcmp(argv[1], "--servo-status-request") == 0) {
    return servoStatusRequest(argc, argv);
  }

  if (argc > 3 && strcmp(argv[1], "--servo-status-scan") == 0) {
    return servoStatusScan(argc, argv);
  }

  if (argc > 3 && strcmp(argv[1], "--apply-feedback") == 0) {
    return applyFeedback(argc, argv);
  }

  if (argc > 6 && strcmp(argv[1], "--telemetry-from-state") == 0) {
    return telemetryFromState(argc, argv);
  }

  if (argc > 4 && strcmp(argv[1], "--zero-seq") == 0) {
    return zeroSeq(argc, argv);
  }

  if (argc > 6 && strcmp(argv[1], "--judge-zero") == 0) {
    return judgeZero(argc, argv);
  }

  if (argc > 3 && strcmp(argv[1], "--servo-calib-scan") == 0) {
    return servoCalibScan(argc, argv);
  }

  if (argc > 3 && strcmp(argv[1], "--calib") == 0) {
    return calibRoundTrip(argc, argv);
  }

  if (argc > 2 && strcmp(argv[1], "--calib-decode") == 0) {
    return calibDecode(argc, argv);
  }

  if (argc > 3 && strcmp(argv[1], "--servo-write8") == 0) {
    uint8_t packet[16];
    size_t len = buildServoWrite8((uint8_t)strtoul(argv[2], nullptr, 0),
                                  (uint8_t)strtoul(argv[3], nullptr, 0),
                                  (uint8_t)strtoul(argv[4], nullptr, 0),
                                  packet, sizeof(packet));
    if (len == 0) { fprintf(stderr, "write8 build failed\n"); return 2; }
    printHex(packet, len);
    return 0;
  }

  if (argc > 4 && strcmp(argv[1], "--servo-write16") == 0) {
    uint8_t packet[16];
    size_t len = buildServoWrite16((uint8_t)strtoul(argv[2], nullptr, 0),
                                   (uint8_t)strtoul(argv[3], nullptr, 0),
                                   (uint16_t)strtoul(argv[4], nullptr, 0),
                                   packet, sizeof(packet));
    if (len == 0) { fprintf(stderr, "write16 build failed\n"); return 2; }
    printHex(packet, len);
    return 0;
  }

  if (argc > 4 && strcmp(argv[1], "--servo-register-read") == 0) {
    uint8_t packet[16];
    size_t len = buildServoRegisterRead(
        (uint8_t)strtoul(argv[2], nullptr, 0),
        (uint8_t)strtoul(argv[3], nullptr, 0),
        (uint8_t)strtoul(argv[4], nullptr, 0), packet, sizeof(packet));
    if (len == 0) { fprintf(stderr, "read build failed\n"); return 2; }
    printHex(packet, len);
    return 0;
  }

  if (argc > 2 && strcmp(argv[1], "--response-for") == 0) {
    rocsar_v1_PicoCommand command =
        rocsar_v1_PicoCommand_init_zero;
    if (decodeFrame(argv[2], &command) != 0) {
      return 2;
    }
    bool ok = (argc <= 3) || strcmp(argv[3], "ok") == 0;
    emitResponse(command.sequence, ok,
                 ok ? rocsar_v1_ErrorCode_ERROR_NONE
                    : rocsar_v1_ErrorCode_ERROR_INVALID_SERVO);
    return 0;
  }

  if (argc > 1) {
    rocsar_v1_PicoCommand ignored =
        rocsar_v1_PicoCommand_init_zero;
    return decodeFrame(argv[1], &ignored);
  }

  emitTelemetry();
  emitResponse(11, true, rocsar_v1_ErrorCode_ERROR_NONE);
  emitResponse(12, false, rocsar_v1_ErrorCode_ERROR_INVALID_SERVO);
  return 0;
}

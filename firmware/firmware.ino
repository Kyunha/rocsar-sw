#include <Arduino.h>
#include <Wire.h>
#include <Adafruit_Sensor.h>
#include <Adafruit_BNO055.h>

// Nanopb & Protocol includes (using local quotes)
#include "pb_encode.h"
#include "pb_decode.h"
#include "pico.pb.h"
#include "cobs.h"
#include "pico_wire.h"

// The alignment record's format and its EEPROM home. calibration.h is pure
// arithmetic and is compiled and driven on the host by firmware/tests; the store
// is the only Arduino-aware part of it.
#include "calibration.h"
#include "calibration_store.h"

// Kinematics, command handling and the ST3215 status parser live in
// gondola_model.h, which touches no hardware and can therefore be compiled and
// driven on a host. This sketch keeps only what needs pins, timers and the UART.
#include "gondola_model.h"

// ============================================================================
// HARDWARE CONFIGURATION
// ============================================================================
// The ST3215 bus rate, on Serial1. NOT the USB CDC rate to the Pi, which is
// PI_CDC_BAUD below and is set separately in setup(). Two buses, two constants,
// one number: changing this one does not change the other, and they are easy to
// confuse precisely because they match. Neither is allowed to be a bare literal
// -- a reader who has seen one 115200 has no way to know the other is a
// different wire.
#define SERVO_BAUD 115200    // ST3215 UART Baud Rate
#define PI_CDC_BAUD 115200   // USB CDC to the Pi. Coincidentally the same number.
#define BNO_SDA_PIN 16      // RP2040 I2C SDA
#define BNO_SCL_PIN 17      // RP2040 I2C SCL
#define HEATER_1_PIN 4      // Servo 1 Heating Module GPIO
#define HEATER_2_PIN 5      // Servo 2 Heating Module GPIO
#define CONTROL_LOOP_MS 20  // 50 Hz Update Rate

// Note what is *not* here: IMU_ALPHA. The heading filter's weight belongs to
// gondola_model.h, next to the applyImuHeading() that applies it. A second filter
// declared here would stack with that one -- two EMAs give an effective weight of
// 1-(1-a)^2, which is 0.28 and not the 0.15 written down there, and nobody
// reading either file would see it.

// ============================================================================
// GLOBAL STATE
// ============================================================================
GondolaState gondola;
Adafruit_BNO055 bno = Adafruit_BNO055(55, 0x28, &Wire);

unsigned long lastLoopTime = 0;
// The heading policy lives in applyImuHeading() in gondola_model.h. Sensor
// presence has one home -- GondolaState.imuPresent, written by that function and
// read back here -- rather than a parallel latch that could disagree with the
// state it is supposed to describe.
unsigned long lastImuProbeTime = 0;

// Serial RX Buffer for COBS
uint8_t rxBuffer[256];
size_t rxLen = 0;

// Outgoing sequence numbers for the PicoMessage wrapper
uint32_t txSequence = 0;

// ============================================================================
// PROTOBUF & COBS COMMUNICATIONS
// ============================================================================
// encodePicoFrame() lives in pico_wire.h so the sketch and anything else that
// encodes a frame go through the same one.
bool sendMessage(const rocsar_v1_PicoMessage& message) {
  // No host listening. Serial (USB CDC) reports false until the far end has
  // opened the port, and writing to a CDC that nobody reads fills the TX
  // buffer and then blocks -- which would stall the 20 ms control tick and
  // freeze the antennas because nobody was there to read their telemetry.
  // A frame with no reader is not a frame, so drop it and keep looping.
  if (!Serial) {
    return false;
  }
  uint8_t frame[PICO_TX_FRAME_MAX];
  size_t frameLen = encodePicoFrame(message, frame, sizeof(frame));
  if (frameLen == 0) {
    return false;
  }
  Serial.write(frame, frameLen);
  return true;
}

bool sendCommandResponse(uint32_t sequence, rocsar_v1_ErrorCode error) {
  rocsar_v1_PicoMessage message = rocsar_v1_PicoMessage_init_zero;

  message.sequence = txSequence++;
  message.timestamp_us = micros();
  message.which_payload = rocsar_v1_PicoMessage_ack_tag;
  message.payload.ack.command_sequence = sequence;
  message.payload.ack.success =
      (error == rocsar_v1_ErrorCode_ERROR_NONE);
  message.payload.ack.error = error;

  return sendMessage(message);
}

// ============================================================================
// IMU SENSING
// ============================================================================
// This function only plumbs the sensor. The heading policy and the EMA filter are
// in gondola_model.h, next to the alpha they apply -- see the note at the top of
// this file for why a second filter here would be invisible.
void readImuHeading(unsigned long now) {
  if (!gondola.imuPresent && (now - lastImuProbeTime >= IMU_REPROBE_MS)) {
    lastImuProbeTime = now;
    if (bno.begin()) {
      bno.setExtCrystalUse(true);
      gondola.imuPresent = true;
    }
  }

  if (!gondola.imuPresent) {
    // Hold. applyImuHeading records the absence so the telemetry can say so.
    applyImuHeading(gondola, 0.0f, false);
    return;
  }

  sensors_event_t event;
  if (!bno.getEvent(&event, Adafruit_BNO055::VECTOR_EULER)) {
    // A fitted sensor that delivered nothing this tick. Hold, and count it.
    //
    // Note this deliberately does NOT call applyImuHeading(gondola, 0.0f, true):
    // a 0.0f sample is finite, so that call would pass the guard and integrate
    // a reading of zero, dragging the bearing toward north. "No sample" and
    // "a sample of 0 degrees" are different facts and only one of them is a
    // measurement. `imuPresent` stays as it is for as long as the sensor keeps
    // answering at all -- but not forever: after IMU_MISSED_SAMPLES_MAX
    // consecutive silence the sensor is declared absent through the same
    // applyImuHeading(..., false) path a missing sensor takes, so the re-probe
    // above gets a chance to run and the wire stops claiming a heading source
    // it has not heard from in a second.
    if (noteImuMiss(gondola)) {
      applyImuHeading(gondola, 0.0f, false);
    }
    return;
  }

  clearImuMisses(gondola);
  applyImuHeading(gondola, event.orientation.x, true);
  // All three Euler angles arrive in this one read and two of them were being
  // thrown away. Tilt is not filtered: it exists to be a confidence signal on the
  // heading, and filtering the evidence would only delay it.
  applyImuAttitude(gondola, event.orientation.y, event.orientation.z, true);

  // Read alongside the heading so the two agree about the sensor: a miss
  // above holds the last temperature rather than zeroing it, the same posture
  // as the bearing and the servo readings. The BNO055 reports whole degrees;
  // a tenth of a degree is far finer than anyone acts on.
  gondola.imuTemperatureC = (float)bno.getTemp();
}

// One register read, the whole reason it is here: whether the sensor is
// calibrated. A 2-24 hour flight with a night-to-day temperature swing at
// altitude degrades the BNO055's fusion, and `imu_present` only ever said that
// a sensor answered -- which an uncalibrated one does just as readily.
void readImuCalibration() {
  uint8_t system = 0, gyroscope = 0, accelerometer = 0, magnetometer = 0;
  bno.getCalibration(&system, &gyroscope, &accelerometer, &magnetometer);
  setImuCalibration(gondola, packImuCalibration(system, gyroscope, accelerometer,
                                                magnetometer));
}

// Feeds the peak-hold accumulator.
//
// A second getEvent(), and it has to be a second one: the BNO055 fills the
// sensors_event_t union with one vector per call, so asking for linear
// acceleration through the same read as the Euler angles would silently return
// the Euler angles again and peak-hold them as if they were accelerations.
// That is why the two are separate functions rather than one read.
void readImuPeakAccel() {
  sensors_event_t linear;
  if (!bno.getEvent(&linear, Adafruit_BNO055::VECTOR_LINEARACCEL)) {
    // Not a miss for the presence counter. That counts getEvent failures on the
    // *heading* read, which is what the re-probe and the wire's presence bit are
    // about; a second read failing says nothing about whether the sensor is there,
    // and counting it would declare a working sensor absent.
    return;
  }

  // The type check is not ceremony.
  //
  // Adafruit_BNO055 fills the SAME union member -- event->acceleration -- for
  // VECTOR_LINEARACCEL, VECTOR_ACCELEROMETER and VECTOR_GRAVITY, and
  // distinguishes them only by event->type. So asking for the wrong one yields
  // plausible accelerations with gravity still in them, and nothing about the
  // numbers looks wrong: the raw accelerometer would peak-hold at 1 g on a level
  // gondola and the peak would never mean a shock. The type is the only thing in
  // the event that says which of the three this actually is.
  if (linear.type != SENSOR_TYPE_LINEAR_ACCELERATION) {
    return;
  }

  noteImuPeakAccel(gondola, linear.acceleration.x, linear.acceleration.y,
                   linear.acceleration.z);
}

void sendTelemetryMessage() {
  rocsar_v1_PicoMessage message = rocsar_v1_PicoMessage_init_zero;

  message.sequence = txSequence++;
  message.timestamp_us = micros();
  message.which_payload = rocsar_v1_PicoMessage_telemetry_tag;

  // The mapping from model to wire lives in gondola_model.h, so this calls
  // fillTelemetryMessage() rather than filling the struct by hand; see there.
  fillTelemetryMessage(gondola, message.payload.telemetry);

  sendMessage(message);
}

// The model owns heater state and the dead-man clock behind it; the pins are
// hardware, so this is the one place that translates one into the other. It is
// idempotent and called from both places the flags can change -- after a
// command, for an immediate response, and after expireHeaters() in the loop,
// for the silent case where the keep-alive simply stopped arriving.
void syncHeaterPins() {
  digitalWrite(HEATER_1_PIN, gondola.heater1 ? HIGH : LOW);
  digitalWrite(HEATER_2_PIN, gondola.heater2 ? HIGH : LOW);
}

// ============================================================================
// COMMAND PARSER & EXECUTION
// ============================================================================
// Defined in the ST3215 section below. Forward-declared rather than relied upon
// to be reordered, because a sketch whose compilation depends on definition
// order is one edit away from not compiling.
ServoZeroOutcome zeroServoHardware(uint8_t id, uint16_t& verifiedCentreTick);
bool readServoRegisterHardware(uint8_t id, ServoCalibRead& out);

// Handles a `zero`, which is the one command applyCommand() does not finish.
//
// The command itself has not changed -- ZeroCommand still carries a servo_id and
// nothing else -- but what it now does has: instead of overwriting a firmware
// variable with the current reading, it writes a position offset into the
// ST3215's own EEPROM so the servo reports its centre tick at the boresight.
// That needs the bus, so it happens here and not in the model.
//
// The three model functions are the whole policy and each is named for what it
// settles: which axis, did it take, record it. The ordering is load-bearing --
// the ack below is built from the hardware's answer, so a teach that failed
// cannot report success, which is the one thing the old three-line version got
// wrong.
void handleZeroCommand(const rocsar_v1_PicoCommand &cmd) {
  int axisIndex = validateZeroCommand(gondola, cmd);
  if (axisIndex < 0) {
    sendCommandResponse(cmd.sequence,
                        rocsar_v1_ErrorCode_ERROR_INVALID_SERVO);
    return;
  }

  uint16_t verifiedCentre = ST3215_SERVO_CENTRE_TICK;
  ServoZeroOutcome outcome =
      zeroServoHardware(gondola.antennas[axisIndex].id, verifiedCentre);

  if (outcome == ZERO_OK) {
    commitServoZero(gondola, axisIndex, verifiedCentre);

    // Persist the fact that a teach took. This is the only place the record
    // changes.
    //
    // It does sit between the bus work and the acknowledgement, and that is
    // deliberate rather than accidental: EEPROMClass::commit() takes interrupts
    // off for a whole sector erase, so the cost is paid either way, and paying it
    // before the ack means the ack is sent from a board whose record already
    // matches what it is reporting. Doing it after would risk acknowledging a
    // teach and then losing the record that says so, with the servo already
    // holding a correction nobody has a note about.
    //
    // Neither ordering rescues the control tick: handleZeroCommand is called from
    // loop(), so the tick is stalled for the erase either way. That is part of the
    // 100-250 ms the acknowledgement timeout is documented against.
    CalibrationRecord record;
    snapshotCalibration(gondola, record);
    calibrationSave(record);
  }

  // One mapping, in the model, so there is one place that knows a dead servo
  // (nothing written) is a different fact from a teach that did not take (the
  // servo may be holding a correction nobody can vouch for).
  sendCommandResponse(cmd.sequence, servoZeroErrorCode(outcome));
}

void handleCommand(const rocsar_v1_PicoCommand &cmd) {
  // `zero` needs the servo bus and a verified result, so it is dispatched before
  // the model sees it. Everything else is the model alone, which is what keeps
  // policy testable on a host.
  if (cmd.which_payload == rocsar_v1_PicoCommand_zero_tag) {
    handleZeroCommand(cmd);
    return;
  }

  // millis() is passed because an "on" starts the heater dead-man clock; see
  // applyCommand() for why the clock is command data rather than a sketch
  // concern.
  rocsar_v1_ErrorCode error = applyCommand(gondola, cmd, millis());

  // The model tracks heater state; the pins have to follow.
  syncHeaterPins();

  // Always close the loop!
  sendCommandResponse(cmd.sequence, error);
}

// ============================================================================
// ST3215 SERVO UART DRIVER (WRITE & HARDWARE TELEMETRY READ)
// ============================================================================
// The write packet itself is built by buildServoPacket() in gondola_model.h.
// All that is left here is the part that is genuinely hardware: writing it to the
// bus and waiting for the shift register to drain, which keeps the 13 bytes
// contiguous on the wire.
void sendServoPosition(uint8_t id, int32_t position) {
  uint8_t pkt[SERVO_PACKET_LEN];

  size_t len = buildServoPacket(id, position, pkt, sizeof(pkt));
  if (len == 0) {
    return;
  }

  Serial1.write(pkt, len);
  Serial1.flush();
}

// Asks one servo for its status block and waits for the answer.
//
// The framing -- the request, the checksum, and every rule about what counts as
// a valid reply -- is buildServoStatusRequest() and scanServoStatus() in
// gondola_model.h. Only the three things that genuinely need hardware are here:
// putting the request on the wire, letting time pass, and taking bytes off it.
// That split is the whole point. The version this replaces built its own packet,
// computed its own checksum and parsed the reply inline in the sketch, where the
// only way to test any of it was to flash a board and read a serial console.
//
// On the bus echo: TX and RX are the same conductor, so the Pico hears itself and
// the 8-byte request comes straight back with LEN=0x04. The scanner skips it
// structurally -- a status reply carries LEN=0x0A and an echo cannot -- so there
// is no byte count here to be wrong about. A previous version drained exactly 8
// bytes on the assumption the echo was always there and exactly that long, which
// silently ate the first 8 bytes of a real reply if the assumption failed and
// reported the result as a dead servo.
//
// The timeout is bounded and the loop inside it is not a busy-wait on the wire:
// it polls available() and yields, so the RP2040's other work is not starved for
// the duration. At SERVO_STATUS_WAIT_MS the worst case is bounded by the reply's
// own length -- 14 bytes is about 1.2 ms at SERVO_BAUD -- so this normally
// returns in single-digit milliseconds and only reaches the bound on a servo that
// has stopped answering.
bool readServoTelemetryHardware(uint8_t id, ServoStatus &out) {
  uint8_t request[SERVO_STATUS_REQUEST_LEN];
  size_t requestLen = buildServoStatusRequest(id, request, sizeof(request));
  if (requestLen == 0) {
    return false;
  }

  Serial1.write(request, requestLen);
  Serial1.flush();

  uint8_t buffer[SERVO_STATUS_BUFFER_LEN];
  size_t filled = 0;
  unsigned long deadline = millis() + SERVO_STATUS_WAIT_MS;

  while (true) {
    while (Serial1.available() > 0 && filled < sizeof(buffer)) {
      buffer[filled++] = (uint8_t)Serial1.read();
    }

    ServoScan scan = scanServoStatus(buffer, filled, id);
    if (scan.matched) {
      out = scan.status;
      return true;
    }

    // Consume what the scanner settled on even though it did not match --
    // otherwise a byte it has already rejected would be re-examined on every
    // pass and a corrupt LEN could wedge the read forever. Anything it left in
    // place is a partial frame whose remainder may still be arriving.
    if (scan.consumed > 0) {
      size_t remaining = filled - scan.consumed;
      for (size_t i = 0; i < remaining; i++) {
        buffer[i] = buffer[i + scan.consumed];
      }
      filled = remaining;
    }

    if ((int32_t)(millis() - deadline) >= 0) {
      break;
    }
    delay(1);
  }

  return false;
}

// ============================================================================
// ST3215 EEPROM: TEACHING A SERVO ITS CENTRE
// ============================================================================
// Everything in this section is bus work. The rules -- which offset to write,
// whether the write took, what that means for the model -- are in
// gondola_model.h, and the only reason they are not on one line here is that
// they are the part that has to be provable without a servo attached.

// Drops whatever is in the UART's receive path.
//
// The bus is half-duplex, so every request we send comes back as an echo, and a
// write is answered as well. The official configure sketch drains before each
// read for the same reason. Doing it here as a separate step -- rather than
// letting each scanner cope -- means the scanners only ever see frames that
// arrived after the request they are answering.
void drainServoBus() {
  while (Serial1.available() > 0) {
    Serial1.read();
  }
}

// Writes one register. Fire-and-forget by design, matching the vendor's own
// writeReg8/writeReg16: neither reads an acknowledgement, and every write here
// is proved by the read-back that follows rather than by its own reply.
void writeServoRegister(uint8_t id, uint8_t reg, uint8_t value) {
  uint8_t pkt[9];
  size_t len = buildServoWrite8(id, reg, value, pkt, sizeof(pkt));
  if (len == 0) {
    return;
  }
  Serial1.write(pkt, len);
  Serial1.flush();
}

void writeServoRegister16(uint8_t id, uint8_t reg, uint16_t value) {
  uint8_t pkt[9];
  size_t len = buildServoWrite16(id, reg, value, pkt, sizeof(pkt));
  if (len == 0) {
    return;
  }
  Serial1.write(pkt, len);
  Serial1.flush();
}

// Reads the four calibration registers and waits for the answer.
//
// The mirror of readServoTelemetryHardware(), including the bounded wait and the
// yielding poll loop: the teach sequence is blocking by design, but it should not
// busy-wait the wire while it does. The difference is the scanner -- a 10-byte
// frame at LEN=0x06, which collides with neither the 8-byte request echo nor the
// 14-byte status reply, so this cannot mistake our own echo for an answer.
bool readServoRegisterHardware(uint8_t id, ServoCalibRead &out) {
  uint8_t request[SERVO_STATUS_REQUEST_LEN];
  size_t requestLen =
      buildServoRegisterRead(id, REG_ST3215_ANGULAR_RESOLUTION,
                             ST3215_CALIB_READ_LEN, request, sizeof(request));
  if (requestLen == 0) {
    return false;
  }

  drainServoBus();
  Serial1.write(request, requestLen);
  Serial1.flush();

  uint8_t buffer[SERVO_STATUS_BUFFER_LEN];
  size_t filled = 0;
  unsigned long deadline = millis() + SERVO_STATUS_WAIT_MS;

  while (true) {
    while (Serial1.available() > 0 && filled < sizeof(buffer)) {
      buffer[filled++] = (uint8_t)Serial1.read();
    }

    ServoCalibRead scan = scanServoCalibRead(buffer, filled, id);
    if (scan.matched) {
      out = scan;
      return true;
    }

    // Consume what the scanner rejected even though nothing matched, for the
    // same reason readServoTelemetryHardware() does: a byte it has already
    // refused must not be re-examined forever, or a corrupt LEN wedges the read.
    if (scan.consumed > 0) {
      size_t remaining = filled - scan.consumed;
      for (size_t i = 0; i < remaining; i++) {
        buffer[i] = buffer[i + scan.consumed];
      }
      filled = remaining;
    }

    if ((int32_t)(millis() - deadline) >= 0) {
      break;
    }
    delay(1);
  }

  return false;
}

// One pass of the EEPROM unlock / correction / relock sequence.
//
// `correctionByte` and `correctionValue` are what gets written between the
// unlock and the relock, so both paths share one sequence and one set of vendor
// settle delays rather than each carrying its own copy.
//
// Kept separate from the caller because the two paths differ only in that one
// write, and the differences that matter -- which register, what value, and what
// the servo should then report -- belong to the two functions that own them.
bool applyServoCorrection(uint8_t id, uint8_t correctionByte,
                          uint8_t correctionValue) {
  writeServoRegister(id, REG_ST3215_EEPROM_LOCK, ST3215_EEPROM_UNLOCKED);
  delay(ST3215_EEPROM_SETTLE_MS);

  writeServoRegister(id, correctionByte, correctionValue);
  delay(ST3215_EEPROM_SETTLE_MS);

  // Re-lock even if the correction write was lost. Leaving a servo's EEPROM
  // unlocked is a thing a later, unrelated write could then land in, and there is
  // no way to know from here whether it took.
  writeServoRegister(id, REG_ST3215_EEPROM_LOCK, ST3215_EEPROM_LOCKED);
  delay(ST3215_EEPROM_SETTLE_MS);
  return true;
}

// Reads 0x1E..0x1F and the position, and judges a teach that has already been
// attempted. Shared by both paths so the two cannot disagree about how to verify.
ServoZeroOutcome verifyServoTeach(uint8_t id, bool expectCorrection,
                                  uint16_t wroteCorrection) {
  ServoCalibRead regs;
  if (!readServoRegisterHardware(id, regs)) {
    return ZERO_CORRECTION_MISMATCH;
  }

  ServoStatus after;
  if (!readServoTelemetryHardware(id, after)) {
    return ZERO_POSITION_MISMATCH;
  }

  return judgeServoZero(expectCorrection, wroteCorrection, regs.positionOffset,
                        regs.angularResolution, after.positionTicks);
}

// Teaches one servo its centre and reports whether it took.
//
// Two paths, in order of preference.
//
// The first writes 128 to the Torque switch (0x28), which the vendor's memory
// table documents as "current position correction is 2048": the servo works out
// the correction from its own encoder. That is preferred over doing the sum here
// because 0x1F is a *signed* -2047..+2047 register with bit 11 as the direction
// bit, and the wire encoding of a negative correction is not documented -- two
// encodings fit the description and they disagree. Asking the servo removes the
// question. It is also, almost certainly, what Waveshare's "Set Middle Position"
// button sends.
//
// The second writes the correction to 0x1F directly. It exists for a board whose
// firmware revision does not implement the 0x28 command, and it is a fallback
// rather than an equal path: it can refuse a servo sitting at exactly tick 0,
// because a correction of +2048 does not fit the documented range, and clamping
// it would teach the wrong centre.
//
// Both paths are verified the same way and both are idempotent, so a run that
// half-failed -- a write lost to bus contention, a servo pulled mid-sequence --
// is fixed by running it again.
//
// Blocking, and on purpose. This runs on an explicit operator command and takes
// 100-250 ms, during which the control loop does not tick and no telemetry goes
// out. That is an acceptable cost for an action that should happen a handful of
// times per session, and it buys an acknowledgement that means something: an
// asynchronous version would have to answer "accepted" before knowing whether the
// EEPROM write landed, which is the failure this is most careful about.
//
// Note what is never written: 0 or 1 to 0x28. Those turn torque off and on, and
// torque-off on a 5:1 gear train carrying an antenna means the axis goes limp.
// Only the 128 command is sent to that register.
ServoZeroOutcome zeroServoHardware(uint8_t id, uint16_t &verifiedCentreTick) {
  verifiedCentreTick = ST3215_SERVO_CENTRE_TICK;

  // Take our own reading rather than trusting whatever the alternating poll last
  // left in the model. It costs one bus round trip and it removes a race: a
  // `zero` issued in the first 40 ms after boot would otherwise have nothing to
  // measure from.
  ServoStatus before;
  if (!readServoTelemetryHardware(id, before)) {
    return ZERO_NO_SERVO_REPLY;
  }

  applyServoCorrection(id, REG_ST3215_TORQUE_SWITCH,
                       ST3215_CORRECT_POSITION_COMMAND);
  ServoZeroOutcome outcome =
      verifyServoTeach(id, false, 0);

  if (outcome != ZERO_POSITION_MISMATCH && outcome != ZERO_CORRECTION_MISMATCH) {
    // Either it took, or it failed for a reason the fallback cannot fix (the
    // tick scale changed, or the servo stopped answering). Falling back on those
    // would be guessing.
    if (outcome == ZERO_OK) {
      verifiedCentreTick = ST3215_SERVO_CENTRE_TICK;
    }
    return outcome;
  }

  // The 0x28 command was not honoured. Fall back to writing the correction.
  bool representable = false;
  int32_t correction =
      servoZeroCorrection(before.positionTicks, ST3215_SERVO_CENTRE_TICK,
                          representable);
  if (!representable) {
    return ZERO_UNREPRESENTABLE;
  }

  uint16_t encoded = (uint16_t)(correction & 0xFFFF);
  // Two bytes, so it does not go through applyServoCorrection()'s one-byte
  // write. The unlock and relock around it are the same sequence.
  writeServoRegister(id, REG_ST3215_EEPROM_LOCK, ST3215_EEPROM_UNLOCKED);
  delay(ST3215_EEPROM_SETTLE_MS);
  writeServoRegister16(id, REG_ST3215_POSITION_CORRECTION, encoded);
  delay(ST3215_EEPROM_SETTLE_MS);
  writeServoRegister(id, REG_ST3215_EEPROM_LOCK, ST3215_EEPROM_LOCKED);
  delay(ST3215_EEPROM_SETTLE_MS);

  outcome = verifyServoTeach(id, true, encoded);
  if (outcome == ZERO_OK) {
    verifiedCentreTick = ST3215_SERVO_CENTRE_TICK;
  }
  return outcome;
}

// ============================================================================
// SETUP & MAIN LOOP
// ============================================================================
void setup() {
  Serial.begin(PI_CDC_BAUD);  // Pure COBS stream to Pi

  pinMode(HEATER_1_PIN, OUTPUT);
  pinMode(HEATER_2_PIN, OUTPUT);

  // ST3215 Serial Bus
  Serial1.setTX(0);
  Serial1.setRX(1);
  Serial1.begin(SERVO_BAUD);

  // I2C for BNO055
  Wire.setSDA(BNO_SDA_PIN);
  Wire.setSCL(BNO_SCL_PIN);
  Wire.begin();

  initGondolaState(gondola);

  // Restore the alignment record. The centre itself lives in each servo's own
  // EEPROM -- that is what `zero` writes -- so all that comes back from here is
  // the fact that a teach took, which the servo cannot report for itself.
  //
  // Not fatal on failure, and deliberately so. A missing or unreadable record
  // leaves every axis untaught, which rides the wire as center_zeroed = false
  // and is therefore visible, rather than being a silent return to something
  // that looks like a calibration. The one line below is on the CDC at boot
  // because this is the only moment an operator is guaranteed to see it.
  if (calibrationBegin()) {
    CalibrationRecord record;
    CalibrationResult loaded = calibrationLoad(record);
    if (loaded == CALIBRATION_OK) {
      applyCalibration(gondola, record);
    }
    if (loaded != CALIBRATION_OK) {
      Serial.printf("[calib] %s -- axes report an assumed centre\n",
                    calibrationResultName(loaded));
    }
  } else {
    Serial.println("[calib] EEPROM unavailable -- axes report an assumed centre");
  }

  // Both heaters off, written through the one function that ever touches their
  // pins -- so "the pins mirror the model" is true from the first line of setup
  // and there is no second writer to drift from it.
  syncHeaterPins();

  if (bno.begin()) {
    bno.setExtCrystalUse(true);
    gondola.imuPresent = true;
  } else {
    // Not fatal: a bench Pico may never have an IMU fitted, and holding the
    // last bearing is the same posture this firmware already takes for a lost
    // Pi link. The loop re-probes (see readImuHeading), so fitting one later is
    // picked up without a reflash. Until then `imuPresent` rides the wire and
    // the operator can see the heading is held rather than measured.
    gondola.imuPresent = false;
  }

  lastLoopTime = millis();
}

void loop() {
  // 1. Parse Incoming USB Serial COBS Commands
  while (Serial.available() > 0) {
    uint8_t c = Serial.read();

    if (c == 0x00) {  // Packet Delimiter
      if (rxLen > 0) {
        uint8_t decBuf[256];
        size_t decLen = cobs_decode(rxBuffer, rxLen, decBuf);

        rocsar_v1_PicoCommand cmd = rocsar_v1_PicoCommand_init_zero;
        pb_istream_t stream = pb_istream_from_buffer(decBuf, decLen);

        if (decLen > 0 &&
            pb_decode(&stream, rocsar_v1_PicoCommand_fields, &cmd)) {
          handleCommand(cmd);
        }
        rxLen = 0;
      }
    } else {
      if (rxLen < sizeof(rxBuffer)) {
        rxBuffer[rxLen++] = c;
      } else {
        rxLen = 0;  // Overflow protection
      }
    }
  }

  // 2. Deterministic 50 Hz Control Loop
  unsigned long now = millis();
  if (now - lastLoopTime >= CONTROL_LOOP_MS) {
    lastLoopTime = now;

    // Smooth IMU Reading
    readImuHeading(now);
    // Both of these depend on the sensor the read above just used, so they are
    // called unconditionally rather than guarded on imuPresent: getEvent() fails
    // harmlessly against an absent sensor, and skipping them on a flag would mean
    // a sensor that answers intermittently leaves a stale calibration byte and a
    // peak that silently stops updating.
    readImuCalibration();
    readImuPeakAccel();

    // Dead-man first, so the telemetry frame this tick already reports the
    // truth: a heater whose keep-alive stopped arriving goes off here, and the
    // pins follow. expireHeaters() only ever turns things off, so this can
    // never start heating on its own.
    expireHeaters(gondola, now);
    syncHeaterPins();

    // Update Kinematics & Actuate Servos
    for (int i = 0; i < NUM_ANTENNAS; i++) {
      AntennaAxis& antenna = gondola.antennas[i];
      uint16_t targetTick =
          antenna.calculateTargetTick(gondola.gondolaHeading, gondola.targetHeading);

      // shouldDriveServo() is the boot interlock plus the deadband, in that
      // order: an axis nobody has commanded transmits nothing, so powering up
      // moves nothing until set_target or jog says otherwise.
      if (shouldDriveServo(antenna, targetTick)) {
        sendServoPosition(antenna.id, targetTick);
        antenna.lastSentTick = targetTick;
      }

      // The fallback, and a fallback only: this derives the position from the
      // last command, and does nothing at all once applyServoFeedback() has put a
      // real reading on the axis. Before the ST3215 read existed this was the only
      // thing writing those fields, which is how the GUI ended up showing a
      // healthy-looking 0 on a motor that had not moved.
      updateAxisFeedback(antenna);
    }

    // 3. Read one servo's status block, alternating between the two.
    //
    // One poll per control tick, not two: the bus is half-duplex and a read is a
    // round trip, so each servo is polled every 2 * CONTROL_LOOP_MS (40 ms at the
    // shipped 20 ms tick). That is fast enough for a 0.088-degree-per-tick axis to
    // look continuous and slow enough to leave the 20 ms budget alone -- see the
    // timing note on SERVO_STATUS_WAIT_MS.
    static uint8_t pollIndex = 0;
    AntennaAxis& polled = gondola.antennas[pollIndex];
    ServoStatus status;
    if (readServoTelemetryHardware(polled.id, status)) {
      applyServoFeedback(polled, status);
    } else {
      // Hold the last reading and say so. Zeroing the fields here would be the
      // one thing this must never do: a servo that stopped answering would then
      // be indistinguishable from one reporting a genuine zero load.
      invalidateAxisFeedback(polled);
    }
    pollIndex = (pollIndex + 1) % NUM_ANTENNAS;

    // 4. Emit Telemetry Frame over Protobuf/COBS
    sendTelemetryMessage();
  }
}

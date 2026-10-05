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
  // Read alongside the heading so the two agree about the sensor: a miss
  // above holds the last temperature rather than zeroing it, the same posture
  // as the bearing and the servo readings. The BNO055 reports whole degrees;
  // a tenth of a degree is far finer than anyone acts on.
  gondola.imuTemperatureC = (float)bno.getTemp();
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
void handleCommand(const rocsar_v1_PicoCommand &cmd) {
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

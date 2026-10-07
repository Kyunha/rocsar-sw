#pragma once

// Single source of truth for the Pico wire encoding.
//
// One encoder, one framing. The sketch includes this and then does nothing but
// "write the bytes to Serial" on top of encodePicoFrame(); anything that encodes
// a frame -- the sketch, a host probe, a future tool -- goes through here, so a
// change to the framing cannot land on one side only.

#include <stddef.h>
#include <stdint.h>
#include <string.h>

#include "cobs.h"
#include "pb_encode.h"
#include "pico.pb.h"

// PICO_TX_BUFFER bounds the protobuf payload. cobs_encode() does not bounds-check
// its output buffer, so the COBS buffer must be sized from the documented worst
// case (length + length/254 + 1) rather than from a guessed constant.
#define PICO_TX_BUFFER 256
#define COBS_MAX(length) ((length) + ((length) / 254) + 1)

// Largest frame the sender can ever emit, including the 0x00 delimiter.
#define PICO_TX_FRAME_MAX (COBS_MAX(PICO_TX_BUFFER) + 1)

// nanopb publishes the worst-case encoded size of every message it generates.
// It is currently 244 bytes: two antennas at 81 each, the IMU channels
// (attitude, calibration, peak-hold acceleration) at 37, plus the PicoMessage
// header. PICO_TX_BUFFER is 256, so there is 12 bytes of headroom.
//
// That number was wrong twice before this comment caught up -- it said 198 with
// 58 spare, from before AntennaTelemetry lost its optional fields and gained
// center_zeroed, and the drift was invisible because the static_assert below
// compares against the real constant rather than against this paragraph. It is
// recorded here because the assert protects the build and nothing protected the
// prose, and a stale budget is how the next person decides there is room for a
// 20-byte field that there is not.
//
// This is asserted rather than assumed, because the failure without it is
// silent and total: pb_encode() returns false when the message does not fit,
// encodePicoFrame() returns 0, and the firmware then sends nothing at all while
// reporting no error. Nothing on the flight controller or the OBC would say why.
//
// Adding a third antenna, a wider field, or a message with more of them can each
// push this over, and every one of those is a reasonable thing to want. The
// build should stop, not the sky.
static_assert(rocsar_v1_PicoMessage_size <= PICO_TX_BUFFER,
              "PICO_TX_BUFFER is smaller than the worst-case encoded PicoMessage "
              "(rocsar_v1_PicoMessage_size). pb_encode would fail at runtime and "
              "the flight controller would transmit nothing, silently. Raise "
              "PICO_TX_BUFFER, and re-check PICO_TX_FRAME_MAX against the host's "
              "read buffer.");

// Encodes one PicoMessage into a COBS frame with a trailing 0x00 delimiter.
// Returns the frame length in bytes, or 0 if the message or frame is invalid.
inline size_t encodePicoFrame(const rocsar_v1_PicoMessage& message,
                              uint8_t* out, size_t outLen) {
  uint8_t payload[PICO_TX_BUFFER];
  pb_ostream_t stream = pb_ostream_from_buffer(payload, sizeof(payload));
  if (!pb_encode(&stream, rocsar_v1_PicoMessage_fields, &message)) {
    return 0;
  }

  uint8_t framed[COBS_MAX(PICO_TX_BUFFER)];
  size_t framedLen = cobs_encode(payload, stream.bytes_written, framed);
  if (framedLen + 1 > outLen) {
    return 0;
  }

  memcpy(out, framed, framedLen);
  out[framedLen] = 0x00;
  return framedLen + 1;
}
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
// It is currently 198 bytes: two antennas at 89 each, plus the PicoMessage
// header. PICO_TX_BUFFER is 256, so there is 58 bytes of headroom.
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
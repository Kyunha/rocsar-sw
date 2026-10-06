#pragma once

// The alignment facts the flight controller has to remember, and the format they
// are remembered in.
//
// This is a small, explicit, versioned byte layout rather than a protobuf
// message, and the difference is deliberate. It is a *storage* format: it has to
// stay parseable across firmware versions that have no reason to be wire-
// compatible with each other, it has to be readable on a host with nothing
// linked in, and it has to be rejectable when a write was torn. A fixed layout
// with an explicit little-endian packing and a CRC over the whole record gives
// all three; a protobuf encoding of the same fields gives the last one only by
// accident, because pb_decode() reports success for a frame whose trailing
// fields it never needed.
//
// The bytes are packed by hand rather than memcpy'd from a struct, because a
// struct brings its padding and the host's endianness into a format that has to
// survive both. EEPROMClass::get<T>() and put<T>() -- the obvious tools here --
// are exactly that raw struct memcpy.
//
// What is in it, and what is not, is a consequence of the servo holding its own
// centre. Since `zero` writes a position offset into the ST3215's EEPROM, the
// firmware no longer needs to remember a centre *value*: it is 2048 by
// construction once a teach has taken. What it does need is the fact that a
// teach took at all, and the servo cannot report that -- an axis taught while
// sitting at exactly 2048 stores an offset of zero, which looks identical to one
// never taught. So one bit per axis lives here, and everything else this record
// will grow (mount offsets, direction multipliers, the IMU datum) is the same
// kind of fact: per-installation, lost on every power cycle, silently wrong when
// absent.
//
// The header includes no Arduino and no nanopb, which is what lets
// firmware/tests/conftest.py compile and drive it on a host. The EEPROM itself
// is calibration_store.h, which does include Arduino and is not host-tested.

#include <stddef.h>
#include <stdint.h>
#include <string.h>

#include "gondola_model.h"

// "RCZ1". A wrong magic is the cheapest way to notice that whatever is in flash
// was written by something else, so it is checked before anything else and it is
// a literal rather than a number that could drift.
#define CALIBRATION_MAGIC_0 'R'
#define CALIBRATION_MAGIC_1 'C'
#define CALIBRATION_MAGIC_2 'Z'
#define CALIBRATION_MAGIC_3 '1'

// Bump when the payload layout changes. A record written under a different
// version is rejected rather than reinterpreted: reading a mount offset out of
// bytes that are now a datum is how a board ends up aiming somewhere confident.
#define CALIBRATION_SCHEMA_VERSION 1

#define CALIBRATION_HEADER_SIZE 8
#define CALIBRATION_CRC_SIZE 4
#define CALIBRATION_AXIS_FLAG_ZEROED 0x01

// Fixed, and asserted below rather than computed from the struct: the whole
// point is that the layout does not move when the compiler feels like it.
#define CALIBRATION_RECORD_SIZE \
  (CALIBRATION_HEADER_SIZE + NUM_ANTENNAS + CALIBRATION_CRC_SIZE)

// A record written by a build with a different axis count decodes to nonsense --
// two axes' flags read out of a payload sized for three, and one axis reporting
// another's centre. That check lives in decodeCalibration() because the count is
// data, but this one is arithmetic, and arithmetic about the record's own layout
// is exactly what the compiler should be doing rather than a test.
static_assert(NUM_ANTENNAS > 0 && NUM_ANTENNAS <= 0xFF,
              "CalibrationRecord.axisCount is a byte; NUM_ANTENNAS must fit in one");
static_assert(CALIBRATION_RECORD_SIZE ==
                  CALIBRATION_HEADER_SIZE + NUM_ANTENNAS + CALIBRATION_CRC_SIZE,
              "CALIBRATION_RECORD_SIZE drifted from its layout. It is a storage "
              "format: a size computed from sizeof(CalibrationRecord) instead "
              "would put the struct's padding on the wire.");

// Why a CRC at all, given the Pico also has a checksummed COBS link and a
// checksummed servo protocol two layers down. Because this is the one write here
// with no atomicity: EEPROMClass::commit() erases the sector and reprograms it,
// and a power cut in that window leaves the record unreadable. Without a CRC the
// best available response to that is to believe whatever bytes survived, and the
// consequence is an antenna aiming using a centre that never existed.
//
// This does not protect against a torn *sector erase*, which is the dominant
// failure: flash_range_erase() takes all 4096 bytes as a unit, so a second copy
// alongside the first would be just as lost. One slot, one CRC, and the honest
// answer to an unreadable record is that it was never there.
enum CalibrationResult {
  CALIBRATION_OK = 0,
  CALIBRATION_NO_RECORD = 1,     // all bytes 0xFF: nothing has ever been written
  CALIBRATION_BAD_MAGIC = 2,
  CALIBRATION_BAD_VERSION = 3,
  CALIBRATION_AXIS_COUNT = 4,    // written by a build with a different axis count
  CALIBRATION_BAD_CRC = 5,
};

// What the record carries. Plain values, no model types: this is a decode
// target, and letting it hold an AntennaAxis would make the format depend on the
// struct it is describing.
struct CalibrationRecord {
  uint8_t schemaVersion;
  uint8_t axisCount;
  bool axisZeroed[NUM_ANTENNAS];
};

// CRC-32 (IEEE 802.3, reflected, the same polynomial zlib and nanopb's other
// side use). Bitwise rather than table-driven: it runs a handful of times per
// command and a 1 KB table is RAM this firmware does not need to spend.
//
// Reflected form of x^32 + x^26 + x^23 + x^22 + x^16 + x^12 + x^11 + x^10 +
// x^8 + x^7 + x^5 + x^4 + x^2 + x + 1.
inline uint32_t calibrationCrc32(const uint8_t* data, size_t len) {
  uint32_t crc = 0xFFFFFFFFu;
  for (size_t i = 0; i < len; i++) {
    crc ^= data[i];
    for (int bit = 0; bit < 8; bit++) {
      uint32_t mask = (uint32_t)(-(int32_t)(crc & 1u));
      crc = (crc >> 1) ^ (0xEDB88320u & mask);
    }
  }
  return ~crc;
}

// Serialises `record` into `out`, which must hold CALIBRATION_RECORD_SIZE bytes.
inline bool encodeCalibration(const CalibrationRecord& record, uint8_t* out,
                              size_t outLen) {
  if (out == nullptr || outLen < CALIBRATION_RECORD_SIZE) {
    return false;
  }

  out[0] = CALIBRATION_MAGIC_0;
  out[1] = CALIBRATION_MAGIC_1;
  out[2] = CALIBRATION_MAGIC_2;
  out[3] = CALIBRATION_MAGIC_3;
  out[4] = CALIBRATION_SCHEMA_VERSION;
  out[5] = NUM_ANTENNAS;
  out[6] = 0x00;  // reserved
  out[7] = 0x00;  // reserved

  for (int i = 0; i < NUM_ANTENNAS; i++) {
    out[CALIBRATION_HEADER_SIZE + i] =
        record.axisZeroed[i] ? CALIBRATION_AXIS_FLAG_ZEROED : 0x00;
  }

  size_t crcAt = CALIBRATION_RECORD_SIZE - CALIBRATION_CRC_SIZE;
  uint32_t crc = calibrationCrc32(out, crcAt);
  for (int i = 0; i < 4; i++) {
    out[crcAt + i] = (uint8_t)((crc >> (8 * i)) & 0xFF);
  }
  return true;
}

// Parses a record, filling `record` only on success.
//
// Every failure returns without touching `record`, and the caller is expected to
// carry on with the built-in defaults rather than with a half-filled record. That
// is the point: a partially-applied calibration is precisely the fiction this
// whole change exists to remove, so there is no field-by-field salvage here.
inline CalibrationResult decodeCalibration(const uint8_t* in, size_t len,
                                           CalibrationRecord& record) {
  if (in == nullptr || len < CALIBRATION_RECORD_SIZE) {
    return CALIBRATION_NO_RECORD;
  }

  // All-ones is what an erased sector reads as, and it is the one "no record"
//  // case worth naming: a blank board and a corrupt board are different facts
  // and the operator should be told which one they are looking at.
  bool blank = true;
  for (size_t i = 0; i < CALIBRATION_RECORD_SIZE; i++) {
    if (in[i] != 0xFF) {
      blank = false;
      break;
    }
  }
  if (blank) {
    return CALIBRATION_NO_RECORD;
  }

  if (in[0] != CALIBRATION_MAGIC_0 || in[1] != CALIBRATION_MAGIC_1 ||
      in[2] != CALIBRATION_MAGIC_2 || in[3] != CALIBRATION_MAGIC_3) {
    return CALIBRATION_BAD_MAGIC;
  }
  if (in[4] != CALIBRATION_SCHEMA_VERSION) {
    return CALIBRATION_BAD_VERSION;
  }
  if (in[5] != NUM_ANTENNAS) {
    return CALIBRATION_AXIS_COUNT;
  }

  size_t crcAt = CALIBRATION_RECORD_SIZE - CALIBRATION_CRC_SIZE;
  uint32_t stored = 0;
  for (int i = 0; i < 4; i++) {
    stored |= (uint32_t)in[crcAt + i] << (8 * i);
  }
  if (stored != calibrationCrc32(in, crcAt)) {
    return CALIBRATION_BAD_CRC;
  }

  record.schemaVersion = in[4];
  record.axisCount = in[5];
  for (int i = 0; i < NUM_ANTENNAS; i++) {
    // Unknown flag bits are ignored rather than rejected, so that adding one in a
    // future version does not brick a board that has been downgraded. The bit
    // this firmware cares about still has to be there.
    record.axisZeroed[i] =
        (in[CALIBRATION_HEADER_SIZE + i] & CALIBRATION_AXIS_FLAG_ZEROED) != 0;
  }
  return CALIBRATION_OK;
}

// Reads the alignment facts out of the model.
inline void snapshotCalibration(const GondolaState& state,
                                CalibrationRecord& record) {
  record.schemaVersion = CALIBRATION_SCHEMA_VERSION;
  record.axisCount = NUM_ANTENNAS;
  for (int i = 0; i < NUM_ANTENNAS; i++) {
    record.axisZeroed[i] = state.antennas[i].centerZeroed;
  }
}

// Writes the alignment facts into the model.
//
// Only the flags. This does not set centerTick -- that is commitServoZero()'s
// job, and it is deliberately a different function: restoring a record must not
// be able to install a centre that no teach ever verified, or a corrupted record
// could reintroduce the exact defect the servo-side centre was meant to end.
inline void applyCalibration(GondolaState& state,
                             const CalibrationRecord& record) {
  for (int i = 0; i < NUM_ANTENNAS; i++) {
    if (i < record.axisCount) {
      state.antennas[i].centerZeroed = record.axisZeroed[i];
    }
  }
}

// A one-line description of a decode result, for the boot log. Returning a
// string rather than a number is what stops the caller having to keep a second
// switch in step with the enum.
inline const char* calibrationResultName(CalibrationResult result) {
  switch (result) {
    case CALIBRATION_OK: return "ok";
    case CALIBRATION_NO_RECORD: return "no record (never calibrated)";
    case CALIBRATION_BAD_MAGIC: return "bad magic (not our record)";
    case CALIBRATION_BAD_VERSION: return "unknown schema version";
    case CALIBRATION_AXIS_COUNT: return "axis count mismatch";
    case CALIBRATION_BAD_CRC: return "bad CRC (write was torn)";
  }
  return "unrecognised";
}

#pragma once

// The EEPROM half of calibration.h: where the record actually lives.
//
// Split from the format on purpose. calibration.h is pure arithmetic over bytes
// and firmware/tests/conftest.py compiles it on a host, so every rule about the
// record -- the layout, the CRC, what counts as valid -- is checked without a
// microcontroller. This file is the only thing in the sketch that knows the
// record exists in silicon, and the only thing that includes an Arduino header
// for the sake of storage.
//
// Why EEPROM and not a filesystem
// -------------------------------
// Because the arduino-pico core already reserves the last 4096 bytes of flash for
// its EEPROM emulation on the build this project uses, and the UF2 the flash
// script writes covers only the sketch region. So the record survives a reflash
// with no change to the FQBN -- which is the property that matters here, since a
// centre in the servo's own EEPROM is not something a firmware update can carry.
//
// It also means zeros now outlive a reflash. That is the point, and it has a
// cost: there has to be a way back to the defaults, and there is not one in this
// file. See the note on calibrationReset() below.
//
// The cost of a commit
// -------------------
// EEPROMClass::commit() does this, in EEPROM.cpp:
//
//     noInterrupts();
//     rp2040.idleOtherCore();
//     flash_range_erase(sector, 4096);
//     flash_range_program(sector, data, size);
//     rp2040.resumeOtherCore();
//     interrupts();
//
// Interrupts are off for the whole sector erase -- tens of milliseconds -- which
// stalls the USB CDC, the servo UART and the control loop. That is why commit
// must never be reachable from loop(), and why it only runs on the far side of a
// `zero`, which is already a blocking, deliberate, ground-only operation.
//
// And a commit is a whole-sector erase, so two copies of the record in the same
// 4096 bytes would not help at all: the erase takes the sector as a unit, so a
// power cut mid-erase loses both. One slot, one CRC, and calibration.h says why
// that is the honest arrangement rather than a shortfall.
//
// Wear
// ----
// One erase cycle per `zero`, and `zero` is an operator action that should not
// happen more than a handful of times per session. The identical-record check in
// calibrationSave() is not mainly about wear -- commit() already skips a clean
// sector -- it is so that a retried `zero` cannot burn a cycle writing back the
// same bytes, and so a no-op save costs nothing at all.

#include <Arduino.h>
#include <EEPROM.h>

#include "calibration.h"

// Bytes of emulated EEPROM this firmware reserves. The core rounds up to a 256
// byte boundary and the sector itself is 4096, so 256 leaves the record at 14
// bytes with room to grow for the mount offsets and the IMU datum without
// touching the build.
#define CALIBRATION_EEPROM_SIZE 256

// Offset of the record within the emulated sector.
#define CALIBRATION_EEPROM_OFFSET 0

// Mounts the emulated EEPROM. Idempotent: the core keeps its RAM copy if called
// again with the same size, so this is safe to call from setup() unconditionally
// and cheap if it is.
inline bool calibrationBegin() {
  EEPROM.begin(CALIBRATION_EEPROM_SIZE);
  return EEPROM.length() >= CALIBRATION_RECORD_SIZE;
}

// Reads the record.
//
// Returns CALIBRATION_OK with `record` filled, or the reason it could not be
// used with `record` untouched. Callers keep the built-in defaults on anything
// other than OK -- that is the documented policy for a missing or unreadable
// record, and the alternative (refusing to aim) is a bench decision this project
// has not made.
inline CalibrationResult calibrationLoad(CalibrationRecord& record) {
  uint8_t buf[CALIBRATION_RECORD_SIZE];
  const uint8_t* sector = EEPROM.getConstDataPtr();
  if (sector == nullptr) {
    return CALIBRATION_NO_RECORD;
  }
  memcpy(buf, sector + CALIBRATION_EEPROM_OFFSET, sizeof(buf));
  return decodeCalibration(buf, sizeof(buf), record);
}

// Writes the record, skipping the commit when it would not change anything.
//
// Returns false only if the record could not be encoded, which for a record of
// this shape means the caller's buffer was too small. A failed commit cannot be
// distinguished from a successful one through this API -- EEPROMClass::commit()
// returns true unconditionally once _size is set -- so a save that returns true
// means "the bytes were handed to the flash driver", not "the flash was
// verified". The read-back that actually proves persistence is calibrationLoad()
// on the next boot.
inline bool calibrationSave(const CalibrationRecord& record) {
  uint8_t buf[CALIBRATION_RECORD_SIZE];
  if (!encodeCalibration(record, buf, sizeof(buf))) {
    return false;
  }

  const uint8_t* sector = EEPROM.getConstDataPtr();
  if (sector != nullptr &&
      memcmp(sector + CALIBRATION_EEPROM_OFFSET, buf, sizeof(buf)) == 0) {
    return true;  // already stored; do not spend an erase cycle on it
  }

  for (size_t i = 0; i < sizeof(buf); i++) {
    EEPROM.write(CALIBRATION_EEPROM_OFFSET + (int)i, buf[i]);
  }
  EEPROM.commit();
  return true;
}

// Clears the record so the next boot starts from the built-in defaults.
//
// There is no command for this yet, and that is a known gap rather than an
// oversight: once a centre lives in servo EEPROM there is nothing in *this*
// firmware's storage that can un-teach it, so "reset calibration" is really two
// operations -- clear this record, and run a teach at the default centre on each
// axis. A future reset command has to do both or it will leave an axis reporting
// a taught centre with no record of it, which is worse than either state alone.
inline bool calibrationReset() {
  for (size_t i = 0; i < CALIBRATION_RECORD_SIZE; i++) {
    EEPROM.write(CALIBRATION_EEPROM_OFFSET + (int)i, 0xFF);
  }
  EEPROM.commit();
  return true;
}

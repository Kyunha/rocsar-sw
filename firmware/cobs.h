#ifndef COBS_H
#define COBS_H

#include <stdint.h>
#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

// Encodes data into a COBS formatted buffer.
// The output buffer must be allocated to at least length + length/254 + 1.
size_t cobs_encode(const void *data, size_t length, uint8_t *buffer);

// Decodes a COBS formatted buffer back into the original data.
size_t cobs_decode(const uint8_t *buffer, size_t length, void *data);

#ifdef __cplusplus
}
#endif

#endif // COBS_H
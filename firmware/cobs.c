#include "cobs.h"

size_t cobs_encode(const void *data, size_t length, uint8_t *buffer) {
    size_t read_index = 0;
    size_t write_index = 1;
    size_t code_index = 0;
    uint8_t code = 1;
    const uint8_t *ptr = (const uint8_t *)data;

    while (read_index < length) {
        if (ptr[read_index] == 0) {
            buffer[code_index] = code;
            code = 1;
            code_index = write_index++;
            read_index++;
        } else {
            buffer[write_index++] = ptr[read_index++];
            code++;
            if (code == 0xFF) {
                buffer[code_index] = code;
                code = 1;
                code_index = write_index++;
            }
        }
    }
    buffer[code_index] = code;
    return write_index;
}

size_t cobs_decode(const uint8_t *buffer, size_t length, void *data) {
    size_t read_index = 0;
    size_t write_index = 0;
    uint8_t code;
    uint8_t i;
    uint8_t *dst = (uint8_t *)data;

    if (length == 0) return 0;

    while (read_index < length) {
        code = buffer[read_index];
        if (read_index + code > length && code != 1) return 0;
        read_index++;

        for (i = 1; i < code; i++) {
            dst[write_index++] = buffer[read_index++];
        }
        if (code != 0xFF && read_index != length) {
            dst[write_index++] = '\0';
        }
    }
    return write_index;
}
"""The wire format: the same nanopb encoder the RP2040 runs, on the host.

`firmware/tests/firmware_encoder_probe.cpp` builds COBS-framed PicoMessage
frames byte-for-byte the way `firmware.ino` emits them. Comparing those bytes
against a decoder on this side is what makes "the firmware and the host agree"
a tested claim rather than an assumption -- and the agreement has to be exact,
because a field that shifts by one byte desynchronises COBS and every frame
after it.

The cross-language tests that lived here before are gone, not ported: they drove
the old Python `PicoLink` and `controller.hardware.serial_pico`, neither of
which exists in this repository. The equivalent pairing is Go `PicoLink` against
this same probe, which would test the link we actually ship rather than the one
the tests were written against. That is a separate piece of work and is not
claimed here.
"""

from __future__ import annotations

from conftest import FIRMWARE_DIR, run

SKETCH = FIRMWARE_DIR / "firmware.ino"
WIRE_HEADER = FIRMWARE_DIR / "pico_wire.h"


def _frames(probe) -> list[bytes]:
    return [bytes.fromhex(line) for line in run(probe).splitlines()]


class TestFraming:
    def test_the_probe_emits_a_frame_per_payload(self, probe):
        telemetry_frame, success_frame, error_frame = _frames(probe)
        assert telemetry_frame and success_frame and error_frame

    def test_every_frame_is_delimited_and_starts_after_cobs(self, probe):
        for frame in _frames(probe):
            assert frame[-1] == 0x00, "COBS frames must end in the 0x00 delimiter"
            # COBS guarantees the encoded body never contains a zero byte, which
            # is the entire reason the scheme is used: the delimiter cannot occur
            # inside a frame and desynchronise the reader.
            assert 0x00 not in frame[:-1], "a zero byte inside a COBS frame"

    def test_the_frame_fits_the_transmit_buffer(self, probe):
        # pico_wire.h sizes the COBS buffer from PICO_TX_BUFFER using the
        # documented worst case. encodePicoFrame() returns 0 rather than
        # overflowing and the sketch then drops the frame, so assert the real
        # frames stay inside.
        for frame in _frames(probe):
            assert 0 < len(frame) < 256, "frame must fit PICO_TX_BUFFER"

    def test_a_response_frame_differs_from_a_telemetry_frame(self, probe):
        telemetry_frame, success_frame, error_frame = _frames(probe)
        # The response frames are the ones the OBC's request/response
        # correlation depends on; if success and error encoded identically the
        # OBC could not tell an acknowledgement from a refusal.
        assert success_frame != error_frame


class TestEncoderBounds:
    def test_a_frame_the_encoder_cannot_fit_is_rejected_not_overflowed(self):
        """PICO_TX_BUFFER is the contract; a caller must never exceed it silently."""
        header = WIRE_HEADER.read_text()
        assert "if (framedLen + 1 > outLen)" in header, "encoder must bounds-check its output"
        assert "COBS_MAX(PICO_TX_BUFFER)" in header


class TestReceiveBuffer:
    def test_the_sketch_does_not_overflow_its_receive_buffer(self):
        """Overflow must reset rather than write past the end of rxBuffer."""
        sketch = SKETCH.read_text()
        assert "if (rxLen < sizeof(rxBuffer))" in sketch
        assert "uint8_t decBuf[256]" in sketch, (
            "the decode buffer must match the COBS worst case for the receive buffer"
        )

    def test_the_sketch_receive_buffer_holds_a_whole_frame(self):
        """rxBuffer must be able to hold the largest frame the host can send.

        The sketch drops an overflowing frame silently, so a buffer one byte too
        small shows up on the OBC as a control command that simply never answers.
        256 is COBS_MAX(256), the same bound pico_wire.h encodes against.
        """
        sketch = SKETCH.read_text()
        assert "uint8_t rxBuffer[256]" in sketch
        assert "uint8_t decBuf[256]" in sketch


# There is deliberately no round-trip test here. The probe can encode a
# telemetry frame and decode a command frame, but it has no mode that emits a
# command frame, so there is nothing to feed its own decoder without a host
# encoder supplying one. That is not a gap in the port: the old suite got its
# command frames from the Python PicoLink, which is why the five cross-language
# tests it held could not come across. The replacement pairs the Go encoder
# with this decoder, which tests the link we actually ship.
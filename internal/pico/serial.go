package pico

import (
	"fmt"
	"os"
	"time"

	goserial "go.bug.st/serial"
)

// SerialTransport is a USB CDC serial port.
//
// go.bug.st/serial is a pure-Go implementation, so the OBC cross-compiles for
// the Pi with cgo disabled and there is no libftdi to install on the aircraft.
// The library is imported HERE and nowhere else: a bare `go build` of any other
// package does not need it, and the link's framing and correlation logic can be
// tested against a buffer instead.
type SerialTransport struct {
	port     goserial.Port
	path     string
	baudrate int
}

// DefaultBaudrate is the USB link rate to the Pi.
//
// It is NOT the ST3215 servo bus rate, which is also 115200 and is a completely
// unrelated wire. Two buses, one coincidental number, and they are separate
// constants in separate packages. A comment at each site says so because they
// are easy to confuse precisely because they match.
const DefaultBaudrate = 115200

// NewSerialTransport returns an unopened port. The port is opened by OpenPort so
// the composition root decides when the device is touched.
func NewSerialTransport(path string, baudrate int) *SerialTransport {
	if baudrate <= 0 {
		baudrate = DefaultBaudrate
	}
	return &SerialTransport{path: path, baudrate: baudrate}
}

// OpenPort opens the port.
//
// A missing device is returned as an error rather than being retried forever.
// Retrying here is what produced the previous system's behaviour, where the OBC
// logged "waiting for device" every two seconds indefinitely and telemetry said
// nothing about it.
func (s *SerialTransport) OpenPort() error {
	mode := &goserial.Mode{BaudRate: s.baudrate, DataBits: 8, Parity: goserial.NoParity, StopBits: goserial.OneStopBit}

	p, err := goserial.Open(s.path, mode)
	if err != nil {
		return fmt.Errorf("open %s at %d baud: %w", s.path, s.baudrate, err)
	}
	s.port = p
	return nil
}

func (s *SerialTransport) requireOpen(op string) error {
	if s.port == nil {
		return fmt.Errorf("cannot %s: serial transport is not open", op)
	}
	return nil
}

func (s *SerialTransport) Read(p []byte) (int, error) {
	if err := s.requireOpen("read"); err != nil {
		return 0, err
	}
	return s.port.Read(p)
}

func (s *SerialTransport) Write(p []byte) (int, error) {
	if err := s.requireOpen("write"); err != nil {
		return 0, err
	}
	// The port is opened with a blocking write timeout, so a short write is
	// possible but not likely. It is not retried here: the frame is already
	// partly on the wire and the receiver resynchronises at the next delimiter,
	// so a blind retry would splice two frames together.
	return s.port.Write(p)
}

// SetReadTimeout bounds one read. The receive loop needs it so a silent device
// cannot block the goroutine forever.
//
// go.bug.st/serial reports an expired read as (0, nil) rather than an error.
// That convention is preserved by the Link: treating it as an error would make
// the link flap once per timeout on a healthy but idle device.
func (s *SerialTransport) SetReadTimeout(d time.Duration) error {
	if err := s.requireOpen("set a read timeout"); err != nil {
		return err
	}
	return s.port.SetReadTimeout(d)
}

// DrainRead discards buffered input.
//
// Called before each command so a late reply to a command that already timed out
// cannot be delivered as the acknowledgement to the next one. Without it, a
// command that timed out because the device was briefly busy is followed by a
// command that succeeds instantly with the PREVIOUS command's ACK, and the
// console shows the wrong servo moving -- which on a loaded axis is a
// maintenance-crew safety problem, not a cosmetic one.
func (s *SerialTransport) DrainRead() error {
	if err := s.requireOpen("drain"); err != nil {
		return err
	}
	return s.port.ResetInputBuffer()
}

func (s *SerialTransport) Close() error {
	if s.port == nil {
		return nil
	}
	err := s.port.Close()
	s.port = nil
	return err
}

func (s *SerialTransport) Path() string { return s.path }

// Exists reports whether the device node is present.
//
// A cheap pre-check so the composition root can say "no Pico at /dev/ttyACM0"
// rather than opening a port and reporting the syscall error, which on Linux
// says "no such file or directory" without saying which file.
func (s *SerialTransport) Exists() bool {
	_, err := os.Stat(s.path)
	return err == nil
}

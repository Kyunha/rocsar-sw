// Package camera captures stills from a USB camera.
//
// Snapshot only. There is no live stream: it consumed bandwidth that constraint
// H1 does not have, in exchange for a picture nobody needed continuously. One
// photograph on command, written to the SSD, fetched over HTTP.
package camera

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/rocsar/obc/internal/domain"
	"github.com/rocsar/obc/internal/storage"
)

// v4l2 ioctl request codes.
//
// Encoded the way the kernel expects: direction, type ('V' for video), number
// and size. Written out rather than pulled from a package because there is no
// maintained Go V4L2 binding and this is the alternative to shelling out to
// ffmpeg for every photograph.
const (
	iocNrBits   = 8
	iocTypeBits = 8
	iocSizeBits = 14

	iocNrShift   = 0
	iocTypeShift = iocNrShift + iocNrBits
	iocSizeShift = iocTypeShift + iocTypeBits
	iocDirShift  = iocSizeShift + iocSizeBits

	iocWrite = 1
	iocRead  = 2
)

func ioc(dir, typ, nr, size uintptr) uintptr {
	return dir<<iocDirShift | typ<<iocTypeShift | nr<<iocNrShift | size<<iocSizeShift
}

// VIDIOC request codes, from linux/videodev2.h.
//
// Declared as `var` rather than `const` because unsafe.Sizeof is not a constant
// expression in Go. That is also the more honest declaration: the encoded value
// depends on the size of a struct, which is architecture-specific, so these are
// per-target values and not compile-time constants.
var (
	vidiocQueryCap  = ioc(iocRead|iocWrite, 'V', 0, unsafe.Sizeof(v4l2Capability{}))
	vidiocG_Fmt     = ioc(iocRead|iocWrite, 'V', 4, unsafe.Sizeof(v4l2Format{}))
	vidiocS_Fmt     = ioc(iocRead|iocWrite, 'V', 5, unsafe.Sizeof(v4l2Format{}))
	vidiocReqBufs   = ioc(iocRead|iocWrite, 'V', 8, unsafe.Sizeof(v4l2ReqBufs{}))
	vidiocQBUF      = ioc(iocRead|iocWrite, 'V', 11, unsafe.Sizeof(v4l2Buffer{}))
	vidiocDQBUF     = ioc(iocRead|iocWrite, 'V', 13, unsafe.Sizeof(v4l2Buffer{}))
	vidiocStreamOn  = ioc(iocRead|iocWrite, 'V', 18, unsafe.Sizeof(v4l2BufType{}))
	vidiocStreamOff = ioc(iocRead|iocWrite, 'V', 19, unsafe.Sizeof(v4l2BufType{}))
)

// V4L2 pixel formats. Only the ones a USB camera in MJPEG or YUYV mode produces.
const (
	pixelFormatMJPEG = 0x47504a4d // 'MJPG'
	pixelFormatYUYV  = 0x56595559 // 'YUYV'
)

type v4l2Capability struct {
	Driver       [16]uint8
	Card         [32]uint8
	BusInfo      [32]uint8
	Version      uint32
	Capabilities uint32
	DeviceCaps   uint32
	Reserved     [3]uint32
}

type v4l2Format struct {
	PixelFormat  uint32
	Width        uint32
	Height       uint32
	BytesPerLine uint32
	SizeImage    uint32
	Field        uint32
	Colorspace   uint32
	Private      uint32
	BytesUsed    [2]uint32
}

type v4l2ReqBufs struct {
	Count    uint32
	Memory   uint32
	Reserved [2]uint32
}

// v4l2Buffer is struct v4l2_buffer.
//
// The first eight fields are the common header the kernel fills in for every
// buffer. The four after them are the mmap-specific arm of a union, flattened
// here: on 64-bit that union starts at offset 32, and expressing it as a nested
// anonymous struct would need explicit padding that the compiler cannot check
// and that silently corrupts every captured frame when it is wrong.
//
// The field names carry a unit or a role where the C name is a bare type name,
// because `Length` here means two different things depending on which arm of
// the union is live and that ambiguity is the entire bug surface of this
// struct.
type v4l2Buffer struct {
	// Common header.
	BytesUsed  uint32
	BytesLeft  uint32
	Sequence   uint32
	Memory     uint32
	MbufOffset uint32
	Length     uint32
	Field      uint32
	Reserved   [3]uint32

	// mmap union arm.
	MmapStart   uintptr
	MmapLength  uint32
	MmapPadding uint32
}

type v4l2BufType struct {
	Type      uint32
	Memory    uint32
	SizeImage uint32
	Padding   [13]uint32
}

// Capture is a V4L2 snapshot camera.
//
// One-shot: open, queue buffers, stream, grab one frame, close. Keeping the
// device open between shots would be faster, and a USB camera left open is a
// power drain and a device that other programs cannot use. One photograph every
// few seconds does not need the optimisation.
type Capture struct {
	device string
	store  *storage.Store
	dir    string

	quality int

	mu     sync.Mutex
	state  domain.SubsystemState
	shots  uint64
	last   string
	opened bool
}

var _ domain.Camera = (*Capture)(nil)

// NewCapture returns a camera writing into the given subdirectory of the store.
func NewCapture(device, dir string, store *storage.Store, quality int) *Capture {
	if quality <= 0 || quality > 100 {
		quality = 85
	}
	return &Capture{
		device:  device,
		store:   store,
		dir:     dir,
		quality: quality,
		state:   domain.SubsystemDisconnected,
	}
}

func (c *Capture) Device() string { return c.device }

func (c *Capture) State() domain.SubsystemState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *Capture) PhotosTaken() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.shots
}

// Capture grabs one JPEG and writes it to the data directory atomically.
func (c *Capture) Capture(ctx context.Context) (*domain.Photo, error) {
	c.mu.Lock()
	if c.state == domain.SubsystemBusy {
		c.mu.Unlock()
		return nil, fmt.Errorf("camera: a capture is already in progress")
	}
	c.state = domain.SubsystemBusy
	c.mu.Unlock()

	photo, err := c.captureOnce(ctx)

	c.mu.Lock()
	if err != nil {
		c.state = domain.SubsystemDisconnected
	} else {
		c.state = domain.SubsystemReady
		c.shots++
		c.last = photo.Name
	}
	c.mu.Unlock()

	return photo, err
}

func (c *Capture) captureOnce(ctx context.Context) (*domain.Photo, error) {
	if _, err := os.Stat(c.device); err != nil {
		return nil, fmt.Errorf("camera: %s is not present", c.device)
	}

	fd, err := syscall.Open(c.device, syscall.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("camera: open %s: %w", c.device, err)
	}
	defer syscall.Close(fd)

	pixelFormat, err := c.setup(fd)
	if err != nil {
		return nil, err
	}

	buf, err := c.readFrame(fd)
	if err != nil {
		return nil, err
	}

	if pixelFormat == pixelFormatMJPEG {
		// The device already gave us a JPEG. It is not re-encoded: doing so
		// would lose quality and burn CPU to produce a worse copy of a file we
		// did not have to touch.
	} else {
		buf, err = c.toJPEG(buf, pixelFormat)
		if err != nil {
			return nil, err
		}
	}

	name := storage.NameFor(storage.KindCamera, "jpg")
	sub, err := c.store.Sub(c.dir)
	if err != nil {
		return nil, err
	}
	if err := sub.WriteFileAtomic(name, buf, 0o644); err != nil {
		return nil, err
	}

	return &domain.Photo{
		Name:      name,
		SizeBytes: uint64(len(buf)),
		Kind:      storage.KindCamera,
		Path:      c.dir + "/" + name,
	}, nil
}

// setup negotiates the format and queues buffers, returning the pixel format.
func (c *Capture) setup(fd int) (uint32, error) {
	var cap v4l2Capability
	if err := ioctl(fd, vidiocQueryCap, unsafe.Pointer(&cap)); err != nil {
		return 0, fmt.Errorf("camera: VIDIOC_QUERYCAP: %w", err)
	}
	if cap.Capabilities&0x00000001 == 0 { // V4L2_CAP_VIDEO_CAPTURE
		return 0, fmt.Errorf("camera: %s is not a capture device", c.device)
	}

	// Prefer MJPEG: the camera compresses, so a full-resolution frame crosses
	// the bus as a few tens of kilobytes instead of several megabytes. That is
	// the difference between a photograph and a timeout on a USB 2 camera.
	// Named `f`, not `fmt`: a local called fmt shadows the fmt package for the
	// whole function, and every error message below stops compiling.
	var f v4l2Format
	f.PixelFormat = pixelFormatMJPEG
	if err := ioctl(fd, vidiocG_Fmt, unsafe.Pointer(&f)); err != nil {
		return 0, fmt.Errorf("camera: VIDIOC_G_FMT: %w", err)
	}

	// Leave the resolution at whatever the driver chose by default rather than
	// forcing one. Hard-coding a resolution is a class of bug where the camera
	// is fine and the code is wrong; asking is free.
	if err := ioctl(fd, vidiocS_Fmt, unsafe.Pointer(&f)); err != nil {
		return 0, fmt.Errorf("camera: VIDIOC_S_FMT: %w", err)
	}
	if f.PixelFormat != pixelFormatMJPEG && f.PixelFormat != pixelFormatYUYV {
		return 0, fmt.Errorf("camera: unsupported pixel format 0x%08x (MJPEG=0x%08x, YUYV=0x%08x)",
			f.PixelFormat, pixelFormatMJPEG, pixelFormatYUYV)
	}

	var req v4l2ReqBufs
	req.Count = 4
	req.Memory = 1 // V4L2_MEMORY_MMAP
	if err := ioctl(fd, vidiocReqBufs, unsafe.Pointer(&req)); err != nil {
		return 0, fmt.Errorf("camera: VIDIOC_REQBUFS: %w", err)
	}
	if req.Count < 2 {
		// The driver must keep at least one buffer queued while one is being
		// dequeued, or the stream stalls before the first frame.
		return 0, fmt.Errorf("camera: driver granted only %d of 4 buffers", req.Count)
	}
	return f.PixelFormat, nil
}

// readFrame dequeues one frame, copies it out and re-queues the buffer.
func (c *Capture) readFrame(fd int) ([]byte, error) {
	var typ v4l2BufType
	typ.Type = 1 // V4L2_BUF_TYPE_VIDEO_CAPTURE
	typ.Memory = 1
	if err := ioctl(fd, vidiocStreamOn, unsafe.Pointer(&typ)); err != nil {
		return nil, fmt.Errorf("camera: VIDIOC_STREAMON: %w", err)
	}
	defer func() {
		_ = ioctl(fd, vidiocStreamOff, unsafe.Pointer(&typ))
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var b v4l2Buffer
		b.Memory = 1
		if err := ioctl(fd, vidiocDQBUF, unsafe.Pointer(&b)); err != nil {
			if err == syscall.EAGAIN {
				time.Sleep(5 * time.Millisecond)
				continue
			}
			return nil, fmt.Errorf("camera: VIDIOC_DQBUF: %w", err)
		}

		data, err := c.copyBuffer(fd, b)
		if err != nil {
			return nil, err
		}

		// Re-queue before returning, always. A buffer that is not re-queued is
		// not lost, it is leaked from a pool of four, and the third leaked
		// capture fails.
		if err := ioctl(fd, vidiocQBUF, unsafe.Pointer(&b)); err != nil {
			return nil, fmt.Errorf("camera: VIDIOC_QBUF: %w", err)
		}

		if len(data) > 0 {
			return data, nil
		}
	}
	return nil, fmt.Errorf("camera: no frame within 5s")
}

func (c *Capture) copyBuffer(fd int, b v4l2Buffer) ([]byte, error) {
	if b.BytesUsed == 0 {
		return nil, nil
	}

	// Prefer the mmap path when the driver supports it; fall back to a plain
	// read for drivers that do not.
	if b.Memory == 1 && b.MmapLength > 0 {
		m, err := syscall.Mmap(fd, 0, int(b.MmapLength), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err == nil {
			defer func() { _ = syscall.Munmap(m) }()
			used := int(b.BytesUsed)
			if used > len(m) {
				used = len(m)
			}
			out := make([]byte, used)
			copy(out, m[:used])
			return out, nil
		}
	}

	out := make([]byte, b.BytesUsed)
	if _, err := syscall.Pread(fd, out, int64(b.MbufOffset)); err != nil {
		return nil, fmt.Errorf("camera: read frame: %w", err)
	}
	return out, nil
}

// toJPEG converts a raw frame to JPEG.
func (c *Capture) toJPEG(raw []byte, pixelFormat uint32) ([]byte, error) {
	switch pixelFormat {
	case pixelFormatYUYV:
		return yuyvToJPEG(raw, c.quality)
	default:
		return nil, fmt.Errorf("camera: cannot encode pixel format 0x%08x", pixelFormat)
	}
}

// yuyvToJPEG converts packed YUYV to a JPEG.
//
// YUYV is two pixels per four bytes: Y0 U Y1 V. It is the near-universal format
// for a camera that does not compress, and the conversion is the reason a
// snapshot is possible without a codec dependency.
func yuyvToJPEG(raw []byte, quality int) ([]byte, error) {
	if len(raw) < 4 {
		return nil, fmt.Errorf("camera: YUYV frame is %d bytes, too short for a pixel", len(raw))
	}

	pairs := len(raw) / 4
	img := image.NewYCbCr(image.Rect(0, 0, pairs*2, pairs), image.YCbCrSubsampleRatio420)

	// YCbCr has no PixOffset method, so the planar addressing is computed here.
	// With a 4:2:0 subsample ratio, Cb and Cr are pairs*bytesPerRow wide and
	// the offset of row y is y*bytesPerRow, while Y is full width.
	yStride := img.YStride
	cStride := img.CStride

	i := 0
	for y := 0; y < pairs; y++ {
		y0 := raw[i]
		u := raw[i+1]
		y1 := raw[i+2]
		v := raw[i+3]
		i += 4

		row := y * yStride
		crow := y * cStride
		img.Y[row] = y0
		img.Y[row+1] = y1
		img.Cb[crow] = u
		img.Cr[crow] = v
	}

	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, fmt.Errorf("camera: jpeg encode: %w", err)
	}
	return out.Bytes(), nil
}

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}

// Close satisfies domain.Shutdown. A one-shot camera holds nothing between
// captures, so this only clears the reported state.
func (c *Capture) Close() error { return nil }

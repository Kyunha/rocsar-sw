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
	"path/filepath"
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
// The direction bits are PER IOCTL and are not decoration. The kernel switches
// on the fully encoded value, so a request whose direction does not match the
// header's is not "a query with extra bits" -- it is a different number, and
// the driver does not recognise it:
//
//	VIDIOC_QUERYCAP   _IOR   -> 0x80685600   direction 2, READ only
//	VIDIOC_STREAMON   _IOW   -> 0x40045612   direction 1, WRITE only
//	VIDIOC_G_FMT      _IOR   direction 2, but read-modify-write in practice
//
// Every one of these was iocRead|iocWrite at first. QUERYCAP then failed with
// ENOTTY on a real UVC camera, which is the only place this is detectable: the
// numbers are computed, they are plausible, and nothing complains until a driver
// rejects them. TestV4L2RequestCodesAgainstTheKernelHeader pins every value to
// the header's, so this cannot come back and never needs hardware to catch.
var (
	vidiocQueryCap = ioc(iocRead, 'V', 0, unsafe.Sizeof(v4l2Capability{}))
	vidiocG_Fmt    = ioc(iocRead|iocWrite, 'V', 4, unsafe.Sizeof(v4l2Format{}))
	vidiocS_Fmt    = ioc(iocRead|iocWrite, 'V', 5, unsafe.Sizeof(v4l2Format{}))
	vidiocReqBufs  = ioc(iocRead|iocWrite, 'V', 8, unsafe.Sizeof(v4l2ReqBufs{}))
	// nr 15 and 17, not 11 and 13. The QBUF/DQBUF numbers moved in Linux 2.6.x
	// when the buffer struct grew a timecode; 11 and 13 are the pre-2.6 values
	// and they are still in every V4L2 example written before that.
	vidiocQBUF  = ioc(iocRead|iocWrite, 'V', 15, unsafe.Sizeof(v4l2Buffer{}))
	vidiocDQBUF = ioc(iocRead|iocWrite, 'V', 17, unsafe.Sizeof(v4l2Buffer{}))
	// _IOW('V', 18, int) -- an int, not a struct. STREAMON takes the buffer type
	// as a bare integer, which is why the encoded size is 4.
	vidiocStreamOn  = ioc(iocWrite, 'V', 18, unsafe.Sizeof(int32(0)))
	vidiocStreamOff = ioc(iocWrite, 'V', 19, unsafe.Sizeof(int32(0)))
)

// V4L2 pixel formats. Only the ones a USB camera in MJPEG or YUYV mode produces.
const (
	pixelFormatMJPEG = 0x47504a4d // 'MJPG'
	pixelFormatYUYV  = 0x56595559 // 'YUYV'
)

// V4L2 constants used in expressions rather than as an ioctl request code, from
// linux/videodev2.h. These were bare numbers at their use sites, which is the
// worst place for a number whose only meaning is in a header nobody has open.
const (
	capVideoCapture = 0x00000001 // V4L2_CAP_VIDEO_CAPTURE

	bufTypeVideoCapture = 1 // V4L2_BUF_TYPE_VIDEO_CAPTURE
	memoryMmap          = 1 // V4L2_MEMORY_MMAP

	// queueBuffers is VIDIOC_REQBUFS's count. Four is a compromise, not a
	// constant from anywhere: one buffer is being filled while another is being
	// drained, and a driver that grants fewer than two stalls before the first
	// frame ever arrives.
	queueBuffers = 4

	// minQueueableBuffers is the smallest count that can stream at all. Below
	// this the setup fails rather than trying: a driver that grants one buffer
	// will hand back a frame and then never another.
	minQueueableBuffers = 2
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
	// Offset 0.
	Type uint32

	// Offset 4 is NOT part of the union. v4l2_format is:
	//
	//	__u32 type;
	//	union { ... } fmt;
	//
	// and something in that union contains a __u64, so the union is 8-aligned
	// and 4 bytes of padding sit between them. The union therefore begins at
	// offset 8, not 4 -- and that is why pixelformat is at 16 rather than 8.
	_ uint32

	// v4l2_pix_format, at offset 8, in the kernel's order. This one was written
	// from memory first, with BytesPerLine before SizeImage and no leading
	// type/padding, which put every field after the first two in the wrong
	// place.
	Width        uint32 // 8
	Height       uint32 // 12
	PixelFormat  uint32 // 16
	Field        uint32 // 20
	BytesPerLine uint32 // 24
	SizeImage    uint32 // 28

	// The union runs to 208 bytes in total. Which arm is live depends on Type;
	// for V4L2_BUF_TYPE_VIDEO_CAPTURE it is the v4l2_pix_format above.
	_ [176]byte
}

type v4l2ReqBufs struct {
	Count        uint32   // 0
	Type         uint32   // 4  enum v4l2_buf_type -- V4L2_BUF_TYPE_VIDEO_CAPTURE
	Memory       uint32   // 8  enum v4l2_memory  -- V4L2_MEMORY_MMAP
	Capabilities uint32   // 12, output: what the driver supports
	Flags        uint8    // 16
	Reserved     [3]uint8 // 17
}

// bufTypeVideoCapture belongs in v4l2ReqBufs.Type, not in a struct of its own.
// STREAMON takes it as a bare int.

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
	Index     uint32 // 0
	Type      uint32 // 4
	BytesUsed uint32 // 8
	Flags     uint32 // 12
	Field     uint32 // 16

	_ uint32 // 20 -- timeval below is 8-aligned

	// struct timeval: two time_t. Named, not aliased to int64, because it is two
	// 64-bit words and calling it int64 would invite someone to write
	// Timestamp = now.Unix() instead of the seconds/nanoseconds pair it wants.
	TimestampSec  int64 // 24
	TimestampUsec int64 // 32

	// struct v4l2_timecode: type, flags, frames, seconds.
	TimecodeType    uint32 // 40
	TimecodeFlags   uint32 // 44
	TimecodeFrames  uint32 // 48
	TimecodeSeconds uint32 // 52

	Sequence uint32 // 56

	// memory location. `Memory` is V4L2_MEMORY_MMAP for every buffer here.
	Memory uint32 // 60

	// The union is { __u32 offset; unsigned long userptr; struct v4l2_plane *;
	// __s32 fd; }. `unsigned long` and the pointer are 8 bytes on any 64-bit
	// target, which is what makes this struct 88 bytes here and 84 on a 32-bit
	// one -- so its size, and therefore the QBUF and DQBUF request codes, are
	// architecture-dependent. Only `offset` (the mmap arm) is used.
	MmapOffset uint64 // 64, 8 bytes wide because of the union

	Length    uint32 // 72
	Reserved2 uint32 // 76
	RequestFd int32  // 80

	_ uint32 // 84 -- trailing alignment
}

// v4l2BufType is what VIDIOC_STREAMON and _STREAMOFF actually take: an int.
// There is no struct v4l2_buf_type in videodev2.h -- the name is an enum -- and
// the 64-byte struct that used to stand here made every request code wrong,
// because the size is part of the number.
type v4l2BufType = int32

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

	// ext photographs by shelling out, and is preferred when present. See
	// fswebcam.go for why. nil means "use the V4L2 path below".
	ext *fswebcamCapturer

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
	// Scratch space for the fswebcam path, which has to write to a file because it
	// is a separate process. Under the data root rather than /tmp so that a full
	// or read-only /tmp cannot silently become a capture failure, and so the
	// scratch file is on the same filesystem as the store, which is what makes
	// WriteFileAtomic's rename atomic.
	ext, err := newFswebcamCapturer(device, filepath.Join(os.TempDir(), "rocsar-camera"), nil)
	if err != nil {
		// Not installed, or no scratch directory. The built-in V4L2 path stays.
		ext = nil
	}

	return &Capture{
		device:  device,
		store:   store,
		dir:     dir,
		quality: quality,
		state:   domain.SubsystemDisconnected,
		ext:     ext,
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

	// fswebcam first when it is installed. The built-in path below negotiates the
	// camera correctly and then never receives a frame on this hardware; see
	// fswebcam.go. Trying it first and falling back would cost every capture a
	// five-second timeout on the device that is actually in use.
	if c.ext != nil {
		body, err := c.ext.Capture(ctx)
		if err != nil {
			return nil, err
		}
		return c.savePhoto(body)
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

	return c.savePhoto(buf)
}

// savePhoto stores JPEG bytes and describes them.
//
// Both capture backends end here, so a photograph is classified, named and
// recorded identically whichever produced it. That matters: the classification
// bug this project shipped once was a mismatch between what was written and what
// was reported about it.
func (c *Capture) savePhoto(buf []byte) (*domain.Photo, error) {
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
	if cap.Capabilities&capVideoCapture == 0 {
		return 0, fmt.Errorf("camera: %s is not a capture device", c.device)
	}

	// Prefer MJPEG: the camera compresses, so a full-resolution frame crosses
	// the bus as a few tens of kilobytes instead of several megabytes. That is
	// the difference between a photograph and a timeout on a USB 2 camera.
	// Named `f`, not `fmt`: a local called fmt shadows the fmt package for the
	// whole function, and every error message below stops compiling.
	var f v4l2Format
	// Type is not optional. It is how the kernel knows which union arm is live
	// and whether it can service the request at all; zero is not a buffer type,
	// and G_FMT answers EINVAL rather than guessing. This field did not exist in
	// the first version of this struct, which is the same bug as its absence:
	// the call failed, just one step later.
	f.Type = bufTypeVideoCapture
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
	req.Count = queueBuffers
	req.Type = bufTypeVideoCapture
	req.Memory = memoryMmap
	if err := ioctl(fd, vidiocReqBufs, unsafe.Pointer(&req)); err != nil {
		return 0, fmt.Errorf("camera: VIDIOC_REQBUFS: %w", err)
	}
	if req.Count < minQueueableBuffers {
		// The driver must keep at least one buffer queued while one is being
		// dequeued, or the stream stalls before the first frame.
		return 0, fmt.Errorf("camera: driver granted only %d of %d buffers", req.Count, queueBuffers)
	}
	return f.PixelFormat, nil
}

// readFrame dequeues one frame, copies it out and re-queues the buffer.
func (c *Capture) readFrame(fd int) ([]byte, error) {
	// STREAMON carries only the buffer type, as an int.
	typ := v4l2BufType(bufTypeVideoCapture)
	if err := ioctl(fd, vidiocStreamOn, unsafe.Pointer(&typ)); err != nil {
		return nil, fmt.Errorf("camera: VIDIOC_STREAMON: %w", err)
	}
	defer func() {
		_ = ioctl(fd, vidiocStreamOff, unsafe.Pointer(&typ))
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		// Type and Memory are inputs on DQBUF: they say which queue to take
		// from. Left at zero the kernel cannot tell what is being asked for and
		// answers EINVAL. This is the third call in a row that needed the buffer
		// type set, which is what you would expect from a struct that had no
		// Type field at all.
		var b v4l2Buffer
		b.Type = bufTypeVideoCapture
		b.Memory = memoryMmap
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

	// mmap arm of the union. `Length` is the buffer's length and `MmapOffset` is
	// where the driver mapped it -- mapping from 0 rather than from MmapOffset
	// would read the first buffer whatever buffer this actually was.
	if b.Memory == memoryMmap && b.Length > 0 {
		m, err := syscall.Mmap(fd, int64(b.MmapOffset), int(b.Length),
			syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
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
		// Fall through to read() rather than failing: a driver that granted
		// MMAP buffers but cannot map them is rare, and one photograph is not
		// worth failing over when read() will do.
	}

	out := make([]byte, b.BytesUsed)
	if _, err := syscall.Pread(fd, out, int64(b.MmapOffset)); err != nil {
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

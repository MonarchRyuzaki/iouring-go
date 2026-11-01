package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"runtime"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
	"uring-go/common"
)

// ============================================================================
// 1. LINUX IO_URING CONSTANTS & SYS CALL CODES
// ============================================================================
// On x86_64 Linux:
// - SYS_IO_URING_SETUP (425): Initialises a new submission & completion queue ring.
// - SYS_IO_URING_ENTER (426): Submits queued I/O requests and waits for completions.
// - SYS_IO_URING_REGISTER (427): Registers user buffers or file descriptors with the kernel.
const (
	SYS_IO_URING_SETUP = 425
	SYS_IO_URING_ENTER = 426

	// IORING_ENTER_GETEVENTS: Tells io_uring_enter to block until 'min_complete'
	// CQEs have completed. If 0 events are completed, the calling thread sleeps.
	IORING_ENTER_GETEVENTS = 1 << 0

	// Feature flags returned by the kernel in io_uring_params.Features:
	// IORING_FEAT_SINGLE_MMAP (Linux 5.4+): Kernel maps both SQ and CQ rings in
	// a single contiguous memory region, eliminating redundant mmap calls.
	IORING_FEAT_SINGLE_MMAP = 1 << 0

	// Magic offsets used with mmap(2) to map ring structures:
	IORING_OFF_SQ_RING = 0
	IORING_OFF_CQ_RING = 0x8000000
	IORING_OFF_SQES    = 0x10000000

	// Opcodes describing the asynchronous operations submitted to the kernel:
	// IORING_OP_ACCEPT (13): Asynchronously accept incoming TCP connections.
	// IORING_OP_SEND   (26): Asynchronously transmit data over a connected socket.
	// IORING_OP_RECV   (27): Asynchronously receive incoming data from a connected socket.
	// IORING_OP_CLOSE  (19): Asynchronously close a file descriptor.
	IORING_OP_ACCEPT = 13
	IORING_OP_SEND   = 26
	IORING_OP_RECV   = 27
	IORING_OP_CLOSE  = 19

	// Internal operation tags encoded into SQE.UserData:
	// We pack the operation type and socket FD into a single 64-bit uint:
	//   UserData = (OpTag << 32) | uint64(fd)
	// When the kernel finishes the operation, it echoes this exact UserData back
	// inside the CQE, allowing zero-lookup event demultiplexing.
	TagAccept uint64 = 1
	TagRecv   uint64 = 2
	TagSend   uint64 = 3
	TagClose  uint64 = 4

	// Maximum concurrent connection capacity pre-allocated in our buffer pool.
	// 16384 connections * 1024 bytes buffer = ~16 MB RAM (well below 128MB limit).
	MaxConnections = 16384
	BufferSize     = 1024

	// Queue depth: 2048 submission queue entries gives an ultra-deep queue capable
	// of handling massive wrk concurrency spikes without dropping submissions.
	RingEntries = 2048
)

// ============================================================================
// 2. KERNEL UAPI DATA STRUCTURES (EXACT LINUX C ALIGNMENTS)
// ============================================================================

// SQRingOffsets defines the byte offsets where userspace can find the SQ control
// fields inside the memory-mapped buffer returned by mmap(IORING_OFF_SQ_RING).
type SQRingOffsets struct {
	Head        uint32 // Offset to atomic head pointer (advanced by kernel)
	Tail        uint32 // Offset to atomic tail pointer (advanced by userspace)
	RingMask    uint32 // Offset to bitmask used for ring index wrapping (entries - 1)
	RingEntries uint32 // Offset to total capacity of submission queue
	Flags       uint32 // Offset to ring flags (e.g. IORING_SQ_NEED_WAKEUP)
	Dropped     uint32 // Offset to count of dropped SQEs (invalid submissions)
	Array       uint32 // Offset to indirection array mapping ring slots to SQEs
	Resv1       uint32 // Reserved padding
	Resv2       uint64 // Reserved padding
}

// CQRingOffsets defines the byte offsets where userspace can find the CQ control
// fields inside the memory-mapped buffer.
type CQRingOffsets struct {
	Head        uint32 // Offset to atomic head pointer (advanced by userspace)
	Tail        uint32 // Offset to atomic tail pointer (advanced by kernel)
	RingMask    uint32 // Offset to bitmask used for ring index wrapping
	RingEntries uint32 // Offset to total capacity of completion queue
	Overflow    uint32 // Offset to count of dropped CQEs due to full completion queue
	CQEs        uint32 // Offset to start of the io_uring_cqe array
	Flags       uint32 // Offset to CQ flags
	Resv1       uint32 // Reserved padding
	Resv2       uint64 // Reserved padding
}

// IOUringParams is passed to sys_io_uring_setup. The kernel populates it with
// the exact sizes, feature flags, and byte offsets required to mmap the rings.
type IOUringParams struct {
	SqEntries    uint32        // Userspace requests N entries; kernel sets actual power-of-2 size
	CqEntries    uint32        // Kernel sets CQ size (usually 2 * SqEntries)
	Flags        uint32        // Setup flags (e.g. IORING_SETUP_SQPOLL, IORING_SETUP_IOPOLL)
	SqThreadCPU  uint32        // Bound CPU core if SQPOLL is enabled
	SqThreadIdle uint32        // Milliseconds of idle before kernel SQPOLL thread sleeps
	Features     uint32        // Bitmask of supported kernel features (e.g. SINGLE_MMAP)
	WqFd         uint32        // Workqueue FD if attaching to existing ring
	Resv         [3]uint32     // Reserved
	SqOff        SQRingOffsets // SQ byte offsets populated by kernel
	CqOff        CQRingOffsets // CQ byte offsets populated by kernel
}

// IOUringSQE is the Submission Queue Entry submitted by userspace to request an I/O operation.
// It is strictly 64 bytes wide to ensure cache line alignment and avoid false sharing.
type IOUringSQE struct {
	Opcode      uint8     // Operation code (e.g. IORING_OP_ACCEPT, IORING_OP_RECV, IORING_OP_SEND)
	Flags       uint8     // IOSQE_* flags (e.g. IOSQE_FIXED_FILE, IOSQE_IO_LINK)
	Ioprio      uint16    // Priority of the I/O request
	Fd          int32     // Target file/socket descriptor
	Off         uint64    // Offset into file (0 for network stream sockets)
	Addr        uint64    // Memory address pointer to user data buffer (cast to uint64)
	Len         uint32    // Buffer length in bytes
	CmdFlags    uint32    // Operation-specific flags (e.g. msg_flags or accept_flags)
	UserData    uint64    // Opaque 64-bit tag echoed back in CQE when operation completes
	BufIndex    uint16    // Index into registered fixed buffers, if used
	Personality uint16    // Credential personality ID
	SpliceFdIn  int32     // Input descriptor for splice operations
	Pad2        [2]uint64 // 16 bytes of padding ensuring total struct size == 64 bytes
}

// IOUringCQE is the Completion Queue Entry posted by the kernel when an operation finishes.
// It is strictly 16 bytes wide.
type IOUringCQE struct {
	UserData uint64 // Echoed directly from the matching SQE.UserData
	Res      int32  // Return value: bytes read/sent, new client FD, or negative errno (-EAGAIN)
	Flags    uint32 // Operation-specific completion flags
}

// ============================================================================
// 3. ZERO-CGO IO_URING RING WRAPPER IN PURE GO
// ============================================================================

// Ring encapsulates the memory-mapped Submission and Completion queues.
type Ring struct {
	ringFd    int
	sqEntries uint32
	cqEntries uint32

	// SQ ring pointers mapped directly into kernel memory
	sqRingMmap []byte
	sqHead     *uint32
	sqTail     *uint32
	sqMask     uint32
	sqArray    *[1 << 20]uint32

	// SQE array mapped from IORING_OFF_SQES
	sqeMmap []byte
	sqes    *[1 << 20]IOUringSQE

	// CQ ring pointers mapped directly into kernel memory
	cqHead *uint32
	cqTail *uint32
	cqMask uint32
	cqes   *[1 << 20]IOUringCQE

	// Number of SQEs added since the last call to sys_io_uring_enter
	pendingSubmissions uint32
}

// NewRing initializes an io_uring instance and memory-maps its rings in pure Go.
func NewRing(entries uint32) (*Ring, error) {
	var params IOUringParams

	// Syscall 1: sys_io_uring_setup
	// Allocates the internal kernel ring buffers and returns a file descriptor.
	fd, _, errno := unix.Syscall(
		SYS_IO_URING_SETUP,
		uintptr(entries),
		uintptr(unsafe.Pointer(&params)),
		0,
	)
	if errno != 0 {
		return nil, fmt.Errorf("sys_io_uring_setup failed: %w", errno)
	}
	ringFd := int(fd)

	// Calculate memory-map sizes based on kernel offsets:
	sringSize := params.SqOff.Array + params.SqEntries*4
	cringSize := params.CqOff.CQEs + params.CqEntries*16
	ringSize := sringSize
	if cringSize > ringSize {
		ringSize = cringSize
	}

	// Map SQ and CQ rings into userspace memory.
	// Modern Linux kernels (IORING_FEAT_SINGLE_MMAP) map both queues in one region.
	sqRingMmap, err := unix.Mmap(
		ringFd,
		IORING_OFF_SQ_RING,
		int(ringSize),
		unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_SHARED|unix.MAP_POPULATE,
	)
	if err != nil {
		unix.Close(ringFd)
		return nil, fmt.Errorf("mmap SQ/CQ ring failed: %w", err)
	}

	// Map the flat array of 64-byte SQE structures.
	sqeSize := params.SqEntries * 64
	sqeMmap, err := unix.Mmap(
		ringFd,
		IORING_OFF_SQES,
		int(sqeSize),
		unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_SHARED|unix.MAP_POPULATE,
	)
	if err != nil {
		_ = unix.Munmap(sqRingMmap)
		unix.Close(ringFd)
		return nil, fmt.Errorf("mmap SQEs failed: %w", err)
	}

	r := &Ring{
		ringFd:     ringFd,
		sqEntries:  params.SqEntries,
		cqEntries:  params.CqEntries,
		sqRingMmap: sqRingMmap,
		sqeMmap:    sqeMmap,

		// Derive SQ pointers using kernel byte offsets
		sqHead:  (*uint32)(unsafe.Pointer(&sqRingMmap[params.SqOff.Head])),
		sqTail:  (*uint32)(unsafe.Pointer(&sqRingMmap[params.SqOff.Tail])),
		sqMask:  *(*uint32)(unsafe.Pointer(&sqRingMmap[params.SqOff.RingMask])),
		sqArray: (*[1 << 20]uint32)(unsafe.Pointer(&sqRingMmap[params.SqOff.Array])),
		sqes:    (*[1 << 20]IOUringSQE)(unsafe.Pointer(&sqeMmap[0])),

		// Derive CQ pointers using kernel byte offsets
		cqHead: (*uint32)(unsafe.Pointer(&sqRingMmap[params.CqOff.Head])),
		cqTail: (*uint32)(unsafe.Pointer(&sqRingMmap[params.CqOff.Tail])),
		cqMask: *(*uint32)(unsafe.Pointer(&sqRingMmap[params.CqOff.RingMask])),
		cqes:   (*[1 << 20]IOUringCQE)(unsafe.Pointer(&sqRingMmap[params.CqOff.CQEs])),
	}

	return r, nil
}

// Close cleans up mmapped regions and closes the ring file descriptor.
func (r *Ring) Close() {
	if r.sqeMmap != nil {
		_ = unix.Munmap(r.sqeMmap)
	}
	if r.sqRingMmap != nil {
		_ = unix.Munmap(r.sqRingMmap)
	}
	if r.ringFd >= 0 {
		_ = unix.Close(r.ringFd)
	}
}

// AddSQE places an I/O request into the shared submission queue ring.
// If the ring is currently full, it flushes pending submissions to the kernel first.
func (r *Ring) AddSQE(sqe IOUringSQE) {
	tail := atomic.LoadUint32(r.sqTail)
	head := atomic.LoadUint32(r.sqHead)

	// Ring full check: if pending items reach ring capacity, flush to kernel
	if tail-head >= r.sqEntries {
		r.Flush()
		tail = atomic.LoadUint32(r.sqTail)
	}

	idx := tail & r.sqMask
	r.sqes[idx] = sqe
	r.sqArray[idx] = idx

	// Store with release semantics so kernel sees populated SQE before tail advances
	atomic.StoreUint32(r.sqTail, tail+1)
	r.pendingSubmissions++
}

// Flush dispatches queued submissions to the kernel without blocking.
func (r *Ring) Flush() {
	if r.pendingSubmissions == 0 {
		return
	}
	_, _, _ = unix.Syscall6(
		SYS_IO_URING_ENTER,
		uintptr(r.ringFd),
		uintptr(r.pendingSubmissions),
		0, // min_complete = 0 (do not block)
		0, // flags
		0, 0,
	)
	r.pendingSubmissions = 0
}

// Enter flushes all pending submissions and blocks until at least 'minComplete'
// completions are posted to the completion queue ring.
func (r *Ring) Enter(minComplete uint32) error {
	toSubmit := r.pendingSubmissions
	r.pendingSubmissions = 0

	_, _, errno := unix.Syscall6(
		SYS_IO_URING_ENTER,
		uintptr(r.ringFd),
		uintptr(toSubmit),
		uintptr(minComplete),
		IORING_ENTER_GETEVENTS,
		0, 0,
	)
	if errno != 0 && errno != unix.EINTR {
		return fmt.Errorf("sys_io_uring_enter failed: %w", errno)
	}
	return nil
}

// ============================================================================
// 4. SERVER IMPLEMENTATION & EVENT LOOP
// ============================================================================

type Server struct {
	port     int
	listenFd int
	ring     *Ring
	metrics  *common.Metrics

	// Pre-allocated static connection buffers:
	// Indexed directly by socket FD to eliminate runtime heap allocations.
	connBufs [MaxConnections][BufferSize]byte
}

func NewServer(port int) *Server {
	return &Server{
		port:    port,
		metrics: &common.Metrics{},
	}
}

// initListener sets up a standard POSIX TCP listening socket.
func (s *Server) initListener() error {
	sfd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err != nil {
		return fmt.Errorf("socket creation failed: %w", err)
	}
	s.listenFd = sfd

	// Allow immediate address/port reuse across restarts
	if err := unix.SetsockoptInt(s.listenFd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
		_ = unix.Close(s.listenFd)
		return fmt.Errorf("setsockopt SO_REUSEADDR failed: %w", err)
	}
	if err := unix.SetsockoptInt(s.listenFd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
		_ = unix.Close(s.listenFd)
		return fmt.Errorf("setsockopt SO_REUSEPORT failed: %w", err)
	}

	sa := &unix.SockaddrInet4{Port: s.port}
	if err := unix.Bind(s.listenFd, sa); err != nil {
		_ = unix.Close(s.listenFd)
		return fmt.Errorf("bind to port %d failed: %w", s.port, err)
	}

	if err := unix.Listen(s.listenFd, 4096); err != nil {
		_ = unix.Close(s.listenFd)
		return fmt.Errorf("listen failed: %w", err)
	}

	return nil
}

// queueAccept posts an asynchronous IORING_OP_ACCEPT request for the listener.
func (s *Server) queueAccept() {
	s.ring.AddSQE(IOUringSQE{
		Opcode:   IORING_OP_ACCEPT,
		Fd:       int32(s.listenFd),
		UserData: (TagAccept << 32) | uint64(uint32(s.listenFd)),
	})
}

// queueRecv posts an asynchronous IORING_OP_RECV request for a client socket.
func (s *Server) queueRecv(fd int32) {
	if fd < 0 || int(fd) >= MaxConnections {
		_ = unix.Close(int(fd))
		return
	}
	bufPtr := uintptr(unsafe.Pointer(&s.connBufs[fd][0]))
	s.ring.AddSQE(IOUringSQE{
		Opcode:   IORING_OP_RECV,
		Fd:       fd,
		Addr:     uint64(bufPtr),
		Len:      uint32(BufferSize),
		UserData: (TagRecv << 32) | uint64(uint32(fd)),
	})
}

// queueSend posts an asynchronous IORING_OP_SEND request containing HTTP response.
func (s *Server) queueSend(fd int32, payload []byte) {
	dataPtr := uintptr(unsafe.Pointer(&payload[0]))
	s.ring.AddSQE(IOUringSQE{
		Opcode:   IORING_OP_SEND,
		Fd:       fd,
		Addr:     uint64(dataPtr),
		Len:      uint32(len(payload)),
		UserData: (TagSend << 32) | uint64(uint32(fd)),
	})
}

// Start launches the single-threaded io_uring event loop.
func (s *Server) Start() error {
	// Pin the server goroutine to a dedicated OS thread.
	// This prevents the Go runtime from migrating the goroutine between OS threads,
	// maximizing L1/L2 cache locality and memory barrier consistency.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := s.initListener(); err != nil {
		return err
	}
	defer unix.Close(s.listenFd)

	ring, err := NewRing(RingEntries)
	if err != nil {
		return err
	}
	s.ring = ring
	defer ring.Close()

	// Launch background resource & throughput telemetry sampler
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	common.StartMonitorTicker(ctx, s.metrics, 1*time.Second)

	// Arm the listener by queueing the initial accept request
	s.queueAccept()

	log.Printf("[IO_URING-VARIANT] Listening on http://0.0.0.0:%d/ping (Pure Go io_uring Completion Engine)\n", s.port)

	// ========================================================================
	// THE MAIN ASYNCHRONOUS COMPLETION LOOP
	// ========================================================================
	for {
		// Single Syscall: Submit pending SQEs and wait for at least 1 completion.
		// When under heavy load, this single call dispatches dozens of operations
		// and harvests dozens of completions simultaneously!
		if err := s.ring.Enter(1); err != nil {
			log.Printf("[IO_URING] Enter error: %v", err)
			continue
		}

		// Drain all completed CQEs from the Completion Queue ring
		head := atomic.LoadUint32(s.ring.cqHead)
		tail := atomic.LoadUint32(s.ring.cqTail)

		for head != tail {
			cqe := s.ring.cqes[head&s.ring.cqMask]
			head++

			// Demultiplex operation tag and socket descriptor from UserData
			opTag := cqe.UserData >> 32
			fd := int32(cqe.UserData & 0xffffffff)

			switch opTag {
			// ----------------------------------------------------------------
			// 1. ASYNCHRONOUS ACCEPT COMPLETED
			// ----------------------------------------------------------------
			case TagAccept:
				clientFd := cqe.Res
				// Re-arm accept immediately so listener is always ready for incoming SYNs
				s.queueAccept()

				if clientFd < 0 {
					// Negative errno (e.g. -EINTR or -EMFILE)
					continue
				}

				// Enforce bounds on client FD against pre-allocated buffer pool
				if int(clientFd) >= MaxConnections {
					_ = unix.Close(int(clientFd))
					continue
				}

				// Optimize socket options for low-latency HTTP request-reply:
				// TCP_NODELAY disables Nagle's algorithm (immediate packet dispatch)
				_ = unix.SetsockoptInt(int(clientFd), unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)

				// Increment active connection gauge
				s.metrics.IncConn()

				// Arm asynchronous RECV on the newly established connection
				s.queueRecv(clientFd)

			// ----------------------------------------------------------------
			// 2. ASYNCHRONOUS RECV COMPLETED
			// ----------------------------------------------------------------
			case TagRecv:
				n := cqe.Res
				if n <= 0 {
					// Client closed connection (EOF, res == 0) or socket error (res < 0)
					s.metrics.DecConn()
					_ = unix.Close(int(fd))
					continue
				}

				// Data has arrived and is ALREADY copied into s.connBufs[fd]!
				// No read() syscall needed. Inspect HTTP request:
				reqData := s.connBufs[fd][:n]
				if bytes.Contains(reqData, []byte("/ping")) {
					// Asynchronously transmit static 200 OK pong response
					s.queueSend(fd, common.Status200Bytes)
				} else {
					// Return 404 for unknown endpoints
					notFound := []byte("HTTP/1.1 404 Not Found\r\nContent-Length: 9\r\nConnection: close\r\n\r\nNot Found")
					s.queueSend(fd, notFound)
				}

			// ----------------------------------------------------------------
			// 3. ASYNCHRONOUS SEND COMPLETED
			// ----------------------------------------------------------------
			case TagSend:
				if cqe.Res <= 0 {
					// Socket closed or write error
					s.metrics.DecConn()
					_ = unix.Close(int(fd))
					continue
				}

				// Response sent successfully: record completed request in telemetry
				s.metrics.IncReq()

				// Re-arm asynchronous RECV on the same socket for HTTP keep-alive!
				s.queueRecv(fd)
			}
		}

		// Advance CQ ring head pointer to release processed slots back to the kernel
		atomic.StoreUint32(s.ring.cqHead, head)
	}
}

func main() {
	port := flag.Int("port", 8080, "TCP port to listen on")
	flag.Parse()

	server := NewServer(*port)
	if err := server.Start(); err != nil {
		log.Fatalf("Server fatal error: %v", err)
	}
}

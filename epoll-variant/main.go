package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"uring-go/common"

	"golang.org/x/sys/unix"
)

// EpollServer encapsulates the low-level Linux epoll event loop.
// Instead of spawning 1 goroutine per connection (like net/http), this server
// runs on a single OS thread using the Linux epoll reactor pattern.
type EpollServer struct {
	cfg      common.ServerConfig
	metrics  *common.Metrics
	listenFd int   // File descriptor for the listening TCP server socket
	epollFd  int   // File descriptor pointing to the in-kernel struct eventpoll instance
	running  int32 // Atomic flag to coordinate shutdown across signal handlers
}

// NewEpollServer initializes the EpollServer instance.
func NewEpollServer(cfg common.ServerConfig, metrics *common.Metrics) *EpollServer {
	return &EpollServer{
		cfg:      cfg,
		metrics:  metrics,
		listenFd: -1,
		epollFd:  -1,
	}
}

// Start sets up the non-blocking listener, creates the epoll instance, and runs the event loop.
func (s *EpollServer) Start() error {
	atomic.StoreInt32(&s.running, 1)

	// Step 1: Initialize non-blocking listener socket
	if err := s.initListener(); err != nil {
		return fmt.Errorf("init listener failed: %w", err)
	}
	defer unix.Close(s.listenFd)

	// Step 2: Initialize epoll instance and register listener
	if err := s.initEpoll(); err != nil {
		return fmt.Errorf("init epoll failed: %w", err)
	}
	defer unix.Close(s.epollFd)

	log.Printf("[EPOLL-VARIANT] Listening on http://%s%s (Single-thread Epoll Reactor)\n", s.cfg.Address(), common.PingPath)

	// Step 3: Run the core event loop
	return s.eventLoop()
}

// Stop signals the event loop to terminate gracefully.
func (s *EpollServer) Stop(ctx context.Context) error {
	log.Println("[EPOLL-VARIANT] Graceful shutdown initiated...")
	atomic.StoreInt32(&s.running, 0)

	// Closing the listener socket unblocks any pending accept/epoll operations
	if s.epollFd >= 0 {
		if s.listenFd >= 0 {
			unix.Close(s.listenFd)
			s.listenFd = -1
		}
	}
	return nil
}

// -----------------------------------------------------------------------------
// Core Epoll Implementation & Syscall Breakdown
// -----------------------------------------------------------------------------

// initListener creates a raw TCP socket, enables SO_REUSEADDR, makes it non-blocking,
// binds to s.cfg.Port, and begins listening for incoming TCP handshakes.
func (s *EpollServer) initListener() error {
	// Syscall: socket(domain, type, protocol)
	// - unix.AF_INET:      IPv4 Internet protocols
	// - unix.SOCK_STREAM:  Two-way, reliable, connection-based byte streams (TCP)
	// - 0:                 Default protocol for stream sockets (IPPROTO_TCP)
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}
	s.listenFd = fd

	// Syscall: setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, 1)
	// - unix.SOL_SOCKET:   Socket-level option (not IP or TCP specific)
	// - unix.SO_REUSEADDR: Allows immediate binding to the port even if previous sockets
	//                      are still lingering in the kernel's TCP TIME_WAIT state.
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
		return fmt.Errorf("setsockopt SO_REUSEADDR: %w", err)
	}

	// Syscall: fcntl(fd, F_SETFL, flags | O_NONBLOCK)
	// Sets the socket to non-blocking mode.
	// In an event-driven reactor, a blocking call would freeze the entire event loop!
	// Any read/write that cannot complete immediately returns EAGAIN instead of blocking.
	if err := unix.SetNonblock(fd, true); err != nil {
		return fmt.Errorf("setnonblock: %w", err)
	}

	// Syscall: bind(fd, sockaddr, addrlen)
	// Associates the socket with a specific network interface and port.
	// Addr defaults to [4]byte{0, 0, 0, 0} (INADDR_ANY), binding to all local interfaces.
	sa := &unix.SockaddrInet4{Port: s.cfg.Port}
	if err := unix.Bind(fd, sa); err != nil {
		return fmt.Errorf("bind: %w", err)
	}

	// Syscall: listen(fd, backlog)
	// Marks the socket as a passive listener that accepts incoming connection requests.
	// - 4096: The backlog depth — maximum number of pending, completed TCP handshakes
	//         waiting in the kernel's accept queue before incoming SYN packets are dropped.
	if err := unix.Listen(fd, 4096); err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	return nil
}

// initEpoll creates an epoll instance in kernel space and adds s.listenFd to the interest list.
func (s *EpollServer) initEpoll() error {
	// Syscall: epoll_create1(flags)
	// Allocates an in-kernel `struct eventpoll` containing:
	// 1. A Red-Black tree to store the interest list in O(log N).
	// 2. A Ready List (doubly-linked list) to hold active events in O(1).
	// - unix.EPOLL_CLOEXEC: Automatically close this FD if the process forks & execs,
	//                       preventing file descriptor leaks into child processes.
	epollFd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return fmt.Errorf("epoll_create1: %w", err)
	}
	s.epollFd = epollFd

	// EpollEvent specifies what events we care about for this descriptor:
	// - unix.EPOLLIN: Notify when there is data ready to read.
	//   For a listening socket, EPOLLIN means a new incoming connection is ready to accept()!
	// - Fd: The file descriptor associated with this event (returned back to us in epoll_wait).
	event := unix.EpollEvent{
		Events: unix.EPOLLIN,
		Fd:     int32(s.listenFd),
	}

	// Syscall: epoll_ctl(epfd, op, fd, event)
	// - unix.EPOLL_CTL_ADD: Insert a new file descriptor into epoll's internal Red-Black tree.
	if err := unix.EpollCtl(s.epollFd, unix.EPOLL_CTL_ADD, s.listenFd, &event); err != nil {
		return fmt.Errorf("epoll_ctl add listener: %w", err)
	}

	return nil
}

// eventLoop drives the reactor: calls unix.EpollWait in a loop, dispatches ready sockets,
// and processes incoming connections without spawning goroutines.
func (s *EpollServer) eventLoop() error {
	// Pre-allocate event slice outside the loop to avoid Go Garbage Collection allocations
	events := make([]unix.EpollEvent, 1024)

	for atomic.LoadInt32(&s.running) == 1 {
		// Syscall: epoll_wait(epfd, events, maxevents, timeout_ms)
		// Blocks the current OS thread until:
		// 1. One or more sockets in the Red-Black tree have active events (copied to `events`).
		// 2. The 100ms timeout expires (allows checking the atomic `s.running` flag for shutdown).
		// 3. An OS signal (like SIGINT/SIGTERM) interrupts the syscall.
		// Returns `n`: The exact count of active sockets. O(ready_events), NOT O(total_connections)!
		n, err := unix.EpollWait(s.epollFd, events, 100)
		if err != nil {
			if err == unix.EINTR {
				// Interrupted by an OS signal (e.g., SIGINT/Ctrl+C) -> retry loop
				continue
			}
			if atomic.LoadInt32(&s.running) == 0 {
				return nil
			}
			log.Printf("[EPOLL-VARIANT] epoll_wait error: %v\n", err)
			continue
		}

		// Process ONLY the ready sockets returned by the kernel
		for i := 0; i < n; i++ {
			fd := int(events[i].Fd)

			if fd == s.listenFd {
				// Event on listener socket -> New client connecting!
				s.handleAccept()
			} else {
				// Event on an existing client connection -> Data ready to read/write!
				s.handleClient(fd, events[i].Events)
			}
		}
	}
	return nil
}

// handleAccept drains all pending incoming connection requests on the listener socket.
func (s *EpollServer) handleAccept() {
	// In Level-Triggered mode with high concurrency, multiple clients may arrive in the same burst.
	// Loop until accept() returns EAGAIN to drain the kernel backlog completely.
	for {
		// Syscall: accept4(listenFd, sockaddr, addrlen, flags)
		// Atomic single syscall that accepts an incoming TCP connection AND sets flags:
		// - unix.SOCK_NONBLOCK: New client socket is non-blocking immediately.
		//                       (Saves a separate fcntl syscall per connection!)
		// - unix.SOCK_CLOEXEC:  New client socket closes on exec.
		nfd, _, err := unix.Accept4(s.listenFd, unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
		if err != nil {
			// EAGAIN or EWOULDBLOCK: No more pending connections in the listen queue right now
			if err == unix.EAGAIN || err == unix.EWOULDBLOCK || err == unix.EINTR {
				break
			}
			return
		}

		s.metrics.IncConn()

		// Register the new client socket with epoll
		// - unix.EPOLLIN: Notify us when the client sends an HTTP request payload
		ev := unix.EpollEvent{
			Events: unix.EPOLLIN,
			Fd:     int32(nfd),
		}

		// Insert new socket into epoll Red-Black tree
		if err := unix.EpollCtl(s.epollFd, unix.EPOLL_CTL_ADD, nfd, &ev); err != nil {
			unix.Close(nfd)
			s.metrics.DecConn()
			break
		}
	}
}

// handleClient reads the HTTP request from the client socket and writes the response.
func (s *EpollServer) handleClient(fd int, events uint32) {
	// Check for hang-up or socket error flags:
	// - unix.EPOLLHUP:   Socket was hung up (e.g. client crashed or closed unexpectedly).
	// - unix.EPOLLERR:   Error condition on the socket (e.g. TCP reset RST received).
	// - unix.EPOLLRDHUP: Peer closed connection or shut down writing half of TCP socket.
	if events&(unix.EPOLLHUP|unix.EPOLLERR|unix.EPOLLRDHUP) != 0 {
		unix.EpollCtl(s.epollFd, unix.EPOLL_CTL_DEL, fd, nil)
		unix.Close(fd)
		s.metrics.DecConn()
		return
	}

	// Read buffer allocated on the stack
	var buf [1024]byte

	// Syscall: read(fd, buf, len)
	// Reads available bytes from the socket's receive buffer in kernel space into user space.
	n, err := unix.Read(fd, buf[:])
	if n <= 0 || err != nil {
		if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
			// In non-blocking sockets, EAGAIN means no data ready right now
			return
		}

		// n == 0 means clean EOF (peer closed the TCP connection gracefully with a FIN packet).
		// Clean up: delete from epoll interest tree and close the socket FD.
		unix.EpollCtl(s.epollFd, unix.EPOLL_CTL_DEL, fd, nil)
		unix.Close(fd)
		s.metrics.DecConn()
		return
	}

	// Successfully received HTTP request bytes -> increment request counter
	s.metrics.IncReq()

	// Syscall: write(fd, buf, len)
	// Writes the pre-formatted wire-level HTTP 200 OK "pong" directly into the kernel's
	// TCP transmit buffer. Since keep-alive is enabled, the connection stays open.
	if _, err := unix.Write(fd, common.Status200Bytes); err != nil {
		s.metrics.IncErr()
		unix.EpollCtl(s.epollFd, unix.EPOLL_CTL_DEL, fd, nil)
		unix.Close(fd)
		s.metrics.DecConn()
	}
}

// -----------------------------------------------------------------------------
// Standard Lifecycle Main (Matches http-variant exactly)
// -----------------------------------------------------------------------------

func main() {
	host := flag.String("host", "0.0.0.0", "Bind host address")
	port := flag.Int("port", 8080, "Bind port")
	metricsSec := flag.Int("metrics", 1, "Metrics reporting interval in seconds (0 to disable)")
	flag.Parse()

	cfg := common.ServerConfig{
		Host:            *host,
		Port:            *port,
		MetricsInterval: time.Duration(*metricsSec) * time.Second,
	}

	metrics := &common.Metrics{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Live telemetry ticker
	common.StartMonitorTicker(ctx, metrics, cfg.MetricsInterval)

	server := NewEpollServer(cfg, metrics)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.Start()
	}()

	select {
	case sig := <-sigChan:
		log.Printf("[EPOLL-VARIANT] Received signal: %v. Shutting down.\n", sig)
	case err := <-serverErr:
		log.Fatalf("[EPOLL-VARIANT] Server error: %v\n", err)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if err := server.Stop(shutdownCtx); err != nil {
		log.Printf("[EPOLL-VARIANT] Shutdown error: %v\n", err)
	}

	vol, invol, _ := common.GetProcessContextSwitches()
	log.Printf("[EPOLL-VARIANT] Final Summary: Total Reqs: %d | Total Errors: %d | Final RSS: %.2f MB | Voluntary Context Switches: %d | Involuntary: %d\n",
		metrics.TotalRequests,
		metrics.TotalErrors,
		common.GetOSMemoryRSS(),
		vol,
		invol,
	)
}

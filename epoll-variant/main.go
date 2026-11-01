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
type EpollServer struct {
	cfg      common.ServerConfig
	metrics  *common.Metrics
	listenFd int
	epollFd  int
	running  int32
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

	// Wake up epoll_wait if it's currently blocked waiting for events
	if s.epollFd >= 0 {
		if s.listenFd >= 0 {
			unix.Close(s.listenFd)
			s.listenFd = -1
		}
	}
	return nil
}

// -----------------------------------------------------------------------------
// Core Epoll Implementation
// -----------------------------------------------------------------------------

// initListener creates a raw TCP socket, enables SO_REUSEADDR, makes it non-blocking,
// binds to s.cfg.Port, and begins listening.
func (s *EpollServer) initListener() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}
	s.listenFd = fd

	// SO_REUSEADDR prevents "address already in use" errors during rapid restarts
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
		return fmt.Errorf("setsockopt SO_REUSEADDR: %w", err)
	}

	if err := unix.SetNonblock(fd, true); err != nil {
		return fmt.Errorf("setnonblock: %w", err)
	}

	sa := &unix.SockaddrInet4{Port: s.cfg.Port}
	// Addr defaults to [4]byte{0, 0, 0, 0} (binds to all interfaces)
	if err := unix.Bind(fd, sa); err != nil {
		return fmt.Errorf("bind: %w", err)
	}

	if err := unix.Listen(fd, 4096); err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	return nil
}

// initEpoll creates an epoll instance via unix.EpollCreate1(0) and adds s.listenFd
// with unix.EPOLLIN to the interest list.
func (s *EpollServer) initEpoll() error {
	epollFd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return fmt.Errorf("epoll_create1: %w", err)
	}
	s.epollFd = epollFd

	event := unix.EpollEvent{
		Events: unix.EPOLLIN,
		Fd:     int32(s.listenFd),
	}
	if err := unix.EpollCtl(s.epollFd, unix.EPOLL_CTL_ADD, s.listenFd, &event); err != nil {
		return fmt.Errorf("epoll_ctl add listener: %w", err)
	}

	return nil
}

// eventLoop drives the reactor: calls unix.EpollWait in a loop, accepts incoming connections,
// and processes read/write events on active sockets.
func (s *EpollServer) eventLoop() error {
	events := make([]unix.EpollEvent, 1024)

	for atomic.LoadInt32(&s.running) == 1 {
		n, err := unix.EpollWait(s.epollFd, events, 100) // 100ms timeout
		if err != nil {
			if err == unix.EINTR {
				continue // Interrupted by OS signal, retry
			}
			if atomic.LoadInt32(&s.running) == 0 {
				return nil
			}
			log.Printf("[EPOLL-VARIANT] epoll_wait error: %v\n", err)
			continue
		}

		for i := 0; i < n; i++ {
			fd := int(events[i].Fd)
			if fd == s.listenFd {
				s.handleAccept()
			} else {
				s.handleClient(fd, events[i].Events)
			}
		}
	}
	return nil
}

// handleAccept drains all incoming connection requests on the listener socket.
func (s *EpollServer) handleAccept() {
	for {
		nfd, _, err := unix.Accept4(s.listenFd, unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
		if err != nil {
			// EAGAIN/EWOULDBLOCK means all pending connections have been accepted
			if err == unix.EAGAIN || err == unix.EWOULDBLOCK || err == unix.EINTR {
				break
			}
			return
		}

		s.metrics.IncConn()

		ev := unix.EpollEvent{
			Events: unix.EPOLLIN,
			Fd:     int32(nfd),
		}
		if err := unix.EpollCtl(s.epollFd, unix.EPOLL_CTL_ADD, nfd, &ev); err != nil {
			unix.Close(nfd)
			s.metrics.DecConn()
			break
		}
	}
}

// handleClient reads the HTTP request from client socket and writes common.Status200Bytes.
func (s *EpollServer) handleClient(fd int, events uint32) {
	// If peer closed connection or error occurred
	if events&(unix.EPOLLHUP|unix.EPOLLERR|unix.EPOLLRDHUP) != 0 {
		unix.EpollCtl(s.epollFd, unix.EPOLL_CTL_DEL, fd, nil)
		unix.Close(fd)
		s.metrics.DecConn()
		return
	}

	var buf [1024]byte
	n, err := unix.Read(fd, buf[:])
	if n <= 0 || err != nil {
		if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
			return
		}
		// Client disconnected or read error -> clean up
		unix.EpollCtl(s.epollFd, unix.EPOLL_CTL_DEL, fd, nil)
		unix.Close(fd)
		s.metrics.DecConn()
		return
	}

	// Valid HTTP request
	s.metrics.IncReq()
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

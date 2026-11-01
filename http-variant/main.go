package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"uring-go/common"
)

// HTTPServer encapsulates the standard library net/http implementation.
type HTTPServer struct {
	cfg     common.ServerConfig
	metrics *common.Metrics
	server  *http.Server
}

// NewHTTPServer constructs a new HTTP server instance with metric tracking hooks.
func NewHTTPServer(cfg common.ServerConfig, metrics *common.Metrics) *HTTPServer {
	s := &HTTPServer{
		cfg:     cfg,
		metrics: metrics,
	}

	mux := http.NewServeMux()
	s.RegisterRoutes(mux)

	s.server = &http.Server{
		Addr:    cfg.Address(),
		Handler: mux,

		// ConnState hooks let us track active connection counts accurately
		// as Go's netpoller accepts, idles, and terminates sockets.
		ConnState: func(conn net.Conn, state http.ConnState) {
			switch state {
			case http.StateNew:
				metrics.IncConn()
			case http.StateClosed, http.StateHijacked:
				metrics.DecConn()
			}
		},
	}

	return s
}

// RegisterRoutes binds HTTP endpoints to handlers.
func (s *HTTPServer) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc(common.PingPath, s.handlePing)
}

// handlePing responds with static "pong" payload and keep-alive headers.
func (s *HTTPServer) handlePing(w http.ResponseWriter, r *http.Request) {
	s.metrics.IncReq()

	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(common.PongBodyBytes); err != nil {
		s.metrics.IncErr()
	}
}

// Start begins listening and serving connections.
func (s *HTTPServer) Start() error {
	log.Printf("[HTTP-VARIANT] Listening on http://%s%s (Goroutine-per-conn)\n", s.cfg.Address(), common.PingPath)
	if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("http server failure: %w", err)
	}
	return nil
}

// Stop initiates graceful shutdown of the server.
func (s *HTTPServer) Stop(ctx context.Context) error {
	log.Println("[HTTP-VARIANT] Graceful shutdown initiated...")
	return s.server.Shutdown(ctx)
}

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

	// Global lifecycle context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start live in-process telemetry reporter
	common.StartMonitorTicker(ctx, metrics, cfg.MetricsInterval)

	// Instantiate HTTP Variant
	server := NewHTTPServer(cfg, metrics)

	// Handle OS shutdown signals (SIGINT, SIGTERM)
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Server start routine
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.Start()
	}()

	// Wait for shutdown trigger or critical server error
	select {
	case sig := <-sigChan:
		log.Printf("[HTTP-VARIANT] Received signal: %v. Shutting down.\n", sig)
	case err := <-serverErr:
		log.Fatalf("[HTTP-VARIANT] Server startup error: %v\n", err)
	}

	// Graceful shutdown with 5s timeout
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if err := server.Stop(shutdownCtx); err != nil {
		log.Printf("[HTTP-VARIANT] Shutdown error: %v\n", err)
	}

	// Print final post-mortem metrics
	vol, invol, _ := common.GetProcessContextSwitches()
	log.Printf(
		"[HTTP-VARIANT] Final Summary: Total Reqs: %d | Total Errors: %d | Final RSS: %.2f MB | Voluntary Context Switches: %d | Involuntary: %d\n",
		metrics.TotalRequests,
		metrics.TotalErrors,
		common.GetOSMemoryRSS(),
		vol,
		invol,
	)
}

package common

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"
)

// Pre-formatted wire-level HTTP/1.1 response for zero-allocation writes.
const (
	PingPath          = "/ping"
	Status200Response = "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 4\r\nConnection: keep-alive\r\n\r\npong"
)

var (
	Status200Bytes = []byte(Status200Response)
	PongBodyBytes  = []byte("pong")
)

// ServerConfig defines the shared network configuration across all variants.
type ServerConfig struct {
	Host            string
	Port            int
	MetricsInterval time.Duration
}

func (c ServerConfig) Address() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

// Metrics tracks real-time traffic statistics atomically across all variants.
type Metrics struct {
	ActiveConns   int64
	TotalRequests uint64
	TotalErrors   uint64
}

func (m *Metrics) IncConn() {
	atomic.AddInt64(&m.ActiveConns, 1)
}

func (m *Metrics) DecConn() {
	atomic.AddInt64(&m.ActiveConns, -1)
}

func (m *Metrics) IncReq() {
	atomic.AddUint64(&m.TotalRequests, 1)
}

func (m *Metrics) IncErr() {
	atomic.AddUint64(&m.TotalErrors, 1)
}

// GetOSMemoryRSS returns the current process Resident Set Size (RSS) in Megabytes directly from Linux /proc.
func GetOSMemoryRSS() float64 {
	pageSize := os.Getpagesize()
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	var size, resident, shared, text, lib, dataP, dt int64
	_, err = fmt.Sscanf(string(data), "%d %d %d %d %d %d %d", &size, &resident, &shared, &text, &lib, &dataP, &dt)
	if err != nil {
		return 0
	}
	rssBytes := resident * int64(pageSize)
	return float64(rssBytes) / (1024 * 1024)
}

// StartMonitorTicker prints telemetry at regular intervals until context cancellation.
func StartMonitorTicker(ctx context.Context, m *Metrics, interval time.Duration) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		var memStats runtime.MemStats
		var lastReqs uint64
		lastTime := time.Now()

		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				runtime.ReadMemStats(&memStats)
				rssMB := GetOSMemoryRSS()

				currentReqs := atomic.LoadUint64(&m.TotalRequests)
				elapsed := now.Sub(lastTime).Seconds()
				rps := float64(currentReqs-lastReqs) / elapsed

				lastReqs = currentReqs
				lastTime = now

				fmt.Printf("[TELEMETRY] RPS: %9.1f | Conns: %6d | Reqs: %10d | Goroutines: %6d | RSS: %6.1f MB | GoAlloc: %6.1f MB\n",
					rps,
					atomic.LoadInt64(&m.ActiveConns),
					currentReqs,
					runtime.NumGoroutine(),
					rssMB,
					float64(memStats.Alloc)/(1024*1024),
				)
			}
		}
	}()
}

// GetProcessContextSwitches returns voluntary and involuntary context switches from /proc/self/status.
func GetProcessContextSwitches() (voluntary int64, involuntary int64, err error) {
	var rusage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &rusage); err != nil {
		return 0, 0, err
	}
	return rusage.Nvcsw, rusage.Nivcsw, nil
}

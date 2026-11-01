# Problem Statement: Network I/O Evolution Benchmark
## Comparative Analysis: `net/http` vs. Raw `epoll` vs. Bare-Metal `io_uring`

---

## 1. Executive Summary & Objective

The objective of this project is to implement, profile, and benchmark three fundamentally distinct Linux network I/O paradigms in Go under identical, tightly constrained system resources (CPU and Memory cgroups):

1. **Standard `net/http`**: Go's default goroutine-per-connection runtime model (abstracted readiness via Go netpoller).
2. **Raw `epoll`**: Low-level event-driven reactor loop using direct Linux system calls (`epoll_create1`, `epoll_ctl`, `epoll_wait`), non-blocking sockets, and fixed user-space buffers.
3. **Bare-Metal `io_uring`**: Asynchronous completion-based event loop using raw Linux syscalls (`io_uring_setup`, `io_uring_enter`), shared kernel/user-space ring buffers, and zero-syscall completion reaping.

Each implementation will be packaged into an identical containerized environment with strict cgroup limits (e.g., **1 CPU core**, **64MB / 128MB RAM**), subjected to extreme concurrency loads (C10K $\to$ C100K connections), and evaluated for throughput, tail latency, memory consumption, context switches, and kernel CPU time.

---

## 2. Theoretical Architecture & Core Hypothesis

```
                     LINUX NETWORK I/O TAXONOMY
   ┌─────────────────────────────────────────────────────────────┐
   │ 1. Standard Go (net/http)                                   │
   │    • 1 Goroutine per connection (~2KB–4KB stack initial)    │
   │    • Managed by Go runtime netpoller (epoll under the hood) │
   │    • High developer ergonomics; higher memory at scale      │
   ├─────────────────────────────────────────────────────────────┤
   │ 2. Handcrafted epoll (Readiness-based)                      │
   │    • Single or fixed-thread event loop                      │
   │    • 2 Syscalls per event: epoll_wait() -> read()/write()   │
   │    • Low memory footprint; syscall boundary tax             │
   ├─────────────────────────────────────────────────────────────┤
   │ 3. Bare-Metal io_uring (Completion-based)                   │
   │    • Shared memory ring buffers (SQ ring & CQ ring)         │
   │    • 0 Syscalls per I/O event during steady state           │
   │    • Batched submission & completion; zero context-switches │
   └─────────────────────────────────────────────────────────────┘
```

### Hypotheses to Verify:
1. **Memory Ceiling & OOM Boundary**:
   Under high concurrent idle connections (C50K+), `net/http` will experience memory exhaustion (OOMKill) first due to per-goroutine stack allocations, whereas `epoll` and `io_uring` will maintain a near-flat memory baseline using pooled buffers.
2. **Context Switching & Kernel CPU Time**:
   Under heavy request loads, `io_uring` will exhibit the lowest voluntary context switches and lowest kernel CPU time (`%sys`) due to ring-buffer batching, compared to `epoll`'s constant syscall transitions.
3. **Tail Latency (P99 / P99.9)**:
   Under saturation, `io_uring`'s completion queues will provide flatter, more predictable P99 latency than `net/http` (which suffers Go runtime scheduler and GC pauses under massive goroutine heaps).

---

## 3. Server Specifications & Protocol Baseline

To maintain absolute benchmarking fairness, all three servers must adhere to identical protocol constraints:

* **Protocol**: HTTP/1.1 over TCP.
* **Target Endpoint**: `GET /ping`
* **Response**: Static payload:
  ```http
  HTTP/1.1 200 OK\r\n
  Content-Type: text/plain\r\n
  Content-Length: 4\r\n
  Connection: keep-alive\r\n
  \r\n
  pong
  ```
* **Payload Size**: Constant (no dynamic serialization, no disk I/O, no business logic).
* **Connection Handling**: Support HTTP keep-alive connection reuse.

---

## 4. The 3 Implementations

### Variant A: Idiomatic Go (`net/http`)
* Standard `http.Server` with `http.HandleFunc`.
* Relies on Go runtime goroutine scheduler and built-in netpoller.
* **Purpose**: Serves as the industry control baseline.

### Variant B: Raw `epoll` (Custom Reactor)
* Bypasses `net.Listen` in favor of raw syscalls (`unix.Socket`, `unix.Bind`, `unix.Listen`, `unix.SetNonblock`).
* Single-threaded (or worker-pool) event loop calling `unix.EpollWait`.
* Manual state machine for connection reading, parsing, and sending.
* Zero goroutine spawning per connection.

### Variant C: Bare-Metal `io_uring` (Ring Buffer Engine)
* Bypasses Cgo: uses raw Linux syscalls (`SYS_IO_URING_SETUP`, `SYS_IO_URING_ENTER`) via `unix.Syscall`.
* Memory-maps the Submission Queue (SQ) and Completion Queue (CQ) rings directly into Go pointers via `unix.Mmap`.
* Handles connection lifecycle (`IORING_OP_ACCEPT`, `IORING_OP_RECV`, `IORING_OP_SEND`) through the ring buffers.
* Batched submission and completion reaping with minimal syscall transitions.

---

## 5. Controlled Benchmarking & Isolation Strategy

### A. Containerized Resource Constraints
Each server will be run inside a Docker container with strict Linux cgroup limits:
```bash
docker run --rm \
  --cpus="1.0" \
  --memory="128m" \
  --memory-swap="128m" \
  --pids-limit=1000 \
  -p 8080:8080 \
  uring-benchmark:<variant>
```

### B. Test Scenarios
1. **Throughput / Burst Test**:
   * Measure maximum Requests Per Second (RPS) under moderate concurrency (100–1,000 connections).
   * Tool: `wrk` or `bombardier`.
2. **High-Concurrency Stress Test (C10K $\to$ C50K)**:
   * Keep-alive connection saturation under tight 64MB / 128MB memory caps.
   * Observe which variant survives vs. triggers kernel OOMKill.
3. **Latency Profile under Saturation**:
   * Record P50, P90, P99, and P99.9 latency distributions at 80% saturation load.

### C. Telemetry Metrics to Capture
* **Throughput**: Peak requests/second (RPS).
* **Latency**: P50, P95, P99, max latency.
* **Memory Footprint**: Working set size (RSS) tracked via `cgroups` (`memory.current`).
* **Kernel vs. User CPU**: Measured via `pidstat -u 1`.
* **Syscall Count & Context Switches**: Measured via `perf stat -e context-switches,cpu-migrations,syscalls:sys_enter_*`.

---

## 6. Implementation Milestones

- [ ] **Milestone 1**: Project scaffold, shared HTTP response generator, and Variant A (`net/http`).
- [ ] **Milestone 2**: Variant B (`epoll`) with non-blocking raw sockets and event loop.
- [ ] **Milestone 3**: Variant C (`io_uring`) with raw syscalls, `mmap` rings, and SQE/CQE processing.
- [ ] **Milestone 4**: Dockerfile and reproducible cgroup test harness script.
- [ ] **Milestone 5**: Benchmark execution, telemetry logging, and final comparative report.

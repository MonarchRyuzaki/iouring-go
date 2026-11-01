# Benchmark Results & Telemetry Comparison

## System & Environment Baseline

| Parameter | Value |
|:---|:---|
| **OS / Kernel** | Linux `6.8.0-111-generic` (x86_64) |
| **Go Version** | `go1.27.3 linux/amd64` |
| **Target Endpoint** | `GET /ping` (Static HTTP/1.1 `pong`, keep-alive) |
| **Load Tool** | `wrk` (4 threads) |

---

## Comparative Scorecard

| Variant | Concurrency | Duration | Peak RPS | Sustained RPS | Idle RSS | Peak RSS | Peak Goroutines | Context Switches (Vol / Invol) | Total Errors |
|:---|:---|:---|:---|:---|:---|:---|:---|:---|:---|
| **Variant A: `net/http`** | **100 conns** | ~10s | **245,247.1** | ~240,000 | 8.0 MB | **16.6 MB** | 120 | *(Tracked cumulatively)* | 0 |
| **Variant A: `net/http`** | **1,000 conns** | ~30s | **282,158.1** | ~245,000–265,000 | 16.3 MB | **46.0 MB** | 1,109 | **787,904 / 197,794** | 0 |
| **Variant B: `epoll`** | *Pending* | - | - | - | - | - | - | - | - |
| **Variant C: `io_uring`** | *Pending* | - | - | - | - | - | - | - | - |

---

## Detailed Run Logs: Variant A (`net/http`)

### 1. Architecture Profile
* **Concurrency Model**: Idiomatic Go (1 goroutine spawned per active TCP connection).
* **I/O Engine**: Go runtime internal netpoller (epoll-backed).
* **Total Traffic Handled**: **9,790,841 requests** (~9.79 Million requests) across 2 test bursts.
* **Reliability**: **0 errors** (100% success rate).

---

### 2. Phase-by-Phase Breakdown

#### Phase 0: Cold Baseline (Idle)
* **Goroutines**: 4 (Go runtime internal background workers).
* **Memory (RSS)**: **7.9 MB – 8.3 MB**
* **Heap Allocated**: **0.4 MB – 0.8 MB**

#### Phase 1: Throughput Burst (100 Concurrent Connections, ~10s)
* **Traffic Inflow**: 0 $\to$ 2,433,534 requests.
* **Sustained Throughput**: **~240,000 – 245,000 req/sec**
* **Peak RPS**: **245,247.1 req/sec**
* **Active Goroutines**: Jumped from 4 $\to$ **112 – 120** (1:1 per-connection mapping).
* **Memory Footprint (RSS)**: Grew from 8.3 MB $\to$ **16.6 MB** (+8.3 MB delta).
* **Heap Allocated**: 2.2 MB – 3.0 MB.
* **Post-Run Recovery**: Once connections closed, goroutines dropped immediately back to 4, while RSS settled at 16.3 MB (retained by Go runtime/allocator).

#### Phase 2: High Concurrency Stress (1,000 Concurrent Connections, ~30s)
* **Traffic Inflow**: 2.43M $\to$ 9,790,841 requests (~7.35 Million requests served).
* **Sustained Throughput**: **~230,000 – 275,000 req/sec**
* **Peak RPS**: **282,158.1 req/sec**
* **Active Goroutines**: Scaled from 4 $\to$ **1,008 – 1,109** (exact reflection of connection load).
* **Memory Footprint (RSS)**: Climbed to **46.0 MB** (a **5.5x memory expansion** compared to idle!).
* **Heap Allocated**: 11.2 MB – 21.1 MB.

#### Phase 3: Final Process Summary (Upon Graceful Shutdown)
* **Total Requests Served**: `9,790,841`
* **Total Socket / Parsing Errors**: `0`
* **Final Process RSS**: `45.46 MB`
* **Voluntary Context Switches**: `787,904`
* **Involuntary Context Switches**: `197,794`
* **Total OS Context Switches**: **`985,698`** (~almost 1 million context switches across the run).

---

### 3. Key Technical Observations (The Baseline To Beat)

1. **Goroutine Stack Overhead is Real and Linear**:
   * As connections jumped from 0 $\to$ 100 $\to$ 1,000, active goroutines jumped 4 $\to$ 112 $\to$ 1,013.
   * Under 1,000 connections, Resident Memory hit **46.0 MB**. 
   * *Critical Implication*: If constrained to a **64MB Docker container**, `net/http` would risk hitting an **OOMKill at ~1,500–2,000 connections**, even with zero business logic!

2. **The Context Switch Tax**:
   * The Go scheduler and thread preemptions accumulated **~985,000 context switches** for ~9.8M requests (~1 context switch per 10 requests).
   * This provides a clear benchmark to compare against `epoll` and `io_uring`, which run on fixed-thread event loops.

3. **Raw Throughput Baseline**:
   * Standard library `net/http` is fast on Go 1.27, peaking at **~282k RPS** on modern Linux.
   * The goal for `epoll` and `io_uring` will be to match or exceed this throughput while **crushing the memory footprint** and **drastically reducing context switches**.

---

## Detailed Run Logs: Variant B (Raw `epoll`)
*Status: Awaiting implementation and benchmark runs.*

---

## Detailed Run Logs: Variant C (Bare-Metal `io_uring`)
*Status: Awaiting implementation and benchmark runs.*

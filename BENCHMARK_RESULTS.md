# Benchmark Results & Telemetry Comparison

## System & Environment Baseline (Containerized)

| Parameter | Value |
|:---|:---|
| **OS / Kernel** | Linux `6.8.0-111-generic` (x86_64) |
| **Isolation** | Docker Container (`alpine:3.19` base) |
| **cgroups CPU Limit** | **`1.0` Core** (`--cpus="1.0"`) |
| **cgroups RAM Limit** | **`128 MB`** (`--memory="128m" --memory-swap="128m"`) |
| **Target Endpoint** | `GET /ping` (Static HTTP/1.1 `pong`, keep-alive) |
| **Load Tool** | `wrk` (4 threads) |

---

## Comparative Scorecard (1.0 CPU Core & 128MB RAM Cap)

| Variant | Test Mode | Concurrency | Duration | Requests/sec | P50 Latency | P90 Latency | P99 Latency | Max Latency | Idle RSS | Peak RSS | Peak Goroutines | Total Errors |
|:---|:---|:---|:---|:---|:---|:---|:---|:---|:---|:---|:---|:---|
| **Variant A: `net/http`** | **Throughput** | **100 conns** | 10s | **31,606.10** | **2.31 ms** | **36.23 ms** | **53.07 ms** | 62.88 ms | 6.0 MB | **12.7 MB** | **113** | 0 |
| **Variant A: `net/http`** | **Stress** | **1,000 conns** | 30s | **26,482.79** | **21.18 ms** | **71.77 ms** | **78.34 ms** | 102.27 ms | 12.8 MB | **38.8 MB** | **1,010** | 0 |
| **Variant B: `epoll`** | Throughput | 100 conns | 10s | *Pending* | - | - | - | - | - | - | - | - |
| **Variant B: `epoll`** | Stress | 1,000 conns | 30s | *Pending* | - | - | - | - | - | - | - | - |
| **Variant C: `io_uring`** | Throughput | 100 conns | 10s | *Pending* | - | - | - | - | - | - | - | - |
| **Variant C: `io_uring`** | Stress | 1,000 conns | 30s | *Pending* | - | - | - | - | - | - | - | - |

---

## Detailed Run Logs: Variant A (`net/http`)

### 1. Architecture Profile
* **Image**: `uring-http:latest`
* **Concurrency Model**: Idiomatic Go (1 goroutine spawned per active TCP connection).
* **I/O Engine**: Go runtime internal netpoller (epoll-backed).
* **Hardware Ceiling**: Exactly **1.0 CPU core**, **128MB RAM** (cgroup v2 enforced).
* **Total Requests Handled**: **1,112,935 requests** across both benchmark runs.
* **Reliability**: **0 socket errors**, **0 timeouts** (100% success rate).

---

### 2. Run Telemetry & Latency Breakdown

#### Run 1: Throughput Mode (100 Connections, 10 Seconds)
```text
Running 10s test @ http://127.0.0.1:8081/ping
  4 threads and 100 connections
  Thread Stats   Avg      Stdev     Max   +/- Stdev
    Latency    10.19ms   14.75ms  62.88ms   82.41%
    Req/Sec     7.94k   306.03     8.88k    68.00%
  Latency Distribution
     50%    2.31ms
     75%   14.81ms
     90%   36.23ms
     99%   53.07ms
  316103 requests in 10.00s, 38.89MB read
Requests/sec:  31606.10
Transfer/sec:      3.89MB
```
* **Throughput**: **31,606.10 req/sec**
* **Memory Trajectory**:
  * Cold Idle: **6.0 MB**
  * Peak under load: **12.7 MB** (+6.7 MB delta)
* **Goroutines**: Scaled from 4 $\to$ **104–113** (1:1 with connection count).
* **Post-test State**: Goroutines dropped immediately back to 4. Memory held at **12.8 MB**.

---

#### Run 2: Concurrency Stress Mode (1,000 Connections, 30 Seconds)
```text
Running 30s test @ http://127.0.0.1:8081/ping
  4 threads and 1000 connections
  Thread Stats   Avg      Stdev     Max   +/- Stdev
    Latency    37.77ms   25.05ms 102.27ms   62.00%
    Req/Sec     6.66k   369.67    10.03k    79.08%
  Latency Distribution
     50%   21.18ms
     75%   68.34ms
     90%   71.77ms
     99%   78.34ms
  795738 requests in 30.05s, 97.89MB read
Requests/sec:  26482.79
Transfer/sec:      3.26MB
```
* **Throughput**: **26,482.79 req/sec** (~16.2% drop from 100 conns due to single-core scheduler contention).
* **Latency Profile**:
  * P50 jumped from **2.31 ms $\to$ 21.18 ms** (9.1x increase).
  * P99 reached **78.34 ms** (Max: 102.27 ms).
* **Memory Trajectory**:
  * Starting: 12.8 MB
  * Peak under load: **38.8 MB** (climbed to **30.3% of the entire 128MB container limit**!).
  * Heap Allocated: Oscillated between 10.9 MB – 20.5 MB under GC cycles.
* **Goroutines**: Scaled from 4 $\to$ **1,004–1,010**.

---

### 3. Core Architectural Takeaways for `net/http`

1. **The 1-Core CPU Reality**:
   * On the unconstrained host, Go spread work across all host cores, hitting ~240k RPS.
   * Under a strict **1.0 CPU core cap**, Go's runtime scheduler must juggle 1,000 goroutines on a single OS thread (`GOMAXPROCS=1`), capping throughput at **~26.5k–31.6k RPS**.
   * *This is our true single-core baseline.*

2. **The Memory Tax Under Concurrency**:
   * Idle baseline: **6.0 MB**
   * 100 connections: **12.7 MB**
   * 1,000 connections: **38.8 MB**
   * *The Extrapolation*: Under `net/http`, memory grows at **~32.8 MB per 1,000 connections**. If we pushed to **3,500–4,000 concurrent connections**, `net/http` would cross the 128MB limit and trigger a kernel **OOMKill**!

3. **Latency Degradation**:
   * As connections increased 10x (100 $\to$ 1,000), P50 latency increased by **9.1x** (2.31ms $\to$ 21.18ms) as the single core handled goroutine scheduling and channel/mutex coordination in the netpoller.

---

## Detailed Run Logs: Variant B (Raw `epoll`)
*Status: Awaiting implementation and benchmark runs.*

---

## Detailed Run Logs: Variant C (Bare-Metal `io_uring`)
*Status: Awaiting implementation and benchmark runs.*

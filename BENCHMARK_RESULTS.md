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

## Comparative Scorecard (Container Environment)

| Variant | Test Mode | Concurrency | Duration | Peak RPS | Sustained RPS | Idle RSS | Peak RSS | Peak Goroutines | Context Switches (Vol / Invol) |
|:---|:---|:---|:---|:---|:---|:---|:---|:---|:---|
| **Variant A: `net/http`** | Throughput | 100 conns | 10s | *Pending* | - | - | - | - | - |
| **Variant A: `net/http`** | Stress | 1,000 conns | 30s | *Pending* | - | - | - | - | - |
| **Variant B: `epoll`** | Throughput | 100 conns | 10s | *Pending* | - | - | - | - | - |
| **Variant B: `epoll`** | Stress | 1,000 conns | 30s | *Pending* | - | - | - | - | - |
| **Variant C: `io_uring`** | Throughput | 100 conns | 10s | *Pending* | - | - | - | - | - |
| **Variant C: `io_uring`** | Stress | 1,000 conns | 30s | *Pending* | - | - | - | - | - |

---

## Detailed Run Logs

### 1. Variant A: Standard Library (`net/http`)
*Status: Container image `uring-http:latest` built. Ready for containerized benchmarks.*

---

### 2. Variant B: Raw `epoll` Reactor
*Status: Awaiting implementation.*

---

### 3. Variant C: Bare-Metal `io_uring` Ring Engine
*Status: Awaiting implementation.*

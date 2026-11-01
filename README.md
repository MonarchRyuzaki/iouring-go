# uring-go: High-Performance Network I/O Evolution

A comparative study and benchmark of three fundamental Linux networking architectures in Go under identical, tightly constrained system resources (**1.0 CPU Core** and **128MB RAM**):

1. **Variant A: `net/http`** — Go runtime goroutine-per-connection netpoller.
2. **Variant B: Raw `epoll`** — Single-threaded non-blocking reactor loop using raw POSIX syscalls.
3. **Variant C: Bare-Metal `io_uring`** — Asynchronous completion-based event loop using shared kernel ring buffers.

---

## 📚 Documentation Index

* **[Problem Statement & Benchmark Spec](PROBLEM_STATEMENT.md)** — Architecture hypotheses, C10K test methodology, and metric vectors.
* **[Linux Epoll Architecture & Systems Theory](EPOLL_THEORY.md)** — Deep dive into `struct eventpoll`, Red-Black trees, ready lists, interrupt callbacks, Level vs. Edge Triggering, and the "Double Syscall Tax".
* **[Benchmark Scorecard & Telemetry Results](BENCHMARK_RESULTS.md)** — Official containerized performance data across throughput, tail latency, memory growth, and context switches.

---

## 🛠️ Quick Commands

```bash
# 1. Start the containerized server (e.g. epoll variant)
./scripts/run_container.sh uring-epoll:latest

# 2. View live container logs with telemetry
docker logs -f uring-bench-target

# 3. Monitor container PID externally (context switches, RSS, FDs)
./scripts/monitor.sh $(docker inspect -f '{{.State.Pid}}' uring-bench-target) 1

# 4. Run Throughput Test (100 conns, 10s)
./scripts/load_test.sh throughput

# 5. Run Concurrency Stress Test (1,000 conns, 30s)
./scripts/load_test.sh stress

# 6. Stop container
docker rm -f uring-bench-target
```

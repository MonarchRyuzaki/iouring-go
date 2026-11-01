# io_uring Deep Dive: Architecture, Memory-Mapped Rings, and Asynchronous Linux I/O

This document provides a comprehensive theoretical and architectural breakdown of **`io_uring`**, Linux's modern high-performance asynchronous I/O interface introduced by Jens Axboe in Linux 5.1.

---

## 1. The Architectural Evolution: From Readiness to Completion

To understand why `io_uring` was created, we must compare the two fundamentally different I/O paradigms in operating systems:

```
┌────────────────────────────────────────────────────────────────────────┐
│                        READINESS (epoll)                               │
├────────────────────────────────────────────────────────────────────────┤
│ 1. Userspace asks kernel: "Which descriptors are ready for I/O?"       │
│ 2. Kernel returns list of ready FDs via epoll_wait().                  │
│ 3. Userspace must issue synchronous syscalls (read / write) per FD.    │
│    --> Syscall Tax: Minimum 2N + 1 syscalls per N requests!            │
└────────────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────────────┐
│                       COMPLETION (io_uring)                            │
├────────────────────────────────────────────────────────────────────────┤
│ 1. Userspace tells kernel: "Read 1KB from FD into this buffer."        │
│ 2. Kernel performs the I/O asynchronously in the background.           │
│ 3. Kernel posts a completion notification: "Done: 1024 bytes read."    │
│    --> Zero Syscall Execution: Shared memory rings eliminate syscalls. │
└────────────────────────────────────────────────────────────────────────┘
```

### Why `epoll` Hit a Ceiling
1. **The Syscall Tax**: `epoll` is merely a *readiness notifier*. Once `epoll_wait` reports that FD 7 is readable, userspace must execute a synchronous `read(7, buf, len)` syscall. Each syscall triggers a CPU privilege transition (user $\to$ kernel $\to$ user), flushing CPU pipeline state, causing TLB and register context switches.
2. **Spectre & Meltdown Overhead**: Hardware mitigations (KPTI — Kernel Page Table Isolation) increased the CPU cycle cost of every user/kernel transition by 30% to 100%.
3. **No Support for Disk I/O**: `epoll` cannot poll regular files on disk — regular file descriptors always report as "ready", meaning file I/O on Linux was historically forced to block or rely on `posix_aio` (which uses internal pthreads and has notorious limitations).

---

## 2. The Core Foundation: Shared-Memory Ring Buffers

`io_uring` solves the syscall overhead by placing **two circular ring buffers in shared kernel-user memory**:
1. **Submission Queue (SQ)**: Userspace writes requests (SQEs) into this ring; the kernel consumes them.
2. **Completion Queue (CQ)**: The kernel writes completed results (CQEs) into this ring; userspace consumes them.

```
       USERSPACE                                    KERNEL
 ┌──────────────────────┐                   ┌──────────────────────┐
 │                      │                   │                      │
 │   SQ Tail (User +)   │ ────── mmap ────> │   SQ Head (Kernel -) │
 │                      │                   │                      │
 │   CQ Head (User -)   │ <───── mmap ───── │   CQ Tail (Kernel +) │
 │                      │                   │                      │
 └──────────────────────┘                   └──────────────────────┘
             ▲                                         │
             │           ZERO SYSCALL INTERACTION      │
             └─────────────────────────────────────────┘
```

Because these ring buffers reside in memory mapped by both userspace and the kernel (`mmap(2)`):
* Userspace can submit work **without making any syscalls** by updating the SQ tail pointer.
* Userspace can harvest completed events **without making any syscalls** by reading CQEs and updating the CQ head pointer.
* A single syscall (`sys_io_uring_enter`) is only required when the application needs to notify the kernel or wait for completions.

---

## 3. The 3 Linux Syscalls of `io_uring`

Unlike complex legacy interfaces, `io_uring` exposes only three raw system calls:

### Syscall 1: `sys_io_uring_setup` (Syscall #425 on x86_64)
```c
int io_uring_setup(u32 entries, struct io_uring_params *params);
```
* **Purpose**: Creates an `io_uring` instance with capacity for `entries` (must be a power of 2, e.g., 512, 1024).
* **Return**: A file descriptor representing the ring.
* **The Params Struct**: The kernel returns exact byte offsets (`sq_off` and `cq_off`) indicating where `head`, `tail`, `mask`, and entries are positioned in memory.

### Syscall 2: `sys_io_uring_enter` (Syscall #426 on x86_64)
```c
int io_uring_enter(unsigned int fd, unsigned int to_submit,
                   unsigned int min_complete, unsigned int flags,
                   sigset_t *sig);
```
* **Purpose**: Submits `to_submit` pending SQEs to the kernel, and optionally blocks until `min_complete` events have completed.
* **Flags**:
  * `IORING_ENTER_GETEVENTS`: Tells the kernel to wait for `min_complete` completions.
* **Dual Action**: Can submit **and** wait in a **single syscall**, cutting context switches in half.

### Syscall 3: `sys_io_uring_register` (Syscall #427 on x86_64)
```c
int io_uring_register(unsigned int fd, unsigned int opcode,
                      void *arg, unsigned int nr_args);
```
* **Purpose**: Registers resources (such as fixed file descriptors or pre-pinned userspace memory buffers) directly with the kernel.
* **Benefit**: The kernel holds persistent references, avoiding per-I/O page table pinning (`get_user_pages`) and file descriptor reference count atomic updates.

---

## 4. Memory Layout & Struct Alignments

### 1. Submission Queue Entry (`struct io_uring_sqe`) — Exactly 64 Bytes
Every operation submitted to `io_uring` is described by a 64-byte SQE:

```text
 0x00: [ opcode (1B) ][ flags (1B) ][ ioprio (2B) ][ fd (4B) ]
 0x08: [ off / addr2 (8B)                                    ]
 0x10: [ addr / splice_off_in (8B)                           ]
 0x18: [ len (4B)          ][ rw_flags / accept_flags (4B)   ]
 0x20: [ user_data (8B: Tag / pointer / connection ID)       ]
 0x28: [ buf_index/buf_group (2B) ][ personality ][ file_idx ]
 0x30: [ __pad2[0] (8B)                                      ]
 0x38: [ __pad2[1] (8B)                                      ]
 Total: 64 Bytes (Cache-line aligned)
```

Key Fields:
* `opcode`: The operation to perform (e.g., `IORING_OP_ACCEPT = 13`, `IORING_OP_RECV = 27`, `IORING_OP_SEND = 26`, `IORING_OP_CLOSE = 19`).
* `fd`: Target socket or file descriptor.
* `addr`: Buffer memory address (pointer passed as a 64-bit unsigned integer).
* `len`: Number of bytes to read or send.
* `user_data`: An opaque 64-bit tag. The kernel **echoes this exact 64-bit value back** in the completion entry (`cqe->user_data`). This lets us correlate completions with client state without hash maps!

### 2. Completion Queue Entry (`struct io_uring_cqe`) — Exactly 16 Bytes
Every completed operation produces a 16-byte CQE:

```text
 0x00: [ user_data (8B: Echoed from SQE)                    ]
 0x08: [ res (4B: Return code or bytes transferred)          ]
 0x0C: [ flags (4B)                                         ]
 Total: 16 Bytes
```

Key Fields:
* `user_data`: Identifies which request completed.
* `res`: The operation result. Equivalent to a syscall return value:
  * For `read`/`recv`: Number of bytes received, `0` for EOF, or `-errno` (e.g., `-EAGAIN`, `-ECONNRESET`).
  * For `accept`: The new client file descriptor (`nfd`), or `-errno`.
  * For `send`/`write`: Number of bytes sent.

---

## 5. Ring Memory-Mapping Architecture

When `io_uring_setup()` succeeds, the kernel allocates ring memory. In modern kernels ($\ge 5.4$), `IORING_FEAT_SINGLE_MMAP` allows mapping both SQ and CQ rings in a single mmap call:

```go
// 1. Map SQ ring + CQ ring (Single MMAP)
sqRingSize := sq_off.array + sq_entries * 4
cqRingSize := cq_off.cqes + cq_entries * 16
ringSize := max(sqRingSize, cqRingSize)

ringPtr, err := unix.Mmap(ringFd, IORING_OFF_SQ_RING, ringSize, PROT_READ|PROT_WRITE, MAP_SHARED|MAP_POPULATE)

// 2. Map SQE Array (64 bytes * sq_entries)
sqeSize := sq_entries * 64
sqePtr, err := unix.Mmap(ringFd, IORING_OFF_SQES, sqeSize, PROT_READ|PROT_WRITE, MAP_SHARED|MAP_POPULATE)
```

### The Submission Queue Indirection Layer
The SQ ring has an extra array of 32-bit integers (`sq_off.array`). Userspace places SQEs in the flat SQE array at index `i`, and writes `i` into `sq_ring->array[tail & mask]`. This allows the kernel to consume SQEs in arbitrary order if needed.

---

## 6. Lock-Free Atomic Coordination & Memory Barriers

To coordinate between userspace and kernel without mutex locks, `io_uring` uses **single-producer, single-consumer ring buffers** guarded by acquire/release memory barriers:

```
               SUBMISSION QUEUE (SQ)
 Producer: Userspace          Consumer: Kernel
 ────────────────────         ──────────────────
 1. sqe = &sqes[tail & mask]
 2. Fill sqe fields
 3. array[tail & mask] = index
 4. StoreRelease(&sq_tail, tail + 1)
                              5. LoadAcquire(&sq_tail)
                              6. Process SQEs
                              7. StoreRelease(&sq_head, head + N)

               COMPLETION QUEUE (CQ)
 Producer: Kernel             Consumer: Userspace
 ──────────────────           ────────────────────
 1. Write CQEs
 2. StoreRelease(&cq_tail, tail + N)
                              3. LoadAcquire(&cq_tail)
                              4. Read CQEs
                              5. StoreRelease(&cq_head, head + N)
```

In Go, we implement these memory semantics using `sync/atomic`:
* Read kernel tail: `atomic.LoadUint32((*uint32)(unsafe.Pointer(cqTailPtr)))` (Acquire)
* Advance user head: `atomic.StoreUint32((*uint32)(unsafe.Pointer(cqHeadPtr)), newHead)` (Release)

---

## 7. HTTP Server Lifecycle in `io_uring`

Here is how a high-throughput HTTP server processes traffic using `io_uring`:

```mermaid
sequenceDiagram
    autonumber
    actor Client
    participant App as Userspace (io_uring Engine)
    participant Kernel as Linux Kernel (io_uring)

    Note over App,Kernel: Initialization
    App->>Kernel: Submit IORING_OP_ACCEPT (listenFd)
    Kernel-->>App: Waiting on incoming SYN...

    Note over Client,Kernel: Connection Established
    Client->>Kernel: TCP SYN -> Handshake Complete
    Kernel->>App: CQE: res = clientFd (Accept Success!)

    Note over App,Kernel: Arm Next Accept & Arm Recv
    App->>Kernel: Submit IORING_OP_ACCEPT (keep listener armed)
    App->>Kernel: Submit IORING_OP_RECV (clientFd, readBuf)

    Note over Client,Kernel: HTTP Request Arrives
    Client->>Kernel: "GET /ping HTTP/1.1\r\n\r\n"
    Kernel->>App: CQE: res = 38 bytes (Recv Complete!)

    Note over App,Kernel: Send Response & Arm Next Recv
    App->>App: Parse /ping, prepare Status200Bytes
    App->>Kernel: Submit IORING_OP_SEND (clientFd, Status200Bytes)
    App->>Kernel: Submit IORING_OP_RECV (clientFd, readBuf for next req)
    Kernel->>Client: "HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\npong"
    Kernel->>App: CQE: res = 143 bytes (Send Complete!)
```

### Key Differences from `epoll`:
1. **Zero Intermediate Syscalls**: When the `RECV` CQE arrives, data is **already in userspace memory**. There is no call to `unix.Read()`.
2. **True Asynchronous Writes**: When sending the HTTP response, the application submits `IORING_OP_SEND`. The kernel handles socket buffer backpressure without `EAGAIN` retry loops in userspace.
3. **Batched Submissions**: Multiple operations (`ACCEPT`, `RECV`, `SEND`) can be queued simultaneously and dispatched with a single call to `io_uring_enter`.

---

## 8. Syscall Budget Comparison: 10,000 HTTP Requests

| Operation | `net/http` (Goroutines) | `epoll` (Reactor Loop) | `io_uring` (Batch Ring) |
|:---|:---|:---|:---|
| **Wait for Events** | Netpoller internal | 1 × `epoll_wait` per batch | 1 × `io_uring_enter` per batch |
| **Accept Sockets** | Synchronous `accept4` | Synchronous `accept4` | Asynchronous `IORING_OP_ACCEPT` |
| **Read HTTP Data** | Synchronous `read` | Synchronous `read` | **Zero syscalls** (Kernel DMA to buffer) |
| **Write Response** | Synchronous `write` | Synchronous `write` | **Zero syscalls** (Queued in SQ) |
| **Context Switches** | Very High (1 goroutine/conn) | Low (Single thread) | **Lowest** (Batched kernel polling) |
| **Syscalls per Request** | $\ge 2$ | $\ge 2$ | **$\approx 0.05$ – $0.1$** (Batched in SQ) |

This zero-syscall capability is the ultimate architectural promise of `io_uring`.

---

## 9. The Low-Level Plumbing: Offsets, Bit Math & Memory Layout Demystified

This section is your persistent reference guide for the low-level "magic" in the implementation. **You do not need to memorize these bit shifts or offsets**—in production libraries (like C `liburing` or Rust `tokio-uring`), this plumbing is hidden behind helper functions. 

However, knowing *why* it works will give you complete clarity when reading the raw code.

---

### A. Why Does the Kernel Give "Offsets" Instead of Pointers?

In Linux, the Kernel and your Go application run in **completely separate virtual address spaces**:
* The kernel accesses memory at privileged virtual addresses (e.g. `0xffff8880...`).
* Go accesses memory at userspace virtual addresses (e.g. `0x00007f9a...`).

```
 Physical RAM (Shared Page allocated by sys_io_uring_setup)
 ┌────────────────────────────────────────────────────────────┐
 │ [head (4B)] [tail (4B)] [ring_mask (4B)] ... [array...]    │
 └────────────────────────────────────────────────────────────┘
        ▲                                              ▲
        │ Mapped into kernel space                     │ Mapped into Go space
 0xffff888000123400                             0x7f9a10000000
 (Kernel's view)                                (Go's view via unix.Mmap)
```

If the kernel handed Go a raw C pointer like `0xffff888000123400`, your Go program would instantly crash with a **Segmentation Fault (`SIGSEGV`)** if it tried to read it.

Instead, the kernel provides **relative byte offsets** (distances from the start of the memory block):
* `params.SqOff.Head = 0` $\to$ `head` is at byte `0`.
* `params.SqOff.Tail = 4` $\to$ `tail` is at byte `4`.
* `params.SqOff.RingMask = 8` $\to$ `ring_mask` is at byte `8`.

In Go:
```go
// Treat byte #0 in our mmapped page as an unsigned 32-bit integer pointer
sqHead = (*uint32)(unsafe.Pointer(&sqRingMmap[params.SqOff.Head]))
```
Now, when Go reads `*sqHead`, it is inspecting the **exact same physical silicon memory cell** that the Linux kernel is inspecting.

---

### B. The Fast Circular Ring Trick: Why `tail & mask` Instead of `tail % entries`?

In a circular buffer of size 4, you want indices to cycle: `0, 1, 2, 3, 0, 1, 2, 3...`

Normally, you'd write a modulo operation:
```go
index = tail % 4
```
* **The Problem**: On modern x86/ARM CPUs, the division instruction (`DIV` / `IDIV`) is slow—it takes **15 to 40 CPU clock cycles**.
* **The Hardware Trick**: If your ring capacity is a **power of 2** (4, 512, 1024, 2048), you can replace division with a **bitwise AND (`&`)**, which takes only **1 clock cycle (0.3 nanoseconds)**!

#### How the Binary Math Works:
Suppose the ring size is `4`. The mask is always `entries - 1` = `4 - 1 = 3`.
In binary, `3` is `0011` (the last two bits are `1`).

Watch how `tail & 3` wraps automatically as `tail` increments endlessly:

| Tail (Decimal) | Tail (Binary) | Mask `3` (`0011`) | Result (`tail & mask`) | What happened? |
|:---|:---|:---|:---|:---|
| `0` | `0000` | `& 0011` | `0000` (**0**) | Slot 0 |
| `1` | `0001` | `& 0011` | `0001` (**1**) | Slot 1 |
| `2` | `0010` | `& 0011` | `0010` (**2**) | Slot 2 |
| `3` | `0011` | `& 0011` | `0011` (**3**) | Slot 3 |
| `4` | `0100` | `& 0011` | `0000` (**0**) | **Wrapped back to 0!** |
| `5` | `0101` | `& 0011` | `0001` (**1**) | Wrapped to 1! |

`tail` can increment to millions without ever resetting, and `tail & mask` will always wrap cleanly in a single sub-nanosecond CPU cycle.

---

### C. The 64-bit Suitcase: Packing `Tag` and `FD` into `UserData`

In `io_uring`, the kernel provides a single 64-bit field called `user_data` (like an opaque luggage tag). When an I/O operation finishes, the kernel echoes this exact number back in `cqe.UserData`.

When a completion arrives, userspace needs to know two pieces of information:
1. **What operation just finished?** (`Accept = 1`, `Recv = 2`, `Send = 3`)
2. **Which socket descriptor (`fd`) does it belong to?** (e.g. FD `7`)

A 64-bit integer (`uint64`) has 64 binary digits (8 bytes). We can pack two 32-bit integers side-by-side:

```
  Bit 63 ............................ 32 | Bit 31 ............................ 0
 ┌──────────────────────────────────────┬──────────────────────────────────────┐
 │          Operation Tag (32 bits)     │          Socket FD (32 bits)         │
 └──────────────────────────────────────┴──────────────────────────────────────┘
             High 32 Bits                           Low 32 Bits
```

#### 1. Packing (Before Submission):
```go
tag := uint64(2) // OpRecv
fd  := uint64(7) // Client Socket FD 7

// Shift the tag 32 bits left to put it in the upper half, then bitwise OR the FD into the lower half:
userData := (tag << 32) | fd
// In hex: 0x00000002_00000007
```

#### 2. Unpacking (When CQE Arrives):
```go
// Shift right by 32 bits: discards the lower 32 bits and brings the tag back:
opTag := cqe.UserData >> 32 // Yields 2 (OpRecv)

// Bitwise AND with 0xffffffff (32 ones): wipes out the upper half, leaving only the FD:
fd := int32(cqe.UserData & 0xffffffff) // Yields 7
```

**Why do this?**
* **Zero allocations**: No creating heap objects or structs per request.
* **Zero lookups**: No hash map searches (`conns[cqe.UserData]`).
* Demultiplexing takes **under 1 nanosecond**.

---

### D. The Physical Memory Layout in RAM

```text
1. The Control Ring Buffer (Mapped via IORING_OFF_SQ_RING)
 0x0000 [ Head: 4B        ] <-- Kernel updates this as it reads SQEs
 0x0004 [ Tail: 4B        ] <-- Userspace updates this as it posts SQEs
 0x0008 [ Ring Mask: 4B   ] <-- (Entries - 1)
 0x000C [ Ring Entries: 4B] <-- Total capacity (e.g. 1024)
 ...
 0x0040 [ CQEs start...   ] <-- Array of 16-byte Completion Queue Entries
 ...
 0x8040 [ SQ Array...     ] <-- Array of 4-byte integers pointing to SQE slots

2. The SQE Buffer (Mapped via IORING_OFF_SQES)
 Slot 0: [ Opcode(1B) | Flags(1B) | FD(4B) | Addr(8B) | Len(4B) | UserData(8B)... ] = 64 Bytes
 Slot 1: [ Opcode(1B) | Flags(1B) | FD(4B) | Addr(8B) | Len(4B) | UserData(8B)... ] = 64 Bytes
 Slot 2: [ Opcode(1B) | Flags(1B) | FD(4B) | Addr(8B) | Len(4B) | UserData(8B)... ] = 64 Bytes
```
* **Why 64 Bytes for SQEs?** Modern CPU L1 cache lines are exactly 64 bytes wide. One SQE occupies exactly one cache line, guaranteeing that memory reads never straddle cache boundaries or suffer false sharing.

---

### E. Quick Reference Cheat Sheet

| Code Snippet | What It Does | Why We Do It |
|:---|:---|:---|
| `unsafe.Pointer(&mmap[offset])` | Converts a byte offset into an actual Go typed pointer. | The kernel tells us where variables live relative to the mapped page. |
| `tail & mask` | Wraps circular ring indices without division. | Takes 1 clock cycle instead of 30 cycles for `tail % entries`. |
| `(tag << 32) \| fd` | Glues two 32-bit values into one 64-bit integer. | Lets the kernel carry both the operation type and socket FD in a single tag. |
| `userData >> 32` | Extracts the upper 32 bits (the operation tag). | Instant $O(1)$ zero-allocation event demultiplexing. |
| `userData & 0xffffffff` | Extracts the lower 32 bits (the socket descriptor). | Instant $O(1)$ socket recovery without hash maps. |
| 64-byte SQE | Cache-aligned submission entry. | Exactly matches CPU L1 cache line size. |


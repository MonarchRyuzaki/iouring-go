# The Epoll Architecture & Systems Theory
## Low-Level Mechanics, Kernel Internals, and Non-Blocking Event Loops

---

## 1. The Genesis: Why Epoll Exists (The C10K Problem)

In the 1990s and early 2000s, internet traffic scaled faster than operating system architectures. Handling 10,000 concurrent connections (**The C10K Problem**) paralyzed standard Linux servers. 

To understand why `epoll` was created, you must understand the failure modes of its predecessors:

### A. The Thread/Process-per-Connection Trap
* Spawning an OS thread or goroutine for every TCP socket works well at small scale (100–1,000 conns).
* At 50,000 connections, thread memory overhead (stack allocation) exhausts RAM, and the CPU spends more cycles on **kernel context-switching** than on processing application bytes.

### B. The Fall of `select()` (1983)
```c
int select(int nfds, fd_set *readfds, fd_set *writefds, fd_set *exceptfds, struct timeval *timeout);
```
1. **Hard Limit**: Fixed to `FD_SETSIZE` (usually 1,024 file descriptors).
2. **Userspace $\to$ Kernel Copy Overhead**: On *every single call*, the application must copy the entire bitmask of monitored file descriptors into kernel memory.
3. **$O(N)$ Linear Scan**: When `select()` unblocks, it returns the total number of ready events, but **not which descriptors are ready**. The kernel must iterate through all $N$ descriptors, and then userspace must iterate through all $N$ descriptors in a loop to check `FD_ISSET(fd, &readfds)`.

### C. The Fall of `poll()` (1997)
```c
int poll(struct pollfd *fds, nfds_t nfds, int timeout);
```
1. Replaced the 1,024 bitmask limit with an array of `struct pollfd`.
2. **Still Broken**: The entire array must still be passed to the kernel on every call ($O(N)$ memory transfer), and the application still has to iterate through all $N$ elements on return to find which sockets had events.

### The Epoll Paradigm Shift (Linux 2.5.44 / 2002)
Sangor Kang and others designed `epoll` with a fundamental insight:  
> **Decouple interest registration from event retrieval.**

Instead of passing the list of monitored sockets to the kernel on every loop iteration, the kernel maintains an **in-kernel stateful object** representing the interest set. You register a socket **once**. When events occur, the kernel returns **only the sockets that have active events** in $O(1)$ complexity.

---

## 2. Kernel Internals: Inside `struct eventpoll`

When you call `epoll_create1()`, the Linux kernel allocates an internal data structure called `struct eventpoll` (defined in `fs/eventpoll.c`):

```
                     LINUX KERNEL MEMORY SPACE
   ┌─────────────────────────────────────────────────────────────┐
   │                    struct eventpoll                         │
   │                                                             │
   │  1. Red-Black Tree (rbr)         2. Ready List (rdllist)    │
   │     • Stores ALL registered         • Doubly-linked list    │
   │       file descriptors              • Stores ONLY sockets   │
   │     • Fast search/insert: O(log N)    with active events    │
   │                                     • O(1) retrieval        │
   │           [Root FD]                         │               │
   │            /     \                          ▼               │
   │         [FD 4]   [FD 9]               [FD 9] <-> [FD 14]    │
   │         /    \                                              │
   │      [FD 2] [FD 7]                                          │
   │                                                             │
   │  3. Wait Queue (wq)                                         │
   │     • Application threads sleeping in epoll_wait()          │
   └─────────────────────────────────────────────────────────────┘
```

### The Two Core Data Structures:
1. **Red-Black Tree (`rbr`)**:
   * Stores the entire set of file descriptors being monitored (the *Interest List*).
   * Keyed by the file descriptor number.
   * Allows adding, modifying, or deleting descriptors in $O(\log N)$ time without linear table scans.
2. **Doubly-Linked List (`rdllist`)**:
   * Stores only the descriptors that currently have unhandled I/O events (the *Ready List*).
   * When `epoll_wait()` is called, the kernel doesn't scan the Red-Black tree; it checks if `rdllist` is non-empty.
   * If non-empty, it copies items from `rdllist` directly to user space in **$O(\text{ready\_events})$** time, completely independent of total monitored sockets!

---

## 3. How a Packet Reaches `epoll_wait` (The Interrupt Path)

How does a socket get placed onto the `rdllist`?

```
 [Network Card] ─── DMA Packet Arrival ───> [Kernel Ring Buffer]
                                                   │
                                            Hardware Interrupt
                                                   │
                                                   ▼
 [TCP/IP Stack] ─── Processes TCP Header ──> Socket Receive Queue
                                                   │
                                      Wakes up socket wait-queue
                                                   │
                                                   ▼
 [Kernel Callback] ───────────────> calls ep_poll_callback()
                                                   │
                               • Adds socket to rdllist
                               • Wakes up thread in epoll_wait()
```

1. **Hardware Event**: A network packet hits the Network Interface Card (NIC).
2. **DMA Transfer**: The NIC copies the packet into kernel memory via Direct Memory Access (DMA) and raises a hardware interrupt (IRQ).
3. **TCP Processing**: The Linux network softirq (`NET_RX_SOFTIRQ`) processes the TCP header and places the payload into the socket's receive buffer (`sk_receive_queue`).
4. **The Epoll Callback (`ep_poll_callback`)**:
   * When you registered the socket via `epoll_ctl(ADD)`, epoll attached an internal callback function (`ep_poll_callback`) to that socket’s wait queue.
   * As soon as data enters the socket's receive buffer, the socket triggers this callback.
   * The callback checks if the socket is already in `eventpoll.rdllist`. If not, it inserts it into `rdllist` and wakes up any application thread sleeping in `epoll_wait()`.

---

## 4. The 3 Core System Calls

### 1. `epoll_create1(int flags)`
```go
epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
```
* **Allocates** the in-kernel `struct eventpoll` and returns a file descriptor pointing to it.
* **Flag `EPOLL_CLOEXEC`**: Automatically closes the epoll file descriptor if the process executes an `execve()` child process, preventing socket leaks.

### 2. `epoll_ctl(int epfd, int op, int fd, struct epoll_event *event)`
```go
err := unix.EpollCtl(epfd, unix.EPOLL_CTL_ADD, clientFd, &unix.EpollEvent{
    Events: unix.EPOLLIN,
    Fd:     int32(clientFd),
})
```
* **Modifies** the Red-Black tree in the kernel.
* **Operations (`op`)**:
  * `unix.EPOLL_CTL_ADD`: Insert a new FD into the interest tree.
  * `unix.EPOLL_CTL_MOD`: Change monitored events for an existing FD.
  * `unix.EPOLL_CTL_DEL`: Remove an FD from the tree.
* **Common Event Flags**:
  * `EPOLLIN`: Socket is ready for `read()` (or listening socket has an incoming connection for `accept()`).
  * `EPOLLOUT`: Socket send buffer has space for `write()`.
  * `EPOLLRDHUP`: Peer closed connection or shut down writing half of TCP socket.
  * `EPOLLERR` / `EPOLLHUP`: Error condition or hung up (always monitored automatically).

### 3. `epoll_wait(int epfd, struct epoll_event *events, int maxevents, int timeout)`
```go
n, err := unix.EpollWait(epfd, eventsSlice, timeoutMs)
```
* **Blocks** until:
  1. One or more file descriptors enter the Ready List (`rdllist`), OR
  2. The `timeout` expires (pass `-1` to block indefinitely, `0` for non-blocking poll), OR
  3. Interrupted by an OS signal (`EINTR`).
* **Return Value**: Returns $N$ (the exact count of ready descriptors). You only iterate over `0 .. n-1`.

---

## 5. Level-Triggered (LT) vs. Edge-Triggered (ET)

This is the most critical design decision when implementing an epoll reactor.

| Concept | Level-Triggered (LT) — **Default** | Edge-Triggered (ET) — **`unix.EPOLLET`** |
|:---|:---|:---|
| **Metaphor** | Light stays ON while door is open. | Bell rings ONCE when door opens. |
| **Notification Condition** | Triggers as long as buffer **has unread bytes**. | Triggers **only when new data arrives** (state transition). |
| **Drain Requirement** | Can read partially; epoll will wake you again next loop. | **Must loop `read()` until `EAGAIN`/`EWOULDBLOCK`**. |
| **Risk** | High CPU churn if you don't read all data immediately. | **Deadlock/Hang** if you don't read all bytes (kernel won't notify again!). |
| **Performance** | Slightly more syscalls if buffer isn't drained. | Peak efficiency; fewest wakeups. |

### Why Edge-Triggered (ET) Demands Non-Blocking Sockets:
If you use `EPOLLET`:
```text
State 1: Socket empty (0 bytes).
State 2: 1,000 bytes arrive. Kernel triggers epoll event ONCE.
State 3: You read 500 bytes. (500 bytes still in buffer).
State 4: You call epoll_wait().
RESULT: DEADLOCK! No new packet arrived, so ET will NEVER fire again, 
        even though 500 bytes are sitting unread in the kernel!
```
To prevent this in ET:
1. Sockets **MUST** be non-blocking (`unix.SetNonblock(fd, true)`).
2. You **MUST** read in a `for` loop until `read()` returns `-1` with error `unix.EAGAIN` or `unix.EWOULDBLOCK`.

---

## 6. The Non-Blocking Socket Contract

In a single-threaded epoll reactor, **a blocking system call is fatal**. If thread A calls `read()` on a blocking socket waiting for bytes that haven't arrived, the entire event loop freezes, blocking all other 10,000 connections!

### Rules of Non-Blocking Sockets:
1. **Making Sockets Non-blocking**:
   * When creating listener: `unix.SetNonblock(listenFd, true)`
   * When accepting clients: Use `unix.Accept4(listenFd, unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)` to accept in non-blocking mode in a single atomic syscall!
2. **Handling `unix.EAGAIN` / `unix.EWOULDBLOCK`**:
   * It is **not** an error! It simply means: *"The buffer is currently empty; come back when epoll notifies you again."*

---

## 7. Anatomy of an HTTP Epoll Reactor Loop

Here is the exact state machine you will implement in [`epoll-variant/main.go`](file:///home/ryuzaki/Desktop/Projects/uring-go/epoll-variant/main.go):

```
                     EVENT LOOP DISPATCH CYCLE
                      ┌──────────────────────┐
                      │  unix.EpollWait()    │
                      └──────────┬───────────┘
                                 │
                   Iterate events[0 .. n-1]
                                 │
           ┌─────────────────────┴─────────────────────┐
           ▼                                           ▼
  Is event.Fd == listenFd?                    Is active client FD?
  (Incoming TCP Connection)                   (Ready for I/O)
           │                                           │
  unix.Accept4()                              unix.Read(fd, buf)
           │                                           │
  Set non-blocking                            Did peer disconnect?
           │                                  (n == 0 or ECONNRESET)
  unix.EpollCtl(ADD, clientFd)                         │
  (with EPOLLIN)                              ┌────────┴────────┐
                                              ▼                 ▼
                                            YES                NO
                                      unix.Close(fd)   unix.Write(fd, pong)
                                      Metrics.DecConn  Metrics.IncReq
```

### Clean Connection Teardown:
When a client closes the TCP connection:
1. `unix.Read(fd, buf)` returns `n == 0` (clean EOF) or an error like `unix.ECONNRESET`.
2. Delete from epoll: `unix.EpollCtl(epollFd, unix.EPOLL_CTL_DEL, fd, nil)`.
3. Close the socket: `unix.Close(fd)`.
4. Decrement active connection metric: `s.metrics.DecConn()`.

---

## 8. Why Epoll Will Beat `net/http` in Our Benchmark

When you run this container under 1,000 connections with a 128MB RAM cap:

1. **Memory Flatline**:
   * `net/http` allocated 1,000 goroutines $\to$ memory jumped from 6MB to 38.8MB.
   * `epoll` uses **1 single thread**. Whether you have 10 connections or 10,000 connections, memory stays nearly flat because there are zero per-connection goroutine stacks!
2. **CPU Efficiency**:
   * Go runtime doesn't have to schedule and context-switch 1,000 goroutines across scheduler queues.
   * The kernel gives you the exact ready FDs, you service them sequentially, and you return to `epoll_wait()`.

Read through this theory, and when you are ready, dive into [`epoll-variant/main.go`](file:///home/ryuzaki/Desktop/Projects/uring-go/epoll-variant/main.go) to wire up the syscalls!

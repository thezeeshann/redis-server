# build-redis

A Redis server written from scratch in Go, with no dependencies outside the standard library. It speaks the real Redis wire protocol, so the official `redis-cli` connects to it and talks to it like any other Redis instance.

```
$ go run ./cmd
Listening on :6379

$ redis-cli SET name Zeeshan
OK
$ redis-cli GET name
"Zeeshan"
```

The point of the project is to understand what Redis actually *is* underneath: a TCP server, a text protocol, a hash map, and an append-only log.

---

## Running it

```bash
go run ./cmd             # start on :6379
redis-cli                # connect with the real Redis client
```

Run it from the project root — the AOF path is relative to the working
directory, so `database.aof` is created there.

Stop it with `Ctrl+C` — the server flushes the AOF to disk before exiting.
Note that `go run` does not forward the interrupt to the program it builds, so
the clean-shutdown path only runs for a compiled binary:

```bash
go build -o redis-server ./cmd && ./redis-server
```

If you get `bind: address already in use`, something else already holds port 6379 (often a real `redis-server`, or a leftover copy of this one):

```bash
lsof -i :6379 -sTCP:LISTEN    # see what it is
kill <pid>
```

### Output climbing across the screen

If `redis-cli` starts printing like this:

<!-- prettier-ignore -->
```
127.0.0.1:6379> ls
                  (error) ERR unknown command 'ls'
                                                  127.0.0.1:6379>
```

your terminal is stuck in raw mode — not a server problem. Interactive `redis-cli`
uses linenoise, which disables the terminal's `ONLCR` flag (the one that turns a
bare `\n` into `\r\n`) while reading a line, and restores it afterwards. If a
session dies before restoring it, every later newline moves down without
returning to column 0.

```bash
stty sane      # fixes it; `reset` if the prompt is garbled too
```

---

## How it fits together

```
build-redis/
├── cmd/
│   └── main.go              entry point: calls redis.ListenAndServe
├── internal/
│   └── redis/
│       ├── server.go        accept loop, dispatch, shutdown
│       ├── resp.go          the RESP protocol
│       ├── handler.go       commands + the in-memory dataset
│       └── aof.go           durability
└── database.aof             the log itself
```

The request path:

```
redis-cli
    │  TCP :6379
    ▼
server.go      accept loop, one goroutine per client
    │
    ▼
resp.go        decode bytes → Value, encode Value → bytes
    │
    ▼
handler.go     execute the command against the in-memory map
    │
    ▼
aof.go         append write-commands to database.aof
```

| File | Responsibility |
|------|----------------|
| `cmd/main.go` | Entry point — wires the address and AOF path together |
| `internal/redis/server.go` | TCP listener, connection handling, command dispatch, shutdown |
| `internal/redis/resp.go` | The RESP protocol — reading and writing |
| `internal/redis/handler.go` | Command implementations and the in-memory dataset |
| `internal/redis/aof.go` | Durability — the append-only file |
| `database.aof` | The log of every write command, replayed at startup |

### Why the server loop lives in the package, not in `main`

Go allows one package per directory, so `cmd/` and `internal/redis/` are
necessarily separate packages. `Value`'s fields (`typ`, `bulk`, `array`, …) are
lowercase, which means only code *inside* the package can touch them. Keeping
the connection loop in `main` would have forced every one of those fields to be
exported, so `main` could construct error replies.

Putting the loop in `internal/redis` instead keeps `Value` completely private
and leaves the package with one exported entry point:

```go
redis.ListenAndServe(":6379", "database.aof")
```

`internal/` is a name Go treats specially: nothing outside this module can
import it, so the whole thing stays an implementation detail.

---

## RESP — the protocol

**RESP** (REdis Serialization Protocol) is how the client and server talk. It is plain text, easy to read by eye, and easy to parse. Every message starts with one byte saying what type follows, and every part ends with `\r\n`.

| Byte | Type | Example on the wire | Means |
|------|------|--------------------|-------|
| `+` | Simple string | `+OK\r\n` | success |
| `-` | Error | `-ERR unknown command\r\n` | failure |
| `:` | Integer | `:1\r\n` | a number |
| `$` | Bulk string | `$7\r\nZeeshan\r\n` | binary-safe string, length first |
| `*` | Array | `*2\r\n...\r\n` | a list, count first |

### A command is just an array of bulk strings

When you type `SET name Zeeshan`, `redis-cli` sends:

```
*3\r\n$3\r\nSET\r\n$4\r\nname\r\n$7\r\nZeeshan\r\n
```

Read it in pieces:

```
*3          an array of 3 items
$3 SET      a 3-character string, "SET"
$4 name     a 4-character string, "name"
$7 Zeeshan  a 7-character string, "Zeeshan"
```

The server replies `+OK\r\n`.

Bulk strings send their **length up front** rather than using a terminator. That is what makes them *binary-safe*: your value can contain `\r`, `\n`, or a null byte and nothing gets confused, because the parser was told exactly how many bytes to read.

### How the code models it

Everything decoded or encoded is a single `Value` struct (`resp.go`):

```go
type Value struct {
    typ   string  // "array" | "bulk" | "string" | "integer" | "error" | "null"
    str   string  // for simple strings and errors
    num   int     // for integers
    bulk  string  // for bulk strings
    array []Value // for arrays
}
```

- `Resp.Read()` reads the first byte, switches on the type, and recurses — `readArray` calls `Read` for each element, which may itself be an array.
- `Value.Marshal()` is the mirror image: it switches on `typ` and produces bytes.

Two details in there that matter more than they look:

- **`io.ReadFull` in `readBulk`.** A plain `Read` is allowed to return fewer bytes than you asked for. If it does, the rest of the value gets misread as the start of the next command, and the stream is silently corrupted from that point on.
- **One `Resp` per connection, not per command.** `bufio.Reader` reads ahead in chunks. Building a fresh one for every command throws away whatever it had already buffered, so pipelined commands vanish.

---

## AOF — how data survives a restart

Without persistence the dataset lives only in the `SETs` map, and a restart loses everything. **AOF (Append Only File)** is one of the two answers Redis offers, and the one implemented here.

**The idea: don't save the data, save the commands that produced it.**

Every command that changes state is appended to `database.aof`, in the same RESP format it arrived in. On startup the server reads the file top to bottom and re-executes every command, arriving at exactly the state it had before.

After `SET name Zeeshan` then `DEL name`, the file literally contains:

```
*3\r\n$3\r\nSET\r\n$4\r\nname\r\n$7\r\nZeeshan\r\n*2\r\n$3\r\nDEL\r\n$4\r\nname\r\n
```

Replay both and you correctly end up with no `name` key.

You can read the file yourself at any time:

```bash
od -c database.aof     # escaped, shows the \r\n
cat database.aof       # readable enough, since RESP is text
```

### Design decisions in `aof.go`

**Only write-commands are logged.** `GET` and `PING` change nothing, so replaying them would be wasted work. The set lives in `writeCommands` in `handler.go` — add a command there when you add one that mutates state.

**Append before applying.** The AOF write happens *before* the handler runs. Crash in between and you replay a command that never took effect the first time, which is harmless. The other order risks a change that is live in memory but missing from the log — real, silent data loss.

**Sync once a second.** Writing to a file does not put bytes on the disk; the OS buffers them. A background goroutine calls `Sync()` every second to force the flush, so a crash costs at most one second of writes. Redis calls this policy `everysec`, and it is the default there too. `Sync()` on every single write would be safer and much slower.

**Seek to the end after replay.** `bufio` reads ahead past what it hands back, so the file descriptor's offset is not where you think it is after a replay. Without an explicit seek to the end, the first new write lands in the middle of the file and destroys existing entries.

### What is `dump.rdb`?

RDB is Redis's *other* persistence mode: a periodic binary snapshot of the whole dataset, rather than a log of commands. It restarts faster and produces smaller files; AOF loses less data on a crash. Real Redis can run both. The `dump.rdb` sitting in this directory is a leftover from a real `redis-server` run and is not used by this project.

### The known limitation: the file only grows

`SET counter 1` a thousand times and the AOF holds a thousand entries, all but the last irrelevant. Real Redis solves this with **AOF rewrite**: periodically replacing the log with the shortest command sequence that reproduces the current state. Not implemented here — it is the natural next thing to build.

---

## Commands

| Command | Reply | Persisted |
|---------|-------|-----------|
| `PING [msg]` | `PONG`, or `msg` echoed back | no |
| `SET key value` | `OK` | **yes** |
| `GET key` | the value, or nil if absent | no |
| `DEL key [key ...]` | count of keys actually removed | **yes** |
| `COMMAND` | empty array | no |

```
$ redis-cli PING
PONG
$ redis-cli SET name Zeeshan
OK
$ redis-cli GET name
"Zeeshan"
$ redis-cli GET missing
(nil)
$ redis-cli DEL name missing
(integer) 1
```

`COMMAND` exists because `redis-cli` sends `COMMAND DOCS` on connect to build its tab-completion. An empty array is a valid "nothing to introspect" answer and keeps the client happy.

Unknown commands get a proper RESP error rather than silence:

```
$ redis-cli FLUSHALL
ERR unknown command 'FLUSHALL'
```

### Adding a command

1. Write `func mycmd(args []Value) Value` in `internal/redis/handler.go`.
2. Register it in the `Handlers` map.
3. If it changes state, add it to `writeCommands` too — forget this and it works until the first restart, then quietly loses data.

Validate your argument count and return `Value{typ: "error", ...}` on a mismatch, the way `set` and `get` do.

---

## Concurrency

Each client gets its own goroutine, so several can connect at once. Two things guard the shared state:

- `SETsMu` — an `RWMutex` around the map. Concurrent reads are fine; writes are exclusive. Without it, two simultaneous `SET`s are a data race, and Go's map will often just crash the process.
- `execMu` — held across *both* the AOF append and the handler call. This keeps the order of commands in the log identical to the order they were applied in memory. Real Redis gets this for free by being single-threaded.

Race detector:

```bash
go run -race ./cmd
```

---

## Ideas to build next

- **AOF rewrite / compaction** — the limitation described above; the most valuable addition.
- **`EXPIRE` and TTLs** — needs a clock and a key-eviction strategy, and forces a real question: what does a replayed AOF do with an expiry that has already passed?
- **Hashes** — `HSET` / `HGET` / `HGETALL`, a second map alongside `SETs`.
- **`INCR`** — read-modify-write, so it has to be atomic under the mutex.
- **Configurable port** — `:6379` is currently the `address` constant in `cmd/main.go`.
- **Tests** — `internal/redis/resp.go` is the easiest and highest-value place to start: feed it byte strings, assert on the `Value` you get back. Being in the same package, a test can reach `Value`'s unexported fields directly.

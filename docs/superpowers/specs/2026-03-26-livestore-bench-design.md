# LiveStore Local Profiling Benchmark — Design Spec

**Date:** 2026-03-26
**Goal:** Profile lock contention in the livestore, specifically whether the flush loop and cleanup loop block the Kafka consumer and query goroutines on `blocksMtx`.

---

## Background

The livestore has a central `blocksMtx` (`sync.RWMutex`) on each tenant instance that is held exclusively by:
- The flush loop (`writeHeadBlock` × N traces, then `headBlock.Flush()`) — every 5s
- The cleanup loop (`deleteOldBlocks`, file deletions) — every 5min
- The completion workers (`completeBlock`, pointer swap only) — as blocks complete

And held with RLock by:
- The Kafka consumer (`backpressure()`) — on every push
- Query goroutines (`iterateBlocks()`) — on every search

The question: do the exclusive lock holds cause measurable stalls for the consumer and queries?

---

## Approach

A Go benchmark (`BenchmarkLiveStore`) in the existing `livestore` package that:
1. Starts a real LiveStore (with fake Kafka, real WAL on disk)
2. Pushes fake traces via `consume()` in the benchmark loop (simulates Kafka consumer)
3. Runs query goroutines concurrently in the background (simulates query traffic)
4. Lets the real background goroutines (flush loop, cleanup loop, completion workers) run naturally
5. Captures mutex, block, and CPU profiles for analysis

---

## File

**`modules/livestore/bench_test.go`** — same package as livestore, so it can access unexported methods (`consume`, `cutOneInstanceToWal`, etc.)

---

## Components

### TestMain

Starts a pprof HTTP server on `localhost:6060` once before all tests/benchmarks run:

```go
func TestMain(m *testing.M) {
    go func() { _ = http.ListenAndServe("localhost:6060", nil) }()
    os.Exit(m.Run())
}
```

Import `_ "net/http/pprof"` to auto-register pprof handlers.

### Record Pool

At benchmark setup time, pre-build a pool of 1000 fake `*kgo.Record` objects:
- 1 tenant (`testTenantID`)
- Random 16-byte trace IDs
- `test.MakeTrace(5, id)` — 1 trace with 5 ResourceSpans batches
- Encoded via `ingest.Encode()` into Kafka record format

Pre-building avoids allocation noise inside the benchmark loop.

### BenchmarkLiveStore

```
Setup:
  - create tmpDir
  - defaultLiveStore(b, tmpDir)  — real LiveStore, fake Kafka, real WAL
  - build record pool (1000 records)
  - runtime.SetMutexProfileFraction(1)
  - runtime.SetBlockProfileRate(1)
  - start 4 query goroutines (SearchRecent loop, query="{}", last 5min window)
  - b.ResetTimer()

Loop (b.N iterations):
  - pick record from pool (index mod 1000)
  - liveStore.consume(ctx, fakeIter([record]), time.Now())

Teardown:
  - cancel query goroutines
  - stop LiveStore
```

Background goroutines (flush every 5s, cleanup every 5min, 2 completion workers) run naturally — no manual triggering needed.

### Query Goroutines

4 goroutines, each in a tight loop:
```go
liveStore.SearchRecent(ctx, &tempopb.SearchRequest{
    Query: "{}",
    Start: uint32(time.Now().Add(-5 * time.Minute).Unix()),
    End:   uint32(time.Now().Unix()),
})
```
Tenant ID injected via `user.InjectOrgID`. Loop exits when benchmark context is cancelled.

---

## Workload Parameters

| Parameter | Value | Rationale |
|-----------|-------|-----------|
| Tenants | 1 | Isolates lock behavior, minimal noise |
| Record pool size | 1000 | Avoids allocation noise, enough variety |
| Spans per trace | 5 batches | Representative size |
| Query goroutines | 4 | Enough to show RLock contention |
| Benchmark duration | 30s (`-benchtime=30s`) | Long enough for background loops to fire |

---

## Running

```bash
# Capture all profiles
go test -bench=BenchmarkLiveStore \
  -benchtime=30s \
  -mutexprofile=mutex.out \
  -blockprofile=block.out \
  -cpuprofile=cpu.out \
  ./modules/livestore/

# Analyze mutex contention (primary question)
go tool pprof mutex.out

# Analyze goroutine blocking (channels + mutex waits)
go tool pprof block.out

# Analyze CPU hotspots
go tool pprof cpu.out
```

Or interactively while the benchmark is running:
```bash
go tool pprof http://localhost:6060/debug/pprof/mutex?debug=1
```

---

## What to Look For

| Profile | Signal | Interpretation |
|---------|--------|----------------|
| `mutex.out` | `blocksMtx` high wait time | Flush/cleanup holding lock too long |
| `mutex.out` | `liveTracesMtx` high wait time | Write path bottleneck |
| `block.out` | `backpressure()` blocking | Kafka consumer stalling on lock |
| `block.out` | `iterateBlocks()` blocking | Query goroutines stalling on lock |
| `cpu.out` | `writeHeadBlock` hot | N lock/unlock cycles per flush costing CPU |

---

## Out of Scope

- Multi-tenant scaling (separate investigation)
- Real Kafka integration
- gRPC query interface (calling methods directly is sufficient)
- Fix/optimization of any identified issues (this spec is profiling only)

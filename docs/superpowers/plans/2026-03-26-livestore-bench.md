# LiveStore Profiling Benchmark — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `modules/livestore/bench_test.go` with a `BenchmarkLiveStore` that runs real push + query traffic concurrently so mutex/block/CPU profiles reveal whether `blocksMtx` contention stalls the Kafka consumer or queries.

**Architecture:** Single benchmark file in the `livestore` package (same package = access to unexported methods). `TestMain` starts a pprof HTTP server. The benchmark pre-builds a 1000-record pool, pushes via `consume()` in the loop, and fires 4 concurrent `SearchRecent` goroutines. Background loops (flush every 5s, cleanup every 5min) run naturally.

**Tech Stack:** Go `testing.B`, `runtime` mutex/block profiling, `net/http/pprof`, existing test helpers (`defaultConfig`, `liveStoreWithConfig`, `createRecordIter`, `testTenantID`), `pkg/util/test`, `pkg/ingest`.

---

## File Map

| Action | Path | Responsibility |
|--------|------|----------------|
| Create | `modules/livestore/bench_test.go` | All benchmark code: TestMain, record pool, BenchmarkLiveStore, query goroutines |

No existing files need modification.

---

### Task 1: Add TestMain with pprof HTTP server

**Files:**
- Create: `modules/livestore/bench_test.go`

> Note: `testTenantID`, `testRecordIter`, `createRecordIter`, `defaultConfig`, and `liveStoreWithConfig` are all defined in the same package in existing test files — no need to redefine them.

- [ ] **Step 1: Create the file with TestMain**

Create `modules/livestore/bench_test.go`:

```go
package livestore

import (
	"context"
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/grafana/dskit/services"
	"github.com/grafana/dskit/user"
	"github.com/grafana/tempo/pkg/ingest"
	"github.com/grafana/tempo/pkg/tempopb"
	"github.com/grafana/tempo/pkg/util/test"
	"github.com/gogo/protobuf/proto"
	"github.com/twmb/franz-go/pkg/kgo"
)

// TestMain starts a pprof HTTP server so you can attach interactively:
//
//	go tool pprof http://localhost:6060/debug/pprof/mutex
func TestMain(m *testing.M) {
	go func() {
		_ = http.ListenAndServe("localhost:6060", nil)
	}()
	os.Exit(m.Run())
}
```

- [ ] **Step 2: Verify it compiles**

```bash
cd /Users/xiaoguang/work/repo/grafana/tempo
go build ./modules/livestore/
```

Expected: no errors.

- [ ] **Step 3: Commit**

```bash
git add modules/livestore/bench_test.go
git commit -m "bench(livestore): add bench_test.go with pprof TestMain"
```

---

### Task 2: Add record pool builder

**Files:**
- Modify: `modules/livestore/bench_test.go`

- [ ] **Step 1: Add `buildRecordPool` function**

Append to `bench_test.go` (before the closing of the file):

```go
const benchPoolSize = 1000

// buildRecordPool pre-builds a pool of Kafka records to avoid allocation
// noise inside the benchmark loop. Each record is a fully-encoded
// PushBytesRequest with one trace of 5 ResourceSpans batches.
func buildRecordPool(b *testing.B) []*kgo.Record {
	b.Helper()
	now := time.Now()
	pool := make([]*kgo.Record, 0, benchPoolSize)
	for range benchPoolSize {
		id := test.ValidTraceID(nil)
		tr := test.MakeTrace(5, id)
		traceBytes, err := proto.Marshal(tr)
		if err != nil {
			b.Fatalf("marshal trace: %v", err)
		}
		req := &tempopb.PushBytesRequest{
			Traces: []tempopb.PreallocBytes{{Slice: traceBytes}},
			Ids:    [][]byte{id},
		}
		records, err := ingest.Encode(0, testTenantID, req, 1_000_000)
		if err != nil {
			b.Fatalf("encode record: %v", err)
		}
		for _, r := range records {
			r.Timestamp = now
			pool = append(pool, r)
		}
	}
	return pool
}
```

- [ ] **Step 2: Verify it compiles**

```bash
cd /Users/xiaoguang/work/repo/grafana/tempo
go build ./modules/livestore/
```

Expected: no errors.

- [ ] **Step 3: Commit**

```bash
git add modules/livestore/bench_test.go
git commit -m "bench(livestore): add record pool builder"
```

---

### Task 3: Add BenchmarkLiveStore with push loop

**Files:**
- Modify: `modules/livestore/bench_test.go`

- [ ] **Step 1: Add `BenchmarkLiveStore`**

Append to `bench_test.go`:

```go
// BenchmarkLiveStore measures throughput and lock contention under concurrent
// push + query traffic. Run with:
//
//	go test -bench=BenchmarkLiveStore -benchtime=30s \
//	  -mutexprofile=mutex.out -blockprofile=block.out -cpuprofile=cpu.out \
//	  ./modules/livestore/
func BenchmarkLiveStore(b *testing.B) {
	// Enable profiling of every mutex and block event.
	runtime.SetMutexProfileFraction(1)
	runtime.SetBlockProfileRate(1)

	tmpDir := b.TempDir()

	// Use defaultConfig but enable background goroutines (flush, cleanup,
	// completion workers). defaultConfig sets holdAllBackgroundProcesses=true
	// for deterministic unit tests — we need it false here.
	cfg := defaultConfig(b, tmpDir)
	cfg.holdAllBackgroundProcesses = false

	ls, err := liveStoreWithConfig(b, cfg)
	if err != nil {
		b.Fatalf("start livestore: %v", err)
	}
	b.Cleanup(func() {
		_ = services.StopAndAwaitTerminated(context.Background(), ls)
	})

	pool := buildRecordPool(b)

	b.ResetTimer()

	for i := range b.N {
		rec := pool[i%benchPoolSize]
		if _, err := ls.consume(b.Context(), createRecordIter([]*kgo.Record{rec}), rec.Timestamp); err != nil {
			b.Fatalf("consume: %v", err)
		}
	}
}
```

- [ ] **Step 2: Run the benchmark briefly to confirm it works**

```bash
cd /Users/xiaoguang/work/repo/grafana/tempo
go test -bench=BenchmarkLiveStore -benchtime=3s -run='^$' ./modules/livestore/
```

Expected: output like:
```
BenchmarkLiveStore-N    XXXX    XXXX ns/op
```
No `FAIL` or panics.

- [ ] **Step 3: Commit**

```bash
git add modules/livestore/bench_test.go
git commit -m "bench(livestore): add BenchmarkLiveStore push loop"
```

---

### Task 4: Add concurrent query goroutines

**Files:**
- Modify: `modules/livestore/bench_test.go`

- [ ] **Step 1: Add `startQueryWorkers` helper and wire it into `BenchmarkLiveStore`**

Append the helper to `bench_test.go`:

```go
// startQueryWorkers launches n goroutines that each fire SearchRecent in a
// tight loop until ctx is cancelled. This simulates concurrent query traffic
// while the benchmark push loop runs.
func startQueryWorkers(ctx context.Context, b *testing.B, ls *LiveStore, n int) {
	b.Helper()
	userCtx := user.InjectOrgID(ctx, testTenantID)
	for range n {
		go func() {
			req := &tempopb.SearchRequest{
				Query: "{}",
			}
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				req.Start = uint32(time.Now().Add(-5 * time.Minute).Unix())
				req.End = uint32(time.Now().Unix())
				_, _ = ls.SearchRecent(userCtx, req)
			}
		}()
	}
}
```

Then edit `BenchmarkLiveStore` — replace the line `b.ResetTimer()` and the for-loop with:

```go
	// Start query workers before the timer so they're running concurrently
	// with the push loop throughout the benchmark.
	queryCtx, cancelQueries := context.WithCancel(context.Background())
	b.Cleanup(cancelQueries)
	startQueryWorkers(queryCtx, b, ls, 4)

	b.ResetTimer()

	for i := range b.N {
		rec := pool[i%benchPoolSize]
		if _, err := ls.consume(b.Context(), createRecordIter([]*kgo.Record{rec}), rec.Timestamp); err != nil {
			b.Fatalf("consume: %v", err)
		}
	}
```

- [ ] **Step 2: Run the benchmark briefly again to confirm query workers don't crash it**

```bash
cd /Users/xiaoguang/work/repo/grafana/tempo
go test -bench=BenchmarkLiveStore -benchtime=3s -run='^$' ./modules/livestore/
```

Expected: same as before — benchmark completes, no panics.

- [ ] **Step 3: Commit**

```bash
git add modules/livestore/bench_test.go
git commit -m "bench(livestore): add concurrent query goroutines"
```

---

### Task 5: Run full profiling and verify outputs

**Files:** none — this task is execution only.

- [ ] **Step 1: Run the full 30s benchmark with all profiles**

```bash
cd /Users/xiaoguang/work/repo/grafana/tempo
go test -bench=BenchmarkLiveStore \
  -benchtime=30s \
  -run='^$' \
  -mutexprofile=mutex.out \
  -blockprofile=block.out \
  -cpuprofile=cpu.out \
  ./modules/livestore/
```

Expected: benchmark completes in ~30s, three `.out` files created in the current directory.

- [ ] **Step 2: Inspect mutex contention**

```bash
go tool pprof -top mutex.out
```

Look for `blocksMtx` or `liveTracesMtx` in the output. High cumulative time means those locks are stalling goroutines.

- [ ] **Step 3: Inspect goroutine blocking**

```bash
go tool pprof -top block.out
```

Look for `backpressure` (Kafka consumer blocked) or `iterateBlocks` (queries blocked).

- [ ] **Step 4: Inspect CPU hotspots**

```bash
go tool pprof -top cpu.out
```

Look for `writeHeadBlock` — if it's hot, the N-locks-per-flush pattern is burning CPU.

- [ ] **Step 5: Optional — interactive profile while benchmark runs**

In one terminal:
```bash
go test -bench=BenchmarkLiveStore -benchtime=120s -run='^$' ./modules/livestore/
```

In a second terminal while the first is running:
```bash
go tool pprof http://localhost:6060/debug/pprof/mutex
```

At the pprof prompt: `top10` then `web` (requires graphviz) for a flame-graph-style view.

---

## Quick Reference

```bash
# Full profiling run
go test -bench=BenchmarkLiveStore -benchtime=30s -run='^$' \
  -mutexprofile=mutex.out -blockprofile=block.out -cpuprofile=cpu.out \
  ./modules/livestore/

# Top mutex holders
go tool pprof -top mutex.out

# Top blockers
go tool pprof -top block.out

# Top CPU consumers
go tool pprof -top cpu.out
```

## What to Look For

| Profile | Signal | Interpretation |
|---------|--------|----------------|
| `mutex.out` | `blocksMtx` high cumulative wait | Flush/cleanup holding lock too long |
| `mutex.out` | `liveTracesMtx` high cumulative wait | Write path bottleneck |
| `block.out` | `backpressure` blocking | Kafka consumer stalling on lock |
| `block.out` | `iterateBlocks` blocking | Query goroutines stalling |
| `cpu.out` | `writeHeadBlock` in top 10 | N lock/unlock cycles per flush costing CPU |

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

// makeRecord builds a single Kafka record with a fresh trace ID, span IDs,
// and current timestamp on every call.
func makeRecord(b *testing.B) *kgo.Record {
	b.Helper()
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
	records[0].Timestamp = time.Now()
	return records[0]
}

// TestMain starts a pprof HTTP server so you can attach interactively:
//
//	go tool pprof http://localhost:6060/debug/pprof/mutex
func TestMain(m *testing.M) {
	go func() {
		_ = http.ListenAndServe("localhost:6060", nil)
	}()
	os.Exit(m.Run())
}

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

	cfg := defaultConfig(b, tmpDir)
	cfg.holdAllBackgroundProcesses = false

	ls, err := liveStoreWithConfig(b, cfg)
	if err != nil {
		b.Fatalf("start livestore: %v", err)
	}
	b.Cleanup(func() {
		_ = services.StopAndAwaitTerminated(context.Background(), ls)
	})

	// Start query workers before the timer so they're running concurrently
	// with the push loop throughout the benchmark.
	queryCtx, cancelQueries := context.WithCancel(context.Background())
	b.Cleanup(cancelQueries)
	startQueryWorkers(queryCtx, b, ls, 4)

	b.ResetTimer()

	for range b.N {
		// Build a fresh record (unique trace ID, span IDs, timestamp) outside
		// the timed region so allocation noise doesn't skew ns/op.
		b.StopTimer()
		rec := makeRecord(b)
		b.StartTimer()

		if _, err := ls.consume(b.Context(), createRecordIter([]*kgo.Record{rec}), rec.Timestamp); err != nil {
			b.Fatalf("consume: %v", err)
		}
	}
}

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

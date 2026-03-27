package livestore

import (
	"context"
	"flag"
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/go-kit/log"
	"github.com/grafana/dskit/kv/consul"
	"github.com/grafana/dskit/ring"
	"github.com/grafana/dskit/services"
	"github.com/grafana/dskit/user"
	"github.com/grafana/tempo/pkg/ingest"
	"github.com/grafana/tempo/pkg/tempopb"
	"github.com/grafana/tempo/pkg/util/test"
	"github.com/grafana/tempo/tempodb/encoding"
	"github.com/gogo/protobuf/proto"
	"github.com/twmb/franz-go/pkg/kgo"
)

// TestMain starts a pprof HTTP server so you can attach interactively:
//
//	go tool pprof http://localhost:6060/debug/pprof/mutex
func TestMain(m *testing.M) {
	http.Handle("/metrics", promhttp.Handler())
	go func() {
		_ = http.ListenAndServe("localhost:6060", nil)
	}()
	os.Exit(m.Run())
}

// profilingConfig returns a minimal Config: no Kafka cluster, background
// processes enabled. consume() is called directly so Kafka is not needed.
func profilingConfig(t testing.TB, tmpDir string) Config {
	t.Helper()
	cfg := Config{}
	cfg.RegisterFlagsAndApplyDefaults("", flag.NewFlagSet("", flag.ContinueOnError))
	cfg.WAL.Filepath = tmpDir
	cfg.WAL.Version = encoding.LatestEncoding().Version()
	cfg.ShutdownMarkerDir = tmpDir
	cfg.BlockConfig.RegisterFlagsAndApplyDefaults("", flag.NewFlagSet("", flag.ContinueOnError))
	cfg.BlockConfig.Version = encoding.LatestEncoding().Version()
	cfg.Ring.RegisterFlagsAndApplyDefaults("", flag.NewFlagSet("", flag.ContinueOnError))
	cfg.Ring.ListenPort = 0
	cfg.Ring.InstanceAddr = "localhost"
	cfg.Ring.InstanceID = "test-1"

	mockPartitionStore, _ := consul.NewInMemoryClient(ring.GetPartitionRingCodec(), log.NewNopLogger(), nil)
	mockStore, _ := consul.NewInMemoryClient(ring.GetCodec(), log.NewNopLogger(), nil)
	cfg.Ring.KVStore.Mock = mockStore
	cfg.PartitionRing.KVStore.Mock = mockPartitionStore

	cfg.holdAllBackgroundProcesses = false
	cfg.skipKafka = true
	return cfg
}

// newRecordTemplate builds one Kafka record with a fixed payload (30 batches ×
// 50 spans, ~300 KB). The Value bytes are reused across all records to avoid
// repeated crypto/rand span-ID generation and proto marshalling.
func newRecordTemplate(t testing.TB) *kgo.Record {
	t.Helper()
	id := test.ValidTraceID(nil)
	tr := test.MakeTraceWithSpanCount(30, 50, id)
	traceBytes, err := proto.Marshal(tr)
	if err != nil {
		t.Fatalf("marshal trace: %v", err)
	}
	req := &tempopb.PushBytesRequest{
		Traces: []tempopb.PreallocBytes{{Slice: traceBytes}},
		Ids:    [][]byte{id},
	}
	records, err := ingest.Encode(0, testTenantID, req, 1_000_000)
	if err != nil {
		t.Fatalf("encode record: %v", err)
	}
	return records[0]
}

// makeRecords generates a fresh template per record (unique trace ID + span IDs)
// and stamps each with *ts. Advances *ts by 1s on each call.
func makeRecords(t testing.TB, dst []*kgo.Record, ts *time.Time) {
	t.Helper()
	for i := range dst {
		tmpl := newRecordTemplate(t)
		dst[i] = &kgo.Record{
			Key:       tmpl.Key,
			Value:     tmpl.Value,
			Topic:     tmpl.Topic,
			Partition: tmpl.Partition,
			Timestamp: *ts,
		}
	}
	*ts = ts.Add(time.Second)
}

// TestLiveStoreProfiling runs push + query traffic for a fixed duration so
// mutex/block/CPU profiles capture real contention without benchmark
// calibration overhead. Run with:
//
//	go test -v -run=TestLiveStoreProfiling -timeout=15m \
//	  -mutexprofile=mutex.out -blockprofile=block.out -cpuprofile=cpu.out \
//	  ./modules/livestore/
//
// To change the duration pass -profiling-duration=5m (default 10m).
var profilingDuration = flag.Duration("profiling-duration", 10*time.Minute, "how long TestLiveStoreProfiling runs")
var profilingBatchSize = flag.Int("profiling-batch-size", 100, "records per consume call in TestLiveStoreProfiling")
var profilingQueries = flag.Bool("profiling-queries", true, "run query workers alongside writes in TestLiveStoreProfiling")
var profilingQueryCount = flag.Int("profiling-query-count", 0, "number of queries per profiling-query-interval (0 = unlimited)")
var profilingQueryInterval = flag.Duration("profiling-query-interval", 10*time.Second, "interval for profiling-query-count")

func TestLiveStoreProfiling(t *testing.T) {
	if *profilingDuration == 0 {
		t.Skip("profiling-duration=0, skipping")
	}

	runtime.SetMutexProfileFraction(1)
	runtime.SetBlockProfileRate(1)

	tmpDir := t.TempDir()
	cfg := profilingConfig(t, tmpDir)

	ls, err := liveStoreWithConfig(t, cfg)
	if err != nil {
		t.Fatalf("start livestore: %v", err)
	}
	t.Cleanup(func() {
		_ = services.StopAndAwaitTerminated(context.Background(), ls)
	})

	if *profilingQueries {
		queryCtx, cancelQueries := context.WithCancel(context.Background())
		t.Cleanup(cancelQueries)
		startQueryWorkers(queryCtx, t, ls, 4, *profilingQueryCount, *profilingQueryInterval)
	}

	t.Logf("running for %s, batch size %d", *profilingDuration, *profilingBatchSize)
	recs := make([]*kgo.Record, *profilingBatchSize)
	ts := time.Now()
	deadline := ts.Add(*profilingDuration)
	for time.Now().Before(deadline) {
		makeRecords(t, recs, &ts)
		now := recs[len(recs)-1].Timestamp
		if _, err := ls.consume(t.Context(), createRecordIter(recs), now); err != nil {
			t.Fatalf("consume: %v", err)
		}
	}
	t.Logf("done")
}

// startQueryWorkers launches n goroutines that each fire SearchRecent until
// ctx is cancelled. count/interval limits total queries across all workers
// (count=0 = unlimited). QPS is logged every 10s.
func startQueryWorkers(ctx context.Context, t testing.TB, ls *LiveStore, n int, count int, interval time.Duration) {
	t.Helper()
	var total atomic.Int64

	// Build a shared token channel when rate limiting is requested.
	// A feeder goroutine emits one token every interval/count; workers consume one per query.
	var tokens <-chan struct{}
	if count > 0 {
		ch := make(chan struct{}, count)
		tokens = ch
		go func() {
			ticker := time.NewTicker(interval / time.Duration(count))
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					select {
					case ch <- struct{}{}:
					default: // drop if workers are behind
					}
				}
			}
		}()
	}

	userCtx := user.InjectOrgID(ctx, testTenantID)
	for range n {
		go func() {
			req := &tempopb.SearchRequest{Query: "{}"}
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				if tokens != nil {
					select {
					case <-ctx.Done():
						return
					case <-tokens:
					}
				}
				req.Start = uint32(time.Now().Add(-5 * time.Minute).Unix())
				req.End = uint32(time.Now().Unix())
				_, _ = ls.SearchRecent(userCtx, req)
				total.Add(1)
			}
		}()
	}
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		var last int64
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cur := total.Load()
				t.Logf("query QPS: %d", (cur-last)/10)
				last = cur
			}
		}
	}()
}

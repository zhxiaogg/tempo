package vcg1_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/google/uuid"
	"github.com/parquet-go/parquet-go"
	"github.com/stretchr/testify/require"

	"github.com/grafana/tempo/v3/pkg/tempopb"
	"github.com/grafana/tempo/v3/tempodb/backend"
	"github.com/grafana/tempo/v3/tempodb/backend/local"
	"github.com/grafana/tempo/v3/tempodb/encoding"
	"github.com/grafana/tempo/v3/tempodb/encoding/common"
	"github.com/grafana/tempo/v3/tempodb/encoding/vcg1"
	"github.com/grafana/tempo/v3/tempodb/encoding/vparquet5"
)

// TestZZWriteBench compares the write cost of vParquet5 and vCG1 on the same traces:
// CreateBlock from proto traces (the block-builder path), then a 4-to-1 compaction.
//
//	WB_SRC=<vp5 block dir> WB_DST=<scratch dir> WB_N=<traces> go test -run TestZZWriteBench -v -timeout 2h
func TestZZWriteBench(t *testing.T) {
	src, dst := os.Getenv("WB_SRC"), os.Getenv("WB_DST")
	if src == "" || dst == "" {
		t.Skip()
	}
	n, _ := strconv.Atoi(os.Getenv("WB_N"))
	if n <= 0 {
		n = 300000
	}
	ctx := context.Background()

	srcMeta, ids, traces := loadTraces(t, src, n)
	t.Logf("loaded %d traces", len(traces))

	rr, rw, _, err := local.New(&local.Config{Path: dst})
	require.NoError(t, err)
	r, w := backend.NewReader(rr), backend.NewWriter(rw)

	cfg := &common.BlockConfig{BloomFP: common.DefaultBloomFP, BloomShardSizeBytes: common.DefaultBloomShardSizeBytes, RowGroupSizeBytes: 500_000_000}

	versions := []string{vparquet5.VersionString, vcg1.VersionString}
	if v := os.Getenv("WB_ONLY"); v != "" {
		versions = []string{v}
	}
	for _, v := range versions {
		enc, err := encoding.FromVersion(v)
		require.NoError(t, err)
		c := *cfg
		c.Version = v

		// CreateBlock: every trace into one block.
		var m *backend.BlockMeta
		st := measure(func() {
			m, err = enc.CreateBlock(ctx, &c, newMeta(srcMeta, v, len(traces)), &sliceIter{ids: ids, trs: traces}, r, w)
		})
		require.NoError(t, err)
		t.Logf("%-10s create : %s  size=%d MiB  traces=%d", v, st, m.Size_>>20, m.TotalObjects)

		// Compaction: 4 inputs, each a quarter of the traces taken round-robin (so every input
		// spans the whole id space, as block-builder blocks of one window do), merged to one.
		var inputs []*backend.BlockMeta
		for q := 0; q < 4; q++ {
			var qi, qt = []common.ID{}, []*tempopb.Trace{}
			for i := q; i < len(traces); i += 4 {
				qi, qt = append(qi, ids[i]), append(qt, traces[i])
			}
			im, err := enc.CreateBlock(ctx, &c, newMeta(srcMeta, v, len(qt)), &sliceIter{ids: qi, trs: qt}, r, w)
			require.NoError(t, err)
			inputs = append(inputs, im)
		}
		var out []*backend.BlockMeta
		prof := os.Getenv("WB_PROF")
		if prof != "" {
			writeAllocs(t, prof+"/"+v+"-before.pb.gz")
		}
		st = measure(func() {
			out, err = enc.NewCompactor(common.CompactionOptions{BlockConfig: c, OutputBlocks: 1}).Compact(ctx, log.NewNopLogger(), r, w, inputs)
		})
		require.NoError(t, err)
		if prof != "" {
			writeAllocs(t, prof+"/"+v+"-after.pb.gz")
		}
		t.Logf("%-10s compact: %s  out=%d MiB", v, st, out[0].Size_>>20)
	}
}

func writeAllocs(t *testing.T, path string) {
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()
	require.NoError(t, pprof.Lookup("allocs").WriteTo(f, 0))
}

type stats struct {
	wall, cpu time.Duration
	peakHeap  uint64
	alloc     uint64
}

func (s stats) String() string {
	return "wall=" + s.wall.Round(10*time.Millisecond).String() + " cpu=" + s.cpu.Round(10*time.Millisecond).String() +
		" peakHeap=" + strconv.FormatUint(s.peakHeap>>20, 10) + "MiB alloc=" + strconv.FormatUint(s.alloc>>20, 10) + "MiB"
}

// measure runs f and reports its wall and process CPU time, the peak heap in use above the
// heap before it (sampled every 20ms) and the bytes it allocated.
func measure(f func()) stats {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base, alloc0 := ms.HeapInuse, ms.TotalAlloc
	var peak atomic.Uint64
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tk := time.NewTicker(20 * time.Millisecond)
		defer tk.Stop()
		var m runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				runtime.ReadMemStats(&m)
				if m.HeapInuse > peak.Load() {
					peak.Store(m.HeapInuse)
				}
			}
		}
	}()
	cpu0 := cpuTime()
	t0 := time.Now()
	f()
	s := stats{wall: time.Since(t0), cpu: cpuTime() - cpu0}
	close(stop)
	<-done
	runtime.ReadMemStats(&ms)
	if p := peak.Load(); p > base {
		s.peakHeap = p - base
	}
	s.alloc = ms.TotalAlloc - alloc0
	return s
}

func cpuTime() time.Duration {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

func newMeta(src *backend.BlockMeta, v string, n int) *backend.BlockMeta {
	m := backend.NewBlockMeta(src.TenantID, uuid.New(), v)
	m.StartTime, m.EndTime = src.StartTime, src.EndTime
	m.DedicatedColumns = src.DedicatedColumns
	m.TotalObjects = int64(n)
	m.ReplicationFactor = 1
	return m
}

// loadTraces reads the first n traces of a vParquet5 block as proto traces.
func loadTraces(t *testing.T, dir string, n int) (*backend.BlockMeta, []common.ID, []*tempopb.Trace) {
	raw, _, _, err := local.New(&local.Config{Path: filepath.Dir(filepath.Dir(dir))})
	require.NoError(t, err)
	meta, err := backend.NewReader(raw).BlockMeta(context.Background(), uuid.MustParse(filepath.Base(dir)), filepath.Base(filepath.Dir(dir)))
	require.NoError(t, err)

	f, err := os.Open(filepath.Join(dir, vparquet5.DataFileName))
	require.NoError(t, err)
	defer f.Close()
	st, err := f.Stat()
	require.NoError(t, err)
	pf, err := parquet.OpenFile(f, st.Size(), parquet.SkipBloomFilters(true), parquet.SkipPageIndex(true))
	require.NoError(t, err)
	_, _, readerOpts := vparquet5.SchemaWithDynamicChanges(meta.DedicatedColumns)
	rd := parquet.NewGenericReader[*vparquet5.Trace](pf, readerOpts...)
	defer rd.Close()

	ids := make([]common.ID, 0, n)
	trs := make([]*tempopb.Trace, 0, n)
	buf := make([]*vparquet5.Trace, 64)
	for len(trs) < n {
		k, err := rd.Read(buf)
		for _, row := range buf[:k] {
			if len(trs) == n {
				break
			}
			ids = append(ids, append(common.ID(nil), row.TraceID...))
			trs = append(trs, vparquet5.ParquetTraceToTempopbTrace(meta, row))
		}
		clear(buf)
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
	}
	return meta, ids, trs
}

type sliceIter struct {
	ids []common.ID
	trs []*tempopb.Trace
	i   int
}

func (s *sliceIter) Next(context.Context) (common.ID, *tempopb.Trace, error) {
	if s.i == len(s.trs) {
		return nil, nil, io.EOF
	}
	s.i++
	return s.ids[s.i-1], s.trs[s.i-1], nil
}

func (s *sliceIter) Close() {}

// zz-cgcompact checks the vCG1 compactor against vparquet5's on real data (test tooling for the
// CG format, not part of Tempo).
//
//	zz-cgcompact -mode split -src <vp5 block dir> [-dump rows.gob.zst] -dst <root> -k 4
//	    Cuts the source block into k overlapping inputs, written twice: as vParquet5 blocks and
//	    as vCG1 blocks of the same rows. Trace i goes to input i%k; every 10th trace (i%10==3)
//	    is split into two partial traces in inputs i%k and (i+1)%k (its spans alternate, every
//	    4th span is in both), every 10th (i%10==7) is copied whole into both. Writes
//	    <dst>/split.json (the block ids) and <dst>/multi_ids.txt (the ids in two inputs).
//	zz-cgcompact -mode compact -root <root> -blocks id,id,... [-out-blocks 1] [-max-bytes n] [-report f]
//	    Compacts the blocks with the compactor of their version, with every callback recorded;
//	    reports wall time, peak heap, max RSS and the callbacks (JSON).
//	zz-cgcompact -mode compare -root <root> -vp id,... -cg id,... [-multi multi_ids.txt] [-sample n] [-ids-out f]
//	    Compares the two compactions' outputs: every trace in order (vParquet5 rows read with
//	    parquet-go, vCG1 rows with cgfile.TraceIterator, both as vparquet5 returns them), then
//	    FindTraceByID on a sample of ids (half of them from -multi) through Tempo's per-block
//	    path; -ids-out writes the sample as a suite sample_ids file.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-kit/log"
	"github.com/gogo/protobuf/proto"
	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
	"github.com/parquet-go/parquet-go"

	"github.com/grafana/tempo/v3/pkg/tempopb"
	v1 "github.com/grafana/tempo/v3/pkg/tempopb/common/v1"
	v1_trace "github.com/grafana/tempo/v3/pkg/tempopb/trace/v1"
	"github.com/grafana/tempo/v3/tempodb/backend"
	"github.com/grafana/tempo/v3/tempodb/backend/local"
	"github.com/grafana/tempo/v3/tempodb/encoding"
	"github.com/grafana/tempo/v3/tempodb/encoding/common"
	"github.com/grafana/tempo/v3/tempodb/encoding/vcg1"
	cg "github.com/grafana/tempo/v3/tempodb/encoding/vcg1/cgfile"
	"github.com/grafana/tempo/v3/tempodb/encoding/vparquet5"
)

func main() {
	var (
		mode     = flag.String("mode", "", "split | compact | compare")
		src      = flag.String("src", "", "split: vParquet5 source block directory (<root>/<tenant>/<uuid>)")
		dumpPath = flag.String("dump", "", "split: read the source rows from this zz-cgwrite dump")
		dst      = flag.String("dst", "", "split: root directory of the inputs")
		k        = flag.Int("k", 4, "split: number of inputs")
		root     = flag.String("root", "", "compact, compare: backend root directory")
		tenant   = flag.String("tenant", "3", "tenant")
		blocks   = flag.String("blocks", "", "compact: comma-separated input block ids")
		outBlks  = flag.Int("out-blocks", 1, "compact: CompactionOptions.OutputBlocks")
		maxBytes = flag.Int("max-bytes", 0, "compact: CompactionOptions.MaxBytesPerTrace")
		report   = flag.String("report", "", "compact: write the JSON report here")
		vpIDs    = flag.String("vp", "", "compare: vParquet5 output block ids (in order)")
		cgIDs    = flag.String("cg", "", "compare: vCG1 output block ids (in order)")
		multi    = flag.String("multi", "", "compare: multi_ids.txt from split")
		sample   = flag.Int("sample", 4000, "compare: FindTraceByID sample size")
		seed     = flag.Int64("seed", 1, "compare: sample seed")
		idsOut   = flag.String("ids-out", "", "compare: write the sample ids here ([{id}])")
	)
	flag.IntVar(&vcg1.IterWindowSpans, "window", vcg1.IterWindowSpans, "compact: vCG1 input window (spans)")
	flag.StringVar(&heapProf, "heapprof", "", "compact: write a heap profile here at the peak")
	flag.StringVar(&cpuProf, "cpuprof", "", "compact: write a CPU profile here")
	flag.Parse()
	var err error
	switch *mode {
	case "split":
		err = split(*src, *dumpPath, *dst, *k)
	case "compact":
		err = compact(*root, *tenant, strings.Split(*blocks, ","), *outBlks, *maxBytes, *report)
	case "sizes":
		err = sizes(*root, *tenant, *blocks)
	case "compare":
		err = compare(*root, *tenant, strings.Split(*vpIDs, ","), strings.Split(*cgIDs, ","), *multi, *sample, *seed, *idsOut)
	default:
		err = errors.New("-mode split|compact|compare")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

var heapProf, cpuProf string

func openBackend(root string) (backend.Reader, backend.Writer, error) {
	rr, ww, _, err := local.New(&local.Config{Path: root})
	if err != nil {
		return nil, nil, err
	}
	return backend.NewReader(rr), backend.NewWriter(ww), nil
}

func blockAt(dir string) (backend.Reader, *backend.BlockMeta, error) {
	dir = filepath.Clean(dir)
	r, _, err := openBackend(filepath.Dir(filepath.Dir(dir)))
	if err != nil {
		return nil, nil, err
	}
	id, err := uuid.Parse(filepath.Base(dir))
	if err != nil {
		return nil, nil, err
	}
	m, err := r.BlockMeta(context.Background(), id, filepath.Base(filepath.Dir(dir)))
	return r, m, err
}

// ---------------------------------------------------------------- source rows

type rowSource func() (*vparquet5.Trace, error)

// parquetRows reads the rows of a vParquet5 data.parquet in order (fresh objects).
func parquetRows(dir string, dc backend.DedicatedColumns) (rowSource, func(), error) {
	f, err := os.Open(filepath.Join(dir, vparquet5.DataFileName))
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	pf, err := parquet.OpenFile(f, st.Size(), parquet.SkipBloomFilters(true), parquet.SkipPageIndex(true))
	if err != nil {
		return nil, nil, err
	}
	_, _, readerOpts := vparquet5.SchemaWithDynamicChanges(dc)
	r := parquet.NewGenericReader[*vparquet5.Trace](pf, readerOpts...)
	buf := make([]*vparquet5.Trace, 64)
	pos, n := 0, 0
	next := func() (*vparquet5.Trace, error) {
		if pos == n {
			for i := range buf {
				buf[i] = &vparquet5.Trace{}
			}
			m, err := r.Read(buf)
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			if m == 0 {
				return nil, nil
			}
			pos, n = 0, m
		}
		t := buf[pos]
		buf[pos] = nil
		pos++
		return t, nil
	}
	return next, func() { _ = r.Close(); _ = f.Close() }, nil
}

type dumpRec struct {
	RG bool
	T  *vparquet5.Trace
}

func dumpRows(path string) (rowSource, func(), error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	zr, err := zstd.NewReader(f, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))
	if err != nil {
		return nil, nil, err
	}
	dec := gob.NewDecoder(bufio.NewReaderSize(zr, 1<<20))
	next := func() (*vparquet5.Trace, error) {
		var r dumpRec
		if err := dec.Decode(&r); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, nil
			}
			return nil, err
		}
		return r.T, nil
	}
	return next, func() { zr.Close(); f.Close() }, nil
}

// ---------------------------------------------------------------- split

// chanRows feeds one CreateBlock from a channel.
type chanRows struct{ ch <-chan *vparquet5.Trace }

func (c chanRows) NextRow(context.Context) (*vparquet5.Trace, error) {
	r, ok := <-c.ch
	if !ok {
		return nil, nil
	}
	return r, nil
}
func (c chanRows) Next(context.Context) (common.ID, *tempopb.Trace, error) { return nil, nil, io.EOF }
func (c chanRows) Close()                                                  {}

type splitManifest struct {
	VP []string `json:"vp"`
	CG []string `json:"cg"`
}

// halves returns two partial traces of tr: spans alternate between them, every 4th span is in
// both.
func halves(tr *tempopb.Trace) [2]*tempopb.Trace {
	var out [2]*tempopb.Trace
	for p := range out {
		out[p] = &tempopb.Trace{}
	}
	n := 0
	for _, rs := range tr.ResourceSpans {
		var prs [2]*v1_trace.ResourceSpans
		for _, ss := range rs.ScopeSpans {
			var pss [2]*v1_trace.ScopeSpans
			for _, sp := range ss.Spans {
				dst := []int{n % 2}
				if n%4 == 0 {
					dst = []int{0, 1}
				}
				n++
				for _, p := range dst {
					if prs[p] == nil {
						prs[p] = &v1_trace.ResourceSpans{Resource: rs.Resource}
						out[p].ResourceSpans = append(out[p].ResourceSpans, prs[p])
					}
					if pss[p] == nil {
						pss[p] = &v1_trace.ScopeSpans{Scope: ss.Scope}
						prs[p].ScopeSpans = append(prs[p].ScopeSpans, pss[p])
					}
					pss[p].Spans = append(pss[p].Spans, sp)
				}
			}
		}
	}
	for p := range out {
		out[p] = proto.Clone(out[p]).(*tempopb.Trace)
	}
	return out
}

func split(src, dumpPath, dst string, k int) error {
	if src == "" || dst == "" || k < 2 {
		return errors.New("split needs -src, -dst and -k >= 2")
	}
	ctx := context.Background()
	_, srcMeta, err := blockAt(src)
	if err != nil {
		return err
	}
	var next rowSource
	var closeFn func()
	if dumpPath != "" {
		next, closeFn, err = dumpRows(dumpPath)
	} else {
		next, closeFn, err = parquetRows(src, srcMeta.DedicatedColumns)
	}
	if err != nil {
		return err
	}
	defer closeFn()
	r, w, err := openBackend(dst)
	if err != nil {
		return err
	}
	cfg := &common.BlockConfig{BloomFP: common.DefaultBloomFP, BloomShardSizeBytes: common.DefaultBloomShardSizeBytes, RowGroupSizeBytes: 100_000_000}
	est := srcMeta.TotalObjects*12/10/int64(k) + 1

	var (
		man   splitManifest
		chans = make([][2]chan *vparquet5.Trace, k)
		wg    sync.WaitGroup
		errs  = make(chan error, 2*k)
		metas = make([][2]*backend.BlockMeta, k)
	)
	for i := 0; i < k; i++ {
		for f, v := range []string{vparquet5.VersionString, vcg1.VersionString} {
			ch := make(chan *vparquet5.Trace, 512)
			chans[i][f] = ch
			m := backend.NewBlockMeta(srcMeta.TenantID, uuid.New(), v)
			m.StartTime, m.EndTime = srcMeta.StartTime, srcMeta.EndTime
			m.DedicatedColumns = srcMeta.DedicatedColumns
			m.TotalObjects = est
			m.ReplicationFactor = 1
			wg.Add(1)
			go func(i, f int, m *backend.BlockMeta) {
				defer wg.Done()
				var out *backend.BlockMeta
				var err error
				if f == 0 {
					out, err = vparquet5.CreateBlockFromRows(ctx, cfg, m, func(context.Context) (*vparquet5.Trace, error) {
						r, ok := <-ch
						if !ok {
							return nil, nil
						}
						return r, nil
					}, r, w)
				} else {
					out, err = vcg1.CreateBlock(ctx, cfg, m, chanRows{ch}, r, w)
				}
				if err != nil {
					errs <- err
					for range ch { //nolint:revive // drain so the producer is not blocked
					}
					return
				}
				// a realistic level-1 input
				out.CompactionLevel = 1
				if err := w.WriteBlockMeta(ctx, out); err != nil {
					errs <- err
					return
				}
				metas[i][f] = out
			}(i, f, m)
		}
	}

	mf, err := os.Create(filepath.Join(dst, "multi_ids.txt"))
	if err != nil {
		return err
	}
	defer mf.Close()
	mw := bufio.NewWriter(mf)
	conv := vparquet5.NewTraceConverter(srcMeta.DedicatedColumns)
	send := func(in int, row *vparquet5.Trace) {
		chans[in][0] <- row
		chans[in][1] <- row
	}
	start := time.Now()
	var n, nSplit, nDup int
	for ; ; n++ {
		row, err := next()
		if err != nil {
			return err
		}
		if row == nil {
			break
		}
		a, b := n%k, (n+1)%k
		switch n % 10 {
		case 3:
			parts := halves(vparquet5.ParquetTraceToTempopbTrace(srcMeta, row))
			for p, in := range [2]int{a, b} {
				if len(parts[p].ResourceSpans) == 0 {
					continue
				}
				pr, _ := conv.Convert(row.TraceID, parts[p], nil)
				send(in, pr)
			}
			nSplit++
			fmt.Fprintln(mw, hex.EncodeToString(row.TraceID))
		case 7:
			send(a, row)
			send(b, row)
			nDup++
			fmt.Fprintln(mw, hex.EncodeToString(row.TraceID))
		default:
			send(a, row)
		}
		if n%200000 == 0 && n > 0 {
			fmt.Fprintf(os.Stderr, "  %d traces (%s)\n", n, time.Since(start).Round(time.Second))
		}
	}
	for i := range chans {
		close(chans[i][0])
		close(chans[i][1])
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		return err
	}
	if err := mw.Flush(); err != nil {
		return err
	}
	for i := range metas {
		man.VP = append(man.VP, metas[i][0].BlockID.String())
		man.CG = append(man.CG, metas[i][1].BlockID.String())
		fmt.Printf("input %d: vParquet5 %s %d traces %d bytes | vCG1 %s %d traces %d bytes\n", i,
			metas[i][0].BlockID, metas[i][0].TotalObjects, metas[i][0].Size_, metas[i][1].BlockID, metas[i][1].TotalObjects, metas[i][1].Size_)
	}
	fmt.Printf("%d source traces: %d split into partial traces, %d duplicated whole; %s\n", n, nSplit, nDup, time.Since(start).Round(time.Second))
	b, _ := json.MarshalIndent(man, "", " ")
	return os.WriteFile(filepath.Join(dst, "split.json"), b, 0o644)
}

// ---------------------------------------------------------------- compact

type callbacks struct {
	mu                                     sync.Mutex
	ObjectsCombined, ObjectsWritten        int
	BytesWritten, TraceWritten             int
	TraceWrittenBytes                      int
	SpansDiscardedCalls, SpansDiscarded    int
	DisconnectedTrace, RootlessTrace       int
	DedupedSpansCalls, DedupedSpans        int
	ObjectsWrittenCalls, BytesWrittenCalls int
	Levels                                 []int
	levels                                 map[int]bool
}

func (c *callbacks) level(l int) {
	if !c.levels[l] {
		c.levels[l] = true
		c.Levels = append(c.Levels, l)
	}
}

type compactReport struct {
	Version   string               `json:"version"`
	Inputs    int                  `json:"inputs"`
	InBytes   uint64               `json:"input_bytes"`
	InTraces  int64                `json:"input_traces"`
	WallS     float64              `json:"wall_s"`
	UserS     float64              `json:"user_s"`
	SysS      float64              `json:"sys_s"`
	PeakHeap  uint64               `json:"peak_heap_inuse_mib"`
	MaxRSS    int64                `json:"max_rss_mib"`
	Outputs   []*backend.BlockMeta `json:"outputs"`
	Callbacks *callbacks           `json:"callbacks"`
}

func compact(root, tenant string, ids []string, outBlocks, maxBytes int, reportPath string) error {
	ctx := context.Background()
	r, w, err := openBackend(root)
	if err != nil {
		return err
	}
	var metas []*backend.BlockMeta
	rep := &compactReport{Callbacks: &callbacks{levels: map[int]bool{}}}
	for _, s := range ids {
		id, err := uuid.Parse(s)
		if err != nil {
			return err
		}
		m, err := r.BlockMeta(ctx, id, tenant)
		if err != nil {
			return err
		}
		metas = append(metas, m)
		rep.InBytes += m.Size_
		rep.InTraces += m.TotalObjects
	}
	rep.Version, rep.Inputs = metas[0].Version, len(metas)
	enc, err := encoding.FromVersion(metas[0].Version)
	if err != nil {
		return err
	}
	cb := rep.Callbacks
	opts := common.CompactionOptions{
		MaxBytesPerTrace: maxBytes,
		OutputBlocks:     uint8(outBlocks),
		BlockConfig:      common.BlockConfig{BloomFP: common.DefaultBloomFP, BloomShardSizeBytes: common.DefaultBloomShardSizeBytes, RowGroupSizeBytes: 100_000_000},
		ObjectsCombined: func(l, n int) {
			cb.mu.Lock()
			defer cb.mu.Unlock()
			cb.level(l)
			cb.ObjectsCombined += n
		},
		ObjectsWritten: func(l, n int) {
			cb.mu.Lock()
			defer cb.mu.Unlock()
			cb.level(l)
			cb.ObjectsWritten += n
			cb.ObjectsWrittenCalls++
		},
		BytesWritten: func(l, n int) {
			cb.mu.Lock()
			defer cb.mu.Unlock()
			cb.level(l)
			cb.BytesWritten += n
			cb.BytesWrittenCalls++
		},
		TraceWritten: func(l, n int) {
			cb.mu.Lock()
			defer cb.mu.Unlock()
			cb.level(l)
			cb.TraceWritten++
			cb.TraceWrittenBytes += n
		},
		SpansDiscarded: func(_, _, _ string, n int) {
			cb.mu.Lock()
			defer cb.mu.Unlock()
			cb.SpansDiscardedCalls++
			cb.SpansDiscarded += n
		},
		DisconnectedTrace: func() { cb.mu.Lock(); cb.DisconnectedTrace++; cb.mu.Unlock() },
		RootlessTrace:     func() { cb.mu.Lock(); cb.RootlessTrace++; cb.mu.Unlock() },
		DedupedSpans: func(_, n int) {
			cb.mu.Lock()
			defer cb.mu.Unlock()
			cb.DedupedSpansCalls++
			cb.DedupedSpans += n
		},
	}

	var peak atomic.Uint64
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		var ms runtime.MemStats
		var profAt uint64
		tk := time.NewTicker(50 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				runtime.ReadMemStats(&ms)
				if heapProf != "" && ms.HeapInuse > profAt+256<<20 {
					profAt = ms.HeapInuse
					if f, err := os.Create(heapProf); err == nil {
						_ = pprof.WriteHeapProfile(f)
						_ = f.Close()
					}
				}
				if ms.HeapInuse > peak.Load() {
					peak.Store(ms.HeapInuse)
				}
			}
		}
	}()
	if cpuProf != "" {
		f, err := os.Create(cpuProf)
		if err != nil {
			return err
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			return err
		}
		defer pprof.StopCPUProfile()
	}
	start := time.Now()
	out, err := enc.NewCompactor(opts).Compact(ctx, log.NewNopLogger(), r, w, metas)
	close(stop)
	<-done
	if err != nil {
		return err
	}
	rep.WallS = time.Since(start).Seconds()
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	rep.UserS = time.Duration(ru.Utime.Nano()).Seconds()
	rep.SysS = time.Duration(ru.Stime.Nano()).Seconds()
	rep.MaxRSS = ru.Maxrss >> 20 // bytes on darwin
	rep.PeakHeap = peak.Load() >> 20
	rep.Outputs = out
	b, _ := json.MarshalIndent(rep, "", " ")
	fmt.Println(string(b))
	if reportPath != "" {
		return os.WriteFile(reportPath, b, 0o644)
	}
	return nil
}

// ---------------------------------------------------------------- compare

// canon marshals tr with every attribute list sorted.
func canon(tr *tempopb.Trace) []byte {
	if tr == nil {
		return nil
	}
	tr = proto.Clone(tr).(*tempopb.Trace)
	srt := func(a []*v1.KeyValue) {
		sort.SliceStable(a, func(i, j int) bool {
			if a[i].Key != a[j].Key {
				return a[i].Key < a[j].Key
			}
			return a[i].Value.String() < a[j].Value.String()
		})
	}
	for _, rs := range tr.ResourceSpans {
		if rs.Resource != nil {
			srt(rs.Resource.Attributes)
		}
		for _, ss := range rs.ScopeSpans {
			if ss.Scope != nil {
				srt(ss.Scope.Attributes)
			}
			for _, sp := range ss.Spans {
				srt(sp.Attributes)
				for _, e := range sp.Events {
					srt(e.Attributes)
				}
				for _, l := range sp.Links {
					srt(l.Attributes)
				}
			}
		}
	}
	b, err := tr.Marshal()
	if err != nil {
		panic(err)
	}
	return b
}

// cgRows walks the CG output blocks in order with the sequential iterator.
func cgRows(ctx context.Context, r backend.Reader, metas []*backend.BlockMeta) rowSource {
	bi := 0
	var it *cg.TraceIterator
	dec, _ := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
	return func() (*vparquet5.Trace, error) {
		for {
			if it == nil {
				if bi >= len(metas) {
					return nil, nil
				}
				m := metas[bi]
				rr := cg.BackendRangeReader{R: r, Name: cg.ObjectName, BlockID: uuid.UUID(m.BlockID), TenantID: m.TenantID}
				rd, err := cg.OpenKnown(rr, int64(m.Size_), m.FooterSize, cg.WithDecoder(dec))
				if err != nil {
					return nil, err
				}
				q, err := rd.NewRequest(ctx)
				if err != nil {
					return nil, err
				}
				it = rd.NewTraceIterator(q.F, m.DedicatedColumns, 0)
			}
			row, err := it.Next(ctx)
			if err != nil || row != nil {
				return row, err
			}
			it.Close()
			it, bi = nil, bi+1
		}
	}
}

func readIDs(path string) ([][]byte, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out [][]byte
	for _, l := range strings.Fields(string(b)) {
		id, err := hex.DecodeString(l)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}

func compare(root, tenant string, vpIDs, cgIDs []string, multiPath string, sample int, seed int64, idsOut string) error {
	ctx := context.Background()
	r, _, err := openBackend(root)
	if err != nil {
		return err
	}
	load := func(ids []string) ([]*backend.BlockMeta, error) {
		var out []*backend.BlockMeta
		for _, s := range ids {
			id, err := uuid.Parse(s)
			if err != nil {
				return nil, err
			}
			m, err := r.BlockMeta(ctx, id, tenant)
			if err != nil {
				return nil, err
			}
			out = append(out, m)
		}
		return out, nil
	}
	vps, err := load(vpIDs)
	if err != nil {
		return err
	}
	cgs, err := load(cgIDs)
	if err != nil {
		return err
	}

	// 1. every trace, in order
	t0 := time.Now()
	vi := 0
	var vnext rowSource
	var vclose func()
	vpRow := func() (*vparquet5.Trace, *backend.BlockMeta, error) {
		for {
			if vnext == nil {
				if vi >= len(vps) {
					return nil, nil, nil
				}
				vnext, vclose, err = parquetRows(filepath.Join(root, tenant, vps[vi].BlockID.String()), vps[vi].DedicatedColumns)
				if err != nil {
					return nil, nil, err
				}
			}
			row, err := vnext()
			if err != nil || row != nil {
				return row, vps[vi], err
			}
			vclose()
			vnext, vi = nil, vi+1
		}
	}
	cnext := cgRows(ctx, r, cgs)
	var all [][]byte
	n, exact, canonEq, bad := 0, 0, 0, 0
	for {
		vr, vm, err := vpRow()
		if err != nil {
			return err
		}
		cr, err := cnext()
		if err != nil {
			return err
		}
		if vr == nil || cr == nil {
			if vr != nil || cr != nil {
				return fmt.Errorf("trace counts differ after %d traces (vParquet5 done %v, vCG1 done %v)", n, vr == nil, cr == nil)
			}
			break
		}
		if !bytes.Equal(vr.TraceID, cr.TraceID) {
			return fmt.Errorf("trace %d: vParquet5 %x, vCG1 %x", n, vr.TraceID, cr.TraceID)
		}
		want := vparquet5.ParquetTraceToTempopbTrace(vm, vr)
		got := vparquet5.ParquetTraceToTempopbTrace(cgs[0], cr)
		wb, _ := want.Marshal()
		gb, _ := got.Marshal()
		switch {
		case bytes.Equal(wb, gb):
			exact++
		case bytes.Equal(canon(want), canon(got)):
			canonEq++
		default:
			if bad++; bad <= 5 {
				fmt.Printf("MISMATCH trace %d %x\n", n, vr.TraceID)
			}
		}
		all = append(all, append([]byte(nil), vr.TraceID...))
		n++
		if n%500000 == 0 {
			fmt.Fprintf(os.Stderr, "  %d traces compared (%s)\n", n, time.Since(t0).Round(time.Second))
		}
	}
	fmt.Printf("in order: %d traces, %d byte-identical, %d equal up to attribute order, %d different (%s)\n", n, exact, canonEq, bad, time.Since(t0).Round(time.Second))

	// 2. FindTraceByID on a sample (half from the multi-input ids)
	multiIDs, err := readIDs(multiPath)
	if err != nil {
		return err
	}
	rnd := rand.New(rand.NewSource(seed))
	pick := map[string][]byte{}
	for len(multiIDs) > 0 && len(pick) < sample/2 && len(pick) < len(multiIDs) {
		id := multiIDs[rnd.Intn(len(multiIDs))]
		pick[string(id)] = id
	}
	for len(pick) < sample && len(pick) < len(all) {
		id := all[rnd.Intn(len(all))]
		pick[string(id)] = id
	}
	ids := make([][]byte, 0, len(pick))
	for _, id := range pick {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool { return bytes.Compare(ids[a], ids[b]) < 0 })
	open := func(ms []*backend.BlockMeta) ([]common.BackendBlock, error) {
		var out []common.BackendBlock
		for _, m := range ms {
			b, err := encoding.OpenBlock(m, r)
			if err != nil {
				return nil, err
			}
			out = append(out, b)
		}
		return out, nil
	}
	vb, err := open(vps)
	if err != nil {
		return err
	}
	cb, err := open(cgs)
	if err != nil {
		return err
	}
	find := func(bs []common.BackendBlock, id []byte) (*tempopb.Trace, error) {
		var found *tempopb.Trace
		for _, b := range bs {
			res, err := b.FindTraceByID(ctx, id, common.DefaultSearchOptions())
			if err != nil {
				return nil, err
			}
			if res != nil && res.Trace != nil {
				if found != nil {
					return nil, fmt.Errorf("%x found in two output blocks", id)
				}
				found = res.Trace
			}
		}
		return found, nil
	}
	t0 = time.Now()
	var fbad, fnil atomic.Int64
	var wg sync.WaitGroup
	work := make(chan []byte)
	var ferr atomic.Value
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range work {
				want, err := find(vb, id)
				if err != nil {
					ferr.Store(err)
					continue
				}
				got, err := find(cb, id)
				if err != nil {
					ferr.Store(err)
					continue
				}
				if want == nil || got == nil {
					fnil.Add(1)
					continue
				}
				if !bytes.Equal(canon(want), canon(got)) {
					fbad.Add(1)
				}
			}
		}()
	}
	for _, id := range ids {
		work <- id
	}
	close(work)
	wg.Wait()
	if e := ferr.Load(); e != nil {
		return e.(error)
	}
	fmt.Printf("FindTraceByID: %d ids (%d from the multi-input ids), %d different, %d not found (%s)\n", len(ids), min(sample/2, len(multiIDs)), fbad.Load(), fnil.Load(), time.Since(t0).Round(time.Second))

	absent := bytes.Repeat([]byte{0xfe}, 16)
	if got, err := find(cb, absent); err != nil || got != nil {
		return fmt.Errorf("absent id: found %v err %v", got != nil, err)
	}
	if idsOut != "" {
		type e struct {
			ID string `json:"id"`
		}
		var list []e
		for i, id := range ids {
			if i%max(1, len(ids)/200) == 0 {
				list = append(list, e{hex.EncodeToString(id)})
			}
		}
		b, _ := json.MarshalIndent(list, "", " ")
		if err := os.WriteFile(idsOut, b, 0o644); err != nil {
			return err
		}
	}
	if bad > 0 || fbad.Load() > 0 || fnil.Load() > 0 {
		return errors.New("outputs differ")
	}
	return nil
}

// sizes reports the largest traces of a vCG1 block (spans, attributes, estimated proto bytes).
func sizes(root, tenant, id string) error {
	ctx := context.Background()
	r, _, err := openBackend(root)
	if err != nil {
		return err
	}
	bid, err := uuid.Parse(id)
	if err != nil {
		return err
	}
	m, err := r.BlockMeta(ctx, bid, tenant)
	if err != nil {
		return err
	}
	next := cgRows(ctx, r, []*backend.BlockMeta{m})
	est := vparquet5.NewSizeEstimator(m.DedicatedColumns)
	type big struct{ spans, attrs, bytes int }
	var top []big
	n := 0
	for {
		tr, err := next()
		if err != nil {
			return err
		}
		if tr == nil {
			break
		}
		n++
		b := big{bytes: est.ProtoSize(tr)}
		for _, rs := range tr.ResourceSpans {
			b.attrs += len(rs.Resource.Attrs)
			for _, ss := range rs.ScopeSpans {
				for _, sp := range ss.Spans {
					b.spans++
					b.attrs += len(sp.Attrs)
					for _, e := range sp.Events {
						b.attrs += len(e.Attrs)
					}
				}
			}
		}
		if n%200000 == 0 {
			runtime.GC()
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			fmt.Printf("  %d traces: live heap %d MiB\n", n, ms.HeapAlloc>>20)
			if heapProf != "" && n == 400000 {
				if f, err := os.Create(heapProf); err == nil {
					_ = pprof.WriteHeapProfile(f)
					_ = f.Close()
				}
			}
		}
		top = append(top, b)
		sort.Slice(top, func(i, j int) bool { return top[i].bytes > top[j].bytes })
		if len(top) > 5 {
			top = top[:5]
		}
	}
	fmt.Printf("%d traces; largest: %+v\n", n, top)
	return nil
}

package vcg1_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/gogo/protobuf/proto"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/grafana/tempo/v3/pkg/tempopb"
	v1_trace "github.com/grafana/tempo/v3/pkg/tempopb/trace/v1"
	"github.com/grafana/tempo/v3/tempodb/backend"
	"github.com/grafana/tempo/v3/tempodb/backend/local"
	"github.com/grafana/tempo/v3/tempodb/encoding"
	"github.com/grafana/tempo/v3/tempodb/encoding/common"
	"github.com/grafana/tempo/v3/tempodb/encoding/vcg1"
	cg "github.com/grafana/tempo/v3/tempodb/encoding/vcg1/cgfile"
	"github.com/grafana/tempo/v3/tempodb/encoding/vcg1/internal/cgtest"
	"github.com/grafana/tempo/v3/tempodb/encoding/vparquet5"
)

func blockConfig() *common.BlockConfig {
	return &common.BlockConfig{BloomFP: 0.01, BloomShardSizeBytes: 100 * 1024, RowGroupSizeBytes: 64 << 10}
}

type store struct {
	r backend.Reader
	w backend.Writer
}

func newStore(t *testing.T) store {
	rr, ww, _, err := local.New(&local.Config{Path: t.TempDir()})
	require.NoError(t, err)
	return store{backend.NewReader(rr), backend.NewWriter(ww)}
}

// rowsIter feeds rows to vcg1.CreateBlock as they are.
type rowsIter struct {
	rows []*vparquet5.Trace
	i    int
}

func (it *rowsIter) NextRow(context.Context) (*vparquet5.Trace, error) {
	if it.i >= len(it.rows) {
		return nil, nil
	}
	it.i++
	return it.rows[it.i-1], nil
}
func (it *rowsIter) Next(context.Context) (common.ID, *tempopb.Trace, error) { return nil, nil, io.EOF }
func (it *rowsIter) Close()                                                  {}

// createBoth writes the same rows (fresh vParquet5 rows of the proto traces) as a vCG1 and a
// vParquet5 block.
func createBoth(t *testing.T, s store, traces []cgtest.Trace, level uint32, dc backend.DedicatedColumns) (cgMeta, vpMeta *backend.BlockMeta) {
	ctx := context.Background()
	conv := vparquet5.NewTraceConverter(dc)
	mk := func() []*vparquet5.Trace {
		rows := make([]*vparquet5.Trace, len(traces))
		for i, tr := range traces {
			rows[i], _ = conv.Convert(tr.ID, tr.Trace, nil)
		}
		return rows
	}
	meta := func(v string) *backend.BlockMeta {
		m := backend.NewBlockMeta("tenant", uuid.New(), v)
		m.DedicatedColumns = dc
		m.TotalObjects = int64(len(traces))
		m.StartTime = time.Unix(1_700_000_000+int64(level)*10, 0)
		m.EndTime = m.StartTime.Add(time.Hour)
		m.ReplicationFactor = 1
		return m
	}
	cm, err := vcg1.CreateBlock(ctx, blockConfig(), meta(vcg1.VersionString), &rowsIter{rows: mk()}, s.r, s.w)
	require.NoError(t, err)
	cm.CompactionLevel = level
	require.NoError(t, s.w.WriteBlockMeta(ctx, cm))

	vrows, i := mk(), 0
	vm, err := vparquet5.CreateBlockFromRows(ctx, blockConfig(), meta(vparquet5.VersionString), func(context.Context) (*vparquet5.Trace, error) {
		if i >= len(vrows) {
			return nil, nil
		}
		i++
		return vrows[i-1], nil
	}, s.r, s.w)
	require.NoError(t, err)
	vm.CompactionLevel = level
	require.NoError(t, s.w.WriteBlockMeta(ctx, vm))
	return cm, vm
}

// split cuts traces into k overlapping inputs: most traces go to one input, every 7th trace
// is copied whole into two inputs, and every 5th has its spans spread over all inputs with
// some spans in two of them (partial traces whose combine dedupes spans).
func split(traces []cgtest.Trace, k int) [][]cgtest.Trace {
	out := make([][]cgtest.Trace, k)
	for i, tr := range traces {
		switch {
		case i%5 == 0:
			parts := make([]*tempopb.Trace, k)
			for p := range parts {
				parts[p] = &tempopb.Trace{}
			}
			n := 0
			for _, rs := range tr.Trace.ResourceSpans {
				prs := make([]*v1_trace.ResourceSpans, k)
				pss := make([]*v1_trace.ScopeSpans, k)
				for _, ss := range rs.ScopeSpans {
					for p := range pss {
						pss[p] = nil
					}
					for _, sp := range ss.Spans {
						dst := []int{n % k}
						if n%3 == 0 {
							dst = append(dst, (n+1)%k)
						}
						n++
						for _, p := range dst {
							if prs[p] == nil {
								prs[p] = &v1_trace.ResourceSpans{Resource: rs.Resource}
								parts[p].ResourceSpans = append(parts[p].ResourceSpans, prs[p])
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
			for p := range parts {
				if len(parts[p].ResourceSpans) > 0 {
					out[p] = append(out[p], cgtest.Trace{ID: tr.ID, Trace: proto.Clone(parts[p]).(*tempopb.Trace)})
				}
			}
		case i%7 == 0:
			out[i%k] = append(out[i%k], tr)
			out[(i+1)%k] = append(out[(i+1)%k], cgtest.Trace{ID: tr.ID, Trace: proto.Clone(tr.Trace).(*tempopb.Trace)})
		default:
			out[i%k] = append(out[i%k], tr)
		}
	}
	return out
}

// recorder records the compaction callbacks.
type recorder struct {
	mu                                             sync.Mutex
	combined, written, bytes, traceN, traceBytes   int
	discardedCalls, discardedSpans, deduped, disc  int
	rootless                                       int
	discardedIDs                                   []string
	dropped                                        map[string]bool
	cancelAfter                                    int
	cancel                                         context.CancelFunc
	dropCalls                                      int
	levels                                         map[int]bool
	objectsWrittenCalls, bytesWrittenCalls, dedupN int
}

func (r *recorder) opts(maxBytes int, outputBlocks uint8) common.CompactionOptions {
	r.levels = map[int]bool{}
	return common.CompactionOptions{
		MaxBytesPerTrace: maxBytes,
		OutputBlocks:     outputBlocks,
		BlockConfig:      *blockConfig(),
		DropObject: func(id common.ID) bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.dropCalls++
			if r.cancel != nil && r.dropCalls == r.cancelAfter {
				r.cancel()
			}
			return r.dropped[string(id)]
		},
		ObjectsCombined: func(l, n int) { r.levels[l] = true; r.combined += n },
		ObjectsWritten:  func(l, n int) { r.levels[l] = true; r.written += n; r.objectsWrittenCalls++ },
		BytesWritten:    func(l, n int) { r.levels[l] = true; r.bytes += n; r.bytesWrittenCalls++ },
		TraceWritten:    func(l, n int) { r.levels[l] = true; r.traceN++; r.traceBytes += n },
		SpansDiscarded: func(id, _, _ string, n int) {
			r.discardedCalls++
			r.discardedSpans += n
			r.discardedIDs = append(r.discardedIDs, id)
		},
		DisconnectedTrace: func() { r.disc++ },
		RootlessTrace:     func() { r.rootless++ },
		DedupedSpans:      func(_, n int) { r.deduped += n; r.dedupN++ },
	}
}

// semantic is what both compactors must report identically.
func (r *recorder) semantic() []int {
	sort.Strings(r.discardedIDs)
	return []int{r.combined, r.traceN, r.traceBytes, r.discardedCalls, r.discardedSpans, r.deduped, r.dedupN, r.disc, r.rootless}
}

func traceIDs(parts [][]cgtest.Trace) [][]byte {
	seen := map[string]bool{}
	var out [][]byte
	for _, p := range parts {
		for _, tr := range p {
			if !seen[string(tr.ID)] {
				seen[string(tr.ID)] = true
				out = append(out, tr.ID)
			}
		}
	}
	return out
}

func sortedInput(p []cgtest.Trace) []cgtest.Trace {
	sort.SliceStable(p, func(a, b int) bool { return bytes.Compare(p[a].ID, p[b].ID) < 0 })
	return p
}

type compaction struct {
	cg, vp   []*backend.BlockMeta
	rcg, rvp *recorder
}

// compactBoth compacts the inputs with both compactors.
func compactBoth(t *testing.T, s store, parts [][]cgtest.Trace, maxBytes int, outputBlocks uint8, dropped map[string]bool) compaction {
	ctx := context.Background()
	var cgIn, vpIn []*backend.BlockMeta
	for i, p := range parts {
		cm, vm := createBoth(t, s, sortedInput(p), uint32(i%2), cgtest.Dedicated)
		cgIn, vpIn = append(cgIn, cm), append(vpIn, vm)
	}
	out := compaction{rcg: &recorder{dropped: dropped}, rvp: &recorder{dropped: dropped}}
	var err error
	out.cg, err = vcg1.NewCompactor(out.rcg.opts(maxBytes, outputBlocks)).Compact(ctx, log.NewNopLogger(), s.r, s.w, cgIn)
	require.NoError(t, err)
	out.vp, err = vparquet5.NewCompactor(out.rvp.opts(maxBytes, outputBlocks)).Compact(ctx, log.NewNopLogger(), s.r, s.w, vpIn)
	require.NoError(t, err)
	return out
}

func find(t *testing.T, s store, metas []*backend.BlockMeta, id []byte) *tempopb.Trace {
	var found []*tempopb.Trace
	for _, m := range metas {
		blk, err := encoding.OpenBlock(m, s.r)
		require.NoError(t, err)
		res, err := blk.FindTraceByID(context.Background(), id, common.DefaultSearchOptions())
		require.NoError(t, err)
		if res != nil && res.Trace != nil {
			found = append(found, res.Trace)
		}
	}
	require.LessOrEqual(t, len(found), 1, "trace %x in several output blocks", id)
	if len(found) == 0 {
		return nil
	}
	return found[0]
}

func checkMetas(t *testing.T, c compaction) {
	require.Equal(t, len(c.vp), len(c.cg))
	for i := range c.cg {
		cm, vm := c.cg[i], c.vp[i]
		require.Equal(t, vcg1.VersionString, cm.Version)
		require.Equal(t, vm.TotalObjects, cm.TotalObjects)
		require.Equal(t, vm.CompactionLevel, cm.CompactionLevel)
		require.Equal(t, vm.StartTime, cm.StartTime)
		require.Equal(t, vm.EndTime, cm.EndTime)
		require.Equal(t, vm.ReplicationFactor, cm.ReplicationFactor)
		require.Equal(t, vm.DedicatedColumns, cm.DedicatedColumns)
		require.Equal(t, vm.BloomShardCount, cm.BloomShardCount)
		require.Positive(t, cm.TotalRecords)
	}
}

// TestCompactorMatchesVP5 compacts overlapping inputs with duplicated and partial traces with
// both compactors and compares the outputs and the callbacks.
func TestCompactorMatchesVP5(t *testing.T) {
	traces := cgtest.Traces(3000, 11)
	for _, tc := range []struct {
		name         string
		k            int
		maxBytes     int
		outputBlocks uint8
	}{
		{"3 inputs", 3, 0, 1},
		{"4 inputs, 2 outputs", 4, 0, 2},
		{"max bytes per trace", 3, 3000, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			parts := split(traces, tc.k)
			c := compactBoth(t, s, parts, tc.maxBytes, tc.outputBlocks, nil)
			checkMetas(t, c)
			require.Len(t, c.cg, int(tc.outputBlocks))
			ids := traceIDs(parts)
			var total int64
			for _, m := range c.cg {
				total += m.TotalObjects
			}
			require.Equal(t, int64(len(ids)), total)

			require.Equal(t, c.rvp.semantic(), c.rcg.semantic())
			require.Equal(t, c.rvp.discardedIDs, c.rcg.discardedIDs)
			require.Positive(t, c.rcg.combined)
			require.Positive(t, c.rcg.deduped)
			if tc.maxBytes > 0 {
				require.Positive(t, c.rcg.discardedCalls)
			}
			// every trace is written once; vParquet5 does not report its last row group's objects
			require.Equal(t, len(ids), c.rcg.written)
			require.LessOrEqual(t, c.rvp.written, c.rcg.written)
			var size uint64
			for _, m := range c.cg {
				size += m.Size_
			}
			require.Equal(t, int(size), c.rcg.bytes)
			require.Equal(t, map[int]bool{1: true}, c.rcg.levels)
			require.Equal(t, c.rvp.levels, c.rcg.levels)

			for i, id := range ids {
				if i%6 != 0 {
					continue
				}
				want := find(t, s, c.vp, id)
				got := find(t, s, c.cg, id)
				require.NotNil(t, want)
				require.NotNil(t, got)
				require.Equal(t, cgtest.Canon(want), cgtest.Canon(got), "trace %d %x", i, id)
			}
		})
	}
}

func TestCompactorDropObject(t *testing.T) {
	traces := cgtest.Traces(600, 5)
	parts := split(traces, 2)
	dropped := map[string]bool{}
	for i, tr := range traces {
		if i%4 == 0 { // single and combined ids
			dropped[string(tr.ID)] = true
		}
	}
	s := newStore(t)
	c := compactBoth(t, s, parts, 0, 1, dropped)
	checkMetas(t, c)
	require.Equal(t, c.rvp.semantic(), c.rcg.semantic())
	require.Equal(t, int64(len(traces)-len(dropped)), c.cg[0].TotalObjects)
	for i, tr := range traces {
		got := find(t, s, c.cg, tr.ID)
		if dropped[string(tr.ID)] {
			require.Nil(t, got, "trace %d", i)
			continue
		}
		require.Equal(t, cgtest.Canon(find(t, s, c.vp, tr.ID)), cgtest.Canon(got), "trace %d", i)
	}
}

func TestCompactorEmptyInputs(t *testing.T) {
	traces := cgtest.Traces(200, 9)
	s := newStore(t)
	t.Run("one empty input", func(t *testing.T) {
		c := compactBoth(t, s, [][]cgtest.Trace{nil, traces, nil}, 0, 1, nil)
		checkMetas(t, c)
		require.Len(t, c.cg, 1)
		require.Equal(t, int64(len(traces)), c.cg[0].TotalObjects)
		require.Equal(t, c.rvp.semantic(), c.rcg.semantic())
		for _, tr := range traces[:50] {
			require.Equal(t, cgtest.Canon(find(t, s, c.vp, tr.ID)), cgtest.Canon(find(t, s, c.cg, tr.ID)))
		}
	})
	t.Run("all empty", func(t *testing.T) {
		c := compactBoth(t, s, [][]cgtest.Trace{nil, nil}, 0, 1, nil)
		require.Empty(t, c.cg)
		require.Empty(t, c.vp)
	})
	t.Run("no inputs", func(t *testing.T) {
		out, err := vcg1.NewCompactor(common.CompactionOptions{OutputBlocks: 1}).Compact(context.Background(), log.NewNopLogger(), s.r, s.w, nil)
		require.NoError(t, err)
		require.Empty(t, out)
	})
	t.Run("zero output blocks", func(t *testing.T) {
		// vparquet5 divides by zero here; vCG1 writes one block
		var in []*backend.BlockMeta
		for _, p := range split(traces, 2) {
			cm, _ := createBoth(t, s, sortedInput(p), 0, cgtest.Dedicated)
			in = append(in, cm)
		}
		out, err := vcg1.NewCompactor((&recorder{}).opts(0, 0)).Compact(context.Background(), log.NewNopLogger(), s.r, s.w, in)
		require.NoError(t, err)
		require.Len(t, out, 1)
		require.Equal(t, int64(len(traces)), out[0].TotalObjects)
	})
}

// TestCompactorDuplicateIDs: an id in every input, identical rows (taken as is, no combine) and
// different rows (combined once, whatever the number of inputs).
func TestCompactorDuplicateIDs(t *testing.T) {
	traces := cgtest.Traces(50, 13)
	same := [][]cgtest.Trace{traces, traces, traces}
	s := newStore(t)
	c := compactBoth(t, s, same, 0, 1, nil)
	require.Equal(t, 0, c.rcg.combined)
	require.Equal(t, c.rvp.semantic(), c.rcg.semantic())
	require.Equal(t, int64(len(traces)), c.cg[0].TotalObjects)

	parts := split(traces, 3)
	parts = append(parts, traces) // every id once more, whole
	c = compactBoth(t, s, parts, 0, 1, nil)
	require.Equal(t, c.rvp.semantic(), c.rcg.semantic())
	require.Equal(t, int64(len(traces)), c.cg[0].TotalObjects)
	for _, tr := range traces {
		got := find(t, s, c.cg, tr.ID)
		require.Equal(t, cgtest.Canon(find(t, s, c.vp, tr.ID)), cgtest.Canon(got))
	}
}

func TestCompactorCancel(t *testing.T) {
	traces := cgtest.Traces(3000, 3)
	s := newStore(t)
	var in []*backend.BlockMeta
	for _, p := range split(traces, 2) {
		cm, _ := createBoth(t, s, sortedInput(p), 0, cgtest.Dedicated)
		in = append(in, cm)
	}
	t.Run("before", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := vcg1.NewCompactor((&recorder{}).opts(0, 1)).Compact(ctx, log.NewNopLogger(), s.r, s.w, in)
		require.ErrorIs(t, err, context.Canceled)
	})
	t.Run("during", func(t *testing.T) {
		old := vcg1.IterWindowSpans
		vcg1.IterWindowSpans = 64
		defer func() { vcg1.IterWindowSpans = old }()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		rec := &recorder{cancelAfter: 500, cancel: cancel}
		_, err := vcg1.NewCompactor(rec.opts(0, 1)).Compact(ctx, log.NewNopLogger(), s.r, s.w, in)
		require.ErrorIs(t, err, context.Canceled)
		require.Less(t, rec.dropCalls, 2600, "stops soon after the cancel")
	})
}

func TestCompactorRefusesMixedInputs(t *testing.T) {
	traces := cgtest.Traces(20, 1)
	s := newStore(t)
	cm, vm := createBoth(t, s, traces, 0, cgtest.Dedicated)
	_, err := vcg1.NewCompactor((&recorder{}).opts(0, 1)).Compact(context.Background(), log.NewNopLogger(), s.r, s.w, []*backend.BlockMeta{cm, vm})
	require.ErrorContains(t, err, "is vParquet5")

	cm2, _ := createBoth(t, s, traces, 0, cgtest.Dedicated[:1])
	_, err = vcg1.NewCompactor((&recorder{}).opts(0, 1)).Compact(context.Background(), log.NewNopLogger(), s.r, s.w, []*backend.BlockMeta{cm, cm2})
	require.ErrorContains(t, err, "dedicated columns")
}

// cgReadCounter counts reads of block.cg.
type cgReadCounter struct {
	backend.Reader
	mu  sync.Mutex
	cgN int
}

func (r *cgReadCounter) ReadRange(ctx context.Context, name string, id uuid.UUID, tenant string, off uint64, b []byte, ci *backend.CacheInfo) error {
	if name == cg.ObjectName {
		r.mu.Lock()
		r.cgN++
		r.mu.Unlock()
	}
	return r.Reader.ReadRange(ctx, name, id, tenant, off, b, ci)
}

// TestFindTraceByIDBloom: an id the bloom rules out returns nil without reading block.cg.
func TestFindTraceByIDBloom(t *testing.T) {
	traces := cgtest.Traces(300, 2)
	s := newStore(t)
	cm, _ := createBoth(t, s, traces, 0, cgtest.Dedicated)
	cr := &cgReadCounter{Reader: s.r}
	blk, err := encoding.OpenBlock(cm, cr)
	require.NoError(t, err)
	ctx := context.Background()
	res, err := blk.FindTraceByID(ctx, traces[10].ID, common.DefaultSearchOptions())
	require.NoError(t, err)
	require.NotNil(t, res.Trace)
	require.Positive(t, cr.cgN)

	absent, misses := 0, 0
	for i := 0; i < 200; i++ {
		id := bytes.Repeat([]byte{byte(i), 0xab}, 8)
		cr.cgN = 0
		res, err := blk.FindTraceByID(ctx, id, common.DefaultSearchOptions())
		require.NoError(t, err)
		if res == nil {
			require.Zero(t, cr.cgN)
			absent++
			continue
		}
		require.Nil(t, res.Trace)
		misses++ // a bloom false positive: the footer was read
	}
	require.Greater(t, absent, 190)

	// a missing bloom shard is an error, as in vparquet5
	m := *cm
	m.BloomShardCount = 1000
	blk, err = encoding.OpenBlock(&m, s.r)
	require.NoError(t, err)
	_, err = blk.FindTraceByID(ctx, traces[0].ID, common.DefaultSearchOptions())
	require.Error(t, err)
	require.True(t, errors.Is(err, backend.ErrDoesNotExist) || strings.Contains(err.Error(), "bloom"))
}

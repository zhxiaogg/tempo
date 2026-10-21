package vcg1

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/grafana/tempo/v3/pkg/tempopb"
	"github.com/grafana/tempo/v3/pkg/traceql"
	"github.com/grafana/tempo/v3/tempodb/backend"
	"github.com/grafana/tempo/v3/tempodb/backend/local"
	"github.com/grafana/tempo/v3/tempodb/encoding/common"
	cg "github.com/grafana/tempo/v3/tempodb/encoding/vcg1/cgfile"
)

func testFooter() *cg.Footer {
	f := &cg.Footer{Keys: make([][]string, cg.NumLevels)}
	f.Keys[cg.LSpan] = []string{"foo", "http.route"}
	f.Keys[cg.LResource] = []string{"plan"}
	f.Keys[cg.LEvent] = []string{"zone"}
	return f
}

var testDedicated = backend.DedicatedColumns{
	{Scope: backend.DedicatedColumnScopeSpan, Name: "http.route", Type: backend.DedicatedColumnTypeString},
	{Scope: backend.DedicatedColumnScopeResource, Name: "k8s.pod", Type: backend.DedicatedColumnTypeString},
}

func cond(a traceql.Attribute, op traceql.Operator, ops ...traceql.Static) traceql.Condition {
	return traceql.Condition{Attribute: a, Op: op, Operands: ops}
}

func TestCompileRouting(t *testing.T) {
	span := func(n string) traceql.Attribute {
		return traceql.NewScopedAttribute(traceql.AttributeScopeSpan, false, n)
	}
	res := func(n string) traceql.Attribute {
		return traceql.NewScopedAttribute(traceql.AttributeScopeResource, false, n)
	}

	t.Run("dedicated by operand type", func(t *testing.T) {
		p, err := compile(testFooter(), []traceql.Condition{
			cond(span("http.route"), traceql.OpEqual, traceql.NewStaticString("/a")),
			cond(span("http.route"), traceql.OpEqual, traceql.NewStaticInt(5)),
		}, true, 0, 0, testDedicated, false, false)
		require.NoError(t, err)
		lv := p.lv[lvSpan]
		require.Len(t, lv.items, 1)
		d, ok := lv.items[0].(*dedItem)
		require.True(t, ok)
		require.Equal(t, uint8(cg.VTStr), d.vt)
		require.NotNil(t, lv.gen, "a wrong-typed operand reads the generic attributes")
		require.Equal(t, []int{cg.VTStr}, lv.gen.ded, "generic attributes exclude the dedicated column's type")
		require.True(t, lv.gen.has[tInt])
		require.False(t, lv.gen.has[tStr])
		require.Equal(t, 1, lv.min, "distinct names: span.http.route counted once")
	})

	t.Run("join only with one value type under AllConditions", func(t *testing.T) {
		p, err := compile(testFooter(), []traceql.Condition{cond(span("foo"), traceql.OpEqual, traceql.NewStaticString("x"))}, true, 0, 0, nil, false, false)
		require.NoError(t, err)
		require.True(t, p.lv[lvSpan].gen.join)
		p, err = compile(testFooter(), []traceql.Condition{cond(span("foo"), traceql.OpNone)}, true, 0, 0, nil, false, false)
		require.NoError(t, err)
		require.False(t, p.lv[lvSpan].gen.join, "OpNone reads every value type")
		p, err = compile(testFooter(), []traceql.Condition{cond(span("foo"), traceql.OpEqual, traceql.NewStaticString("x"))}, false, 0, 0, nil, false, false)
		require.NoError(t, err)
		require.False(t, p.lv[lvSpan].gen.join)
	})

	t.Run("unscoped goes to span and resource and clears AllConditions", func(t *testing.T) {
		p, err := compile(testFooter(), []traceql.Condition{cond(traceql.NewAttribute("foo"), traceql.OpEqual, traceql.NewStaticString("x"))}, true, 0, 0, nil, false, false)
		require.NoError(t, err)
		require.False(t, p.all)
		require.NotNil(t, p.lv[lvSpan].gen)
		require.NotNil(t, p.lv[lvRes].gen)
		require.True(t, p.requireOne)
	})

	t.Run("service.name is the resource column", func(t *testing.T) {
		p, err := compile(testFooter(), []traceql.Condition{cond(res("service.name"), traceql.OpEqual, traceql.NewStaticString("a"))}, true, 0, 0, nil, false, false)
		require.NoError(t, err)
		c, ok := p.lv[lvRes].items[0].(*colItem)
		require.True(t, ok)
		require.Equal(t, [2]int{cg.TResources, cg.ResServiceName}, [2]int{c.t, c.c})
		require.Nil(t, p.lv[lvRes].gen)
	})

	t.Run("trace conditions disable requireOne; time range adds the filter", func(t *testing.T) {
		p, err := compile(testFooter(), []traceql.Condition{cond(traceql.NewIntrinsic(traceql.IntrinsicTraceRootService), traceql.OpNone)}, false, 1, 2, nil, false, false)
		require.NoError(t, err)
		require.False(t, p.requireOne)
		require.Len(t, p.lv[lvTrace].items, 1)
		require.NotNil(t, p.timeStart)
	})

	t.Run("start time precision picks the rounded column", func(t *testing.T) {
		c := cond(traceql.IntrinsicSpanStartTimeAttribute, traceql.OpNone)
		c.Precision = 5 * time.Minute
		p, err := compile(testFooter(), []traceql.Condition{c}, true, 0, 0, nil, false, true)
		require.NoError(t, err)
		require.Equal(t, cg.SpRounded300, p.lv[lvSpan].items[0].(*colItem).c)
		require.Equal(t, 1, p.spanMin)
	})

	t.Run("span-only minimum", func(t *testing.T) {
		p, err := compile(testFooter(), []traceql.Condition{
			cond(span("foo"), traceql.OpEqual, traceql.NewStaticString("x")),
			cond(traceql.NewScopedAttribute(traceql.AttributeScopeEvent, false, "zone"), traceql.OpEqual, traceql.NewStaticString("z")),
		}, true, 0, 0, nil, false, true)
		require.NoError(t, err)
		require.Equal(t, 2, p.spanMin)
		p, err = compile(testFooter(), []traceql.Condition{cond(res("plan"), traceql.OpNone)}, false, 0, 0, nil, false, true)
		require.NoError(t, err)
		require.Equal(t, 1, p.spanMin)
	})
}

func TestWindows(t *testing.T) {
	f := &cg.Footer{Anchors: make([][cg.NumTables]uint32, 40)}
	for i := range f.Anchors {
		f.Anchors[i][cg.TSpans] = uint32(i) * 100_000
	}
	f.Rows[cg.TSpans] = 4_000_000
	w := windows(f, 2, 40)
	require.Equal(t, [2]int{2, 3}, w[0])
	require.Equal(t, [2]int{3, 7}, w[1])
	require.Equal(t, 40, w[len(w)-1][1])
	for _, x := range w[2:] {
		require.LessOrEqual(t, x[1]-x[0], 6, "capped at about windowSpans spans")
	}
}

// Queries on a real block, skipped unless CG_BLOCK_DIR names a vCG1 block directory
// (<root>/<tenant>/<uuid> with meta.json and block.cg). For profiling:
//
//	CG_BLOCK_DIR=... CG_QUERY='{ span.node = "2" }' CG_METRICS=1 go test -run TestBlockQuery -cpuprofile cpu.out
func TestBlockQuery(t *testing.T) {
	dir := os.Getenv("CG_BLOCK_DIR")
	if dir == "" {
		t.Skip("CG_BLOCK_DIR not set")
	}
	ctx := context.Background()
	tenant := filepath.Base(filepath.Dir(dir))
	raw, _, _, err := local.New(&local.Config{Path: filepath.Dir(filepath.Dir(dir))})
	require.NoError(t, err)
	r := backend.NewReader(raw)
	meta, err := r.BlockMeta(ctx, uuid.MustParse(filepath.Base(dir)), tenant)
	require.NoError(t, err)
	query := os.Getenv("CG_QUERY")
	if query == "" {
		query = `{ span.node = "2" }`
	}
	opts := common.DefaultSearchOptions()
	opts.StartPage, opts.TotalPages = atoi(os.Getenv("CG_START_PAGE")), atoi(os.Getenv("CG_PAGES"))
	blk, err := Encoding{}.OpenBlock(meta, r)
	require.NoError(t, err)
	fetcher := traceql.NewSpansetFetcherWrapperBoth(
		func(ctx context.Context, req traceql.FetchSpansRequest) (traceql.FetchSpansResponse, error) {
			return blk.Fetch(ctx, req, opts)
		},
		func(ctx context.Context, req traceql.FetchSpansRequest) (traceql.FetchSpansOnlyResponse, error) {
			return blk.FetchSpans(ctx, req, opts)
		})
	for range max(1, atoi(os.Getenv("CG_ITERS"))) {
		t0 := time.Now()
		if os.Getenv("CG_METRICS") != "" {
			step := uint64(5 * time.Minute)
			start, end, step := traceql.TrimToBlockOverlap(&tempopb.QueryRangeRequest{Query: query, End: math.MaxInt64, Step: step}, meta.StartTime, meta.EndTime)
			req := &tempopb.QueryRangeRequest{Query: query, Start: start, End: end, Step: step}
			eval, err := traceql.NewEngine().CompileMetricsQueryRange(req)
			require.NoError(t, err)
			require.NoError(t, eval.Do(ctx, fetcher, uint64(meta.StartTime.UnixNano()), uint64(meta.EndTime.UnixNano()), 0))
			t.Logf("%s: %d series, %d spans, %v", query, len(eval.Results()), eval.Metrics().SpansTotal, time.Since(t0))
			continue
		}
		resp, err := traceql.NewEngine().ExecuteSearch(ctx, &tempopb.SearchRequest{Query: query, SpansPerSpanSet: 1_000_000}, fetcher)
		require.NoError(t, err)
		t.Logf("%s: %d traces, %d bytes, %v", query, len(resp.Traces), resp.Metrics.InspectedBytes, time.Since(t0))
	}
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

// shardsReader answers shards.json reads: absent, or a fixed map.
type shardsReader struct {
	backend.Reader
	reads int
	raw   []byte
}

func (r *shardsReader) Read(context.Context, string, uuid.UUID, string, *backend.CacheInfo) ([]byte, error) {
	r.reads++
	if r.raw == nil {
		return nil, backend.ErrDoesNotExist
	}
	return r.raw, nil
}

// TestShardCacheBounded: shard maps (and their absence) are read once per block, and the cache
// holds at most shardCacheSize blocks.
func TestShardCacheBounded(t *testing.T) {
	ctx := context.Background()
	r := &shardsReader{}
	b := &backendBlock{meta: backend.NewBlockMeta("t", uuid.New(), VersionString), r: r}
	for i := 0; i < 3; i++ {
		sh, err := b.shards(ctx)
		require.NoError(t, err)
		require.Nil(t, sh)
	}
	require.Equal(t, 1, r.reads)

	r2 := &shardsReader{raw: []byte(`[[0,2],[2,5]]`)}
	b2 := &backendBlock{meta: backend.NewBlockMeta("t", uuid.New(), VersionString), r: r2}
	sh, err := b2.shards(ctx)
	require.NoError(t, err)
	require.Equal(t, [][2]int{{0, 2}, {2, 5}}, sh)

	for i := 0; i < shardCacheSize+100; i++ {
		_, err := (&backendBlock{meta: backend.NewBlockMeta("t", uuid.New(), VersionString), r: r}).shards(ctx)
		require.NoError(t, err)
	}
	require.LessOrEqual(t, shardCache.Len(), shardCacheSize)
}

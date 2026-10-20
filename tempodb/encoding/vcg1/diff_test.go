package vcg1_test

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/grafana/tempo/v3/pkg/tempopb"
	"github.com/grafana/tempo/v3/pkg/traceql"
	"github.com/grafana/tempo/v3/tempodb/backend"
	"github.com/grafana/tempo/v3/tempodb/backend/local"
	"github.com/grafana/tempo/v3/tempodb/encoding"
	"github.com/grafana/tempo/v3/tempodb/encoding/common"
)

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// TestBlockDiff runs a search on the CG block and on its vParquet5 source (CG_VP5_DIR) and
// reports the traces only one of them returns, with the matching spans of a few.
func TestBlockDiff(t *testing.T) {
	cgDir, vpDir := os.Getenv("CG_BLOCK_DIR"), os.Getenv("CG_VP5_DIR")
	if cgDir == "" || vpDir == "" {
		t.Skip("CG_BLOCK_DIR / CG_VP5_DIR not set")
	}
	ctx := context.Background()
	run := func(dir string) map[string]*tempopb.TraceSearchMetadata {
		tenant := filepath.Base(filepath.Dir(dir))
		raw, _, _, err := local.New(&local.Config{Path: filepath.Dir(filepath.Dir(dir))})
		require.NoError(t, err)
		r := backend.NewReader(raw)
		meta, err := r.BlockMeta(ctx, uuid.MustParse(filepath.Base(dir)), tenant)
		require.NoError(t, err)
		blk, err := encoding.OpenBlock(meta, r)
		require.NoError(t, err)
		opts := common.DefaultSearchOptions()
		opts.StartPage, opts.TotalPages = atoi(os.Getenv("CG_START_PAGE")), atoi(os.Getenv("CG_PAGES"))
		f := traceql.NewSpansetFetcherWrapper(func(ctx context.Context, req traceql.FetchSpansRequest) (traceql.FetchSpansResponse, error) {
			return blk.Fetch(ctx, req, opts)
		})
		resp, err := traceql.NewEngine().ExecuteSearch(ctx, &tempopb.SearchRequest{Query: os.Getenv("CG_QUERY"), SpansPerSpanSet: 1_000_000}, f)
		require.NoError(t, err)
		out := map[string]*tempopb.TraceSearchMetadata{}
		for _, tr := range resp.Traces {
			out[tr.TraceID] = tr
		}
		return out
	}
	a, b := run(cgDir), run(vpDir)
	if q := os.Getenv("CG_VP5_QUERY"); q != "" {
		// vP5 answering a weaker query, filtered here by span duration > CG_MIN_DUR_NS
		os.Setenv("CG_QUERY", q)
		minDur := uint64(atoi(os.Getenv("CG_MIN_DUR_NS")))
		all := run(vpDir)
		b = map[string]*tempopb.TraceSearchMetadata{}
		minTrace := uint32(atoi(os.Getenv("CG_MIN_TRACE_MS")))
		for id, tr := range all {
			if minTrace > 0 {
				if tr.DurationMs > minTrace {
					b[id] = tr
				}
				continue
			}
			for _, sp := range tr.SpanSets[0].Spans {
				if sp.DurationNanos > minDur {
					b[id] = tr
					break
				}
			}
		}
	}
	n := 0
	for id, tr := range a {
		if _, ok := b[id]; !ok {
			if n++; n <= 3 {
				t.Logf("only CG: %s dur=%dms %+v", id, tr.DurationMs, tr.SpanSets[0].Spans)
			}
		}
	}
	m := 0
	for id, tr := range b {
		if _, ok := a[id]; !ok {
			if m++; m <= 3 {
				t.Logf("only vP5: %s dur=%dms %+v", id, tr.DurationMs, tr.SpanSets[0].Spans)
			}
		}
	}
	t.Logf("CG %d traces, vP5 %d, only CG %d, only vP5 %d", len(a), len(b), n, m)
}

// TestBlockMetricsDiff runs a metrics query on both blocks and reports series whose totals differ.
func TestBlockMetricsDiff(t *testing.T) {
	cgDir, vpDir := os.Getenv("CG_BLOCK_DIR"), os.Getenv("CG_VP5_DIR")
	if cgDir == "" || vpDir == "" {
		t.Skip("CG_BLOCK_DIR / CG_VP5_DIR not set")
	}
	ctx := context.Background()
	query := os.Getenv("CG_QUERY")
	run := func(dir string) map[string]float64 {
		tenant := filepath.Base(filepath.Dir(dir))
		raw, _, _, err := local.New(&local.Config{Path: filepath.Dir(filepath.Dir(dir))})
		require.NoError(t, err)
		r := backend.NewReader(raw)
		meta, err := r.BlockMeta(ctx, uuid.MustParse(filepath.Base(dir)), tenant)
		require.NoError(t, err)
		blk, err := encoding.OpenBlock(meta, r)
		require.NoError(t, err)
		opts := common.DefaultSearchOptions()
		f := traceql.NewSpansetFetcherWrapperBoth(
			func(ctx context.Context, req traceql.FetchSpansRequest) (traceql.FetchSpansResponse, error) {
				return blk.Fetch(ctx, req, opts)
			},
			func(ctx context.Context, req traceql.FetchSpansRequest) (traceql.FetchSpansOnlyResponse, error) {
				t.Logf("FetchSpans %d conds all=%v %d second selectAll=%v", len(req.Conditions), req.AllConditions, len(req.SecondPassConditions), req.SecondPassSelectAll)
				return blk.FetchSpans(ctx, req, opts)
			})
		step := uint64(5 * time.Minute)
		start, end, step := traceql.TrimToBlockOverlap(&tempopb.QueryRangeRequest{Query: query, End: math.MaxInt64, Step: step}, meta.StartTime, meta.EndTime)
		eval, err := traceql.NewEngine().CompileMetricsQueryRange(&tempopb.QueryRangeRequest{Query: query, Start: start, End: end, Step: step})
		require.NoError(t, err)
		require.NoError(t, eval.Do(ctx, f, uint64(meta.StartTime.UnixNano()), uint64(meta.EndTime.UnixNano()), 0))
		out := map[string]float64{}
		for _, s := range eval.Results() {
			sum := 0.0
			for _, v := range s.Values {
				if !math.IsNaN(v) {
					sum += v
				}
			}
			out[s.Labels.String()] = sum
		}
		return out
	}
	a, b := run(cgDir), run(vpDir)
	n := 0
	for k, v := range a {
		if b[k] != v {
			if n++; n <= 10 {
				t.Logf("%s: CG %v vP5 %v", k, v, b[k])
			}
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			t.Logf("only vP5: %s", k)
		}
	}
	t.Logf("CG %d series, vP5 %d, %d differ", len(a), len(b), n)
}

package vcg1_test

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
	"github.com/grafana/tempo/v3/tempodb/encoding"
	"github.com/grafana/tempo/v3/tempodb/encoding/common"
)

func TestZZProf(t *testing.T) {
	dir := os.Getenv("CG_BLOCK_DIR")
	if dir == "" {
		t.Skip()
	}
	ctx := context.Background()
	query := os.Getenv("CG_QUERY")
	tenant := filepath.Base(filepath.Dir(dir))
	raw, _, _, err := local.New(&local.Config{Path: filepath.Dir(filepath.Dir(dir))})
	require.NoError(t, err)
	r := backend.NewReader(raw)
	meta, err := r.BlockMeta(ctx, uuid.MustParse(filepath.Base(dir)), tenant)
	require.NoError(t, err)
	blk, err := encoding.OpenBlock(meta, r)
	require.NoError(t, err)
	for i := 0; i < 10; i++ {
		t0 := time.Now()
		for p := 0; p < 5; p++ {
			opts := common.DefaultSearchOptions()
			opts.StartPage, opts.TotalPages = p, 1
			f := traceql.NewSpansetFetcherWrapperBoth(
				func(ctx context.Context, req traceql.FetchSpansRequest) (traceql.FetchSpansResponse, error) {
					return blk.Fetch(ctx, req, opts)
				},
				func(ctx context.Context, req traceql.FetchSpansRequest) (traceql.FetchSpansOnlyResponse, error) {
					return blk.FetchSpans(ctx, req, opts)
				})
			step := uint64(5 * time.Minute)
			start, end, step := traceql.TrimToBlockOverlap(&tempopb.QueryRangeRequest{Query: query, End: math.MaxInt64, Step: step}, meta.StartTime, meta.EndTime)
			eval, err := traceql.NewEngine().CompileMetricsQueryRange(&tempopb.QueryRangeRequest{Query: query, Start: start, End: end, Step: step})
			require.NoError(t, err)
			pt := time.Now()
			require.NoError(t, eval.Do(ctx, f, uint64(meta.StartTime.UnixNano()), uint64(meta.EndTime.UnixNano()), 0))
			if i == 0 {
				n := 0.0
				for _, s := range eval.Results() {
					for _, v := range s.Values {
						if !math.IsNaN(v) && v > 0 {
							n++
						}
					}
				}
				t.Logf("page %d: %v series=%d nonzero=%v", p, time.Since(pt), len(eval.Results()), n)
			}
		}
		if i == 0 {
			t.Logf("iter %d: %v", i, time.Since(t0))
		}
	}
}

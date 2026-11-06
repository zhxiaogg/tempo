package vcg1_test

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/grafana/tempo/v3/pkg/cache"
	"github.com/grafana/tempo/v3/pkg/tempopb"
	"github.com/grafana/tempo/v3/pkg/traceql"
	"github.com/grafana/tempo/v3/tempodb/backend"
	"github.com/grafana/tempo/v3/tempodb/encoding"
	"github.com/grafana/tempo/v3/tempodb/encoding/common"
	"github.com/grafana/tempo/v3/tempodb/encoding/vcg1"
	cg "github.com/grafana/tempo/v3/tempodb/encoding/vcg1/cgfile"
	"github.com/grafana/tempo/v3/tempodb/encoding/vcg1/internal/cgtest"
	"github.com/grafana/tempo/v3/tempodb/encoding/vparquet5"
)

// cutRowsIter is rowsIter with an anchor cut every `every` traces.
type cutRowsIter struct {
	rowsIter
	every int
}

func (it *cutRowsIter) CutAnchor() bool { return it.i > 1 && (it.i-1)%it.every == 0 }

// createAnchored writes traces as a vCG1 block with an anchor every `every` traces.
func createAnchored(t *testing.T, s store, traces []cgtest.Trace, every int) *backend.BlockMeta {
	conv := vparquet5.NewTraceConverter(cgtest.Dedicated)
	rows := make([]*vparquet5.Trace, len(traces))
	for i, tr := range traces {
		rows[i], _ = conv.Convert(tr.ID, tr.Trace, nil)
	}
	m := backend.NewBlockMeta("tenant", uuid.New(), vcg1.VersionString)
	m.DedicatedColumns = cgtest.Dedicated
	m.TotalObjects = int64(len(traces))
	m.StartTime = time.Unix(1_700_000_000, 0)
	m.EndTime = m.StartTime.Add(time.Hour)
	m.ReplicationFactor = 1
	cm, err := vcg1.CreateBlock(context.Background(), blockConfig(), m, &cutRowsIter{rowsIter: rowsIter{rows: rows}, every: every}, s.r, s.w)
	require.NoError(t, err)
	return cm
}

// TestSubBlockJobs: the frontend's StartPage/TotalPages jobs over a block (one page per
// anchor) answer search and tag values exactly as one job over the whole block.
func TestSubBlockJobs(t *testing.T) {
	ctx := context.Background()
	traces := cgtest.Traces(600, 3)
	s := newStore(t)
	cm := createAnchored(t, s, traces, 50)
	require.EqualValues(t, 12, cm.TotalRecords, "one page per anchor")
	blk, err := encoding.OpenBlock(cm, s.r)
	require.NoError(t, err)

	search := func(q string, start, pages int) []string {
		opts := common.DefaultSearchOptions()
		opts.StartPage, opts.TotalPages = start, pages
		f := traceql.NewSpansetFetcherWrapper(func(ctx context.Context, req traceql.FetchSpansRequest) (traceql.FetchSpansResponse, error) {
			return blk.Fetch(ctx, req, opts)
		})
		resp, err := traceql.NewEngine().ExecuteSearch(ctx, &tempopb.SearchRequest{Query: q, Limit: 1_000_000, SpansPerSpanSet: 1_000_000}, f)
		require.NoError(t, err)
		var out []string
		for _, tr := range resp.Traces {
			for _, ss := range tr.SpanSets {
				for _, sp := range ss.Spans {
					out = append(out, tr.TraceID+"/"+sp.SpanID)
				}
			}
		}
		return out
	}
	tagValues := func(tag string, start, pages int) []string {
		opts := common.DefaultSearchOptions()
		opts.StartPage, opts.TotalPages = start, pages
		var mu sync.Mutex
		seen := map[string]bool{}
		err := blk.SearchTagValuesV2(ctx, traceql.MustParseIdentifier(tag), func(v traceql.Static) bool {
			mu.Lock()
			seen[v.EncodeToString(false)] = true
			mu.Unlock()
			return false
		}, func(uint64) {}, opts)
		require.NoError(t, err)
		var out []string
		for v := range seen {
			out = append(out, v)
		}
		return out
	}
	sorted := func(xs []string) []string {
		xs = append([]string(nil), xs...)
		sort.Strings(xs)
		return xs
	}
	dedupe := func(xs []string) []string {
		xs = sorted(xs)
		out := xs[:0]
		for i, x := range xs {
			if i == 0 || x != xs[i-1] {
				out = append(out, x)
			}
		}
		return out
	}

	for _, pages := range []int{1, 5} { // one anchor per job, and jobs of several anchors
		for _, q := range []string{`{}`, `{ resource.service.name = "api" }`, `{ span.http.route = "/api/v1/a" && duration > 1ns }`} {
			whole := search(q, 0, 0)
			require.NotEmpty(t, whole, q)
			var union []string
			for start := 0; start < int(cm.TotalRecords); start += pages {
				union = append(union, search(q, start, pages)...)
			}
			require.Equal(t, sorted(whole), sorted(union), "%s, %d pages per job", q, pages)
		}
		for _, tag := range []string{"resource.service.name", "span.http.route", "resource.cluster"} {
			whole := tagValues(tag, 0, 0)
			require.NotEmpty(t, whole, tag)
			var union []string
			for start := 0; start < int(cm.TotalRecords); start += pages {
				union = append(union, tagValues(tag, start, pages)...)
			}
			require.Equal(t, sorted(whole), dedupe(union), "%s, %d pages per job", tag, pages)
		}
	}

	// past the last page: nothing
	require.Empty(t, search(`{}`, int(cm.TotalRecords), 1))
}

// roleRecorder records the cache role of every read of block.cg.
type roleRecorder struct {
	backend.Reader
	mu    sync.Mutex
	roles []cache.Role
	offs  []uint64
}

func (r *roleRecorder) ReadRange(ctx context.Context, name string, id uuid.UUID, tenant string, off uint64, b []byte, ci *backend.CacheInfo) error {
	if name == cg.ObjectName {
		r.mu.Lock()
		role := cache.Role("")
		if ci != nil {
			role = ci.Role
		}
		r.roles, r.offs = append(r.roles, role), append(r.offs, off)
		r.mu.Unlock()
	}
	return r.Reader.ReadRange(ctx, name, id, tenant, off, b, ci)
}

// TestFooterReadUsesFooterCache: every request starts with one read of exactly the footer and
// trailer through the footer cache role; data reads carry no cache role.
func TestFooterReadUsesFooterCache(t *testing.T) {
	ctx := context.Background()
	traces := cgtest.Traces(300, 4)
	s := newStore(t)
	cm := createAnchored(t, s, traces, 50)
	rec := &roleRecorder{Reader: s.r}
	blk, err := encoding.OpenBlock(cm, rec)
	require.NoError(t, err)
	footerOff := cm.Size_ - uint64(cm.FooterSize) - cg.TrailerLen

	res, err := blk.FindTraceByID(ctx, traces[7].ID, common.DefaultSearchOptions())
	require.NoError(t, err)
	require.NotNil(t, res.Trace)
	require.Equal(t, cache.RoleParquetFooter, rec.roles[0])
	require.Equal(t, footerOff, rec.offs[0])
	require.GreaterOrEqual(t, res.Metrics.InspectedBytes, uint64(cm.FooterSize)+cg.TrailerLen, "the footer read counts as inspected")
	for _, r := range rec.roles[1:] {
		require.NotEqual(t, cache.RoleParquetFooter, r)
	}

	rec.roles, rec.offs = nil, nil
	opts := common.DefaultSearchOptions()
	opts.StartPage, opts.TotalPages = 2, 1
	f := traceql.NewSpansetFetcherWrapper(func(ctx context.Context, req traceql.FetchSpansRequest) (traceql.FetchSpansResponse, error) {
		return blk.Fetch(ctx, req, opts)
	})
	_, err = traceql.NewEngine().ExecuteSearch(ctx, &tempopb.SearchRequest{Query: `{}`, Limit: 100}, f)
	require.NoError(t, err)
	require.Equal(t, cache.RoleParquetFooter, rec.roles[0])
	require.Equal(t, footerOff, rec.offs[0])
}

package cgwrite_test

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bits-and-blooms/bloom/v3"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/grafana/tempo/v3/pkg/tempopb"
	"github.com/grafana/tempo/v3/pkg/traceql"
	"github.com/grafana/tempo/v3/tempodb/backend"
	"github.com/grafana/tempo/v3/tempodb/backend/local"
	"github.com/grafana/tempo/v3/tempodb/encoding"
	"github.com/grafana/tempo/v3/tempodb/encoding/common"
	"github.com/grafana/tempo/v3/tempodb/encoding/vcg1"
	"github.com/grafana/tempo/v3/tempodb/encoding/vcg1/internal/cgtest"
	"github.com/grafana/tempo/v3/tempodb/encoding/vparquet5"
)

func blockConfig() *common.BlockConfig {
	return &common.BlockConfig{BloomFP: 0.01, BloomShardSizeBytes: 100 * 1024, RowGroupSizeBytes: 1 << 20}
}

// create writes traces as a block of version v through Tempo's local backend.
func create(t *testing.T, v string, traces []cgtest.Trace, dc backend.DedicatedColumns) (common.BackendBlock, *backend.BlockMeta, backend.Reader, backend.Writer) {
	rr, ww, _, err := local.New(&local.Config{Path: t.TempDir()})
	require.NoError(t, err)
	r, w := backend.NewReader(rr), backend.NewWriter(ww)
	enc, err := encoding.FromVersion(v)
	require.NoError(t, err)
	require.True(t, enc.WritesSupported())
	meta := backend.NewBlockMeta("tenant", uuid.New(), v)
	meta.DedicatedColumns = dc
	meta.TotalObjects = int64(len(traces))
	meta.StartTime = time.Unix(0, 1_700_000_000_000_000_000)
	meta.EndTime = meta.StartTime.Add(2 * time.Hour)
	meta.ReplicationFactor = 1
	newMeta, err := enc.CreateBlock(context.Background(), blockConfig(), meta, &cgtest.Iter{Traces: traces}, r, w)
	require.NoError(t, err)
	// as a querier sees it: the meta read back from the backend
	got, err := r.BlockMeta(context.Background(), uuid.UUID(newMeta.BlockID), newMeta.TenantID)
	require.NoError(t, err)
	blk, err := encoding.OpenBlock(got, r)
	require.NoError(t, err)
	return blk, got, r, w
}

var searchQueries = []string{
	`{ span.node = "alpha" }`,
	`{ resource.plan = "beta" }`,
	`{ event.zone = "gamma" }`,
	`{ name = "HTTP GET" && span.http.method = "alpha" }`,
	`{ span.http.status_code = 5 }`,
	`{ span.http.route =~ "/api/v1/.*" }`,
	`{ span.http.route = 5 }`,
	`{ span.blockSize > 100 }`,
	`{ resource.cluster = "eu" }`,
	`{ resource.cluster = 3 }`,
	`{ resource.process.pid > 50000 }`,
	`{ resource.service.name = "grafana" && kind = server }`,
	`{ event:name = "exception" && event.exception.type =~ ".*Error" }`,
	`{ span.node = "alpha" || span.node = "beta" }`,
	`{ span.user =~ "uniq-.*" }`,
	`{ .zone = "alpha" }`,
	// not `{ span.txn = nil }`: vParquet5's generic nil iterator matches the spans with no
	// generic attributes at all, vCG1 the spans without span.txn (a reader difference)
	`{ span.txn != nil }`,
	`{ span.id != nil && status = error }`,
	`{ link.link.a = "delta" }`,
	`{ instrumentation.scope.a = "alpha" }`,
	`{ span.node = "alpha" } >> { status = error }`,
	`{}`,
}

var metricsQueries = []string{
	`{} | rate() by (resource.cluster)`,
	`{ span.node = "alpha" } | count_over_time()`,
	// Group-bys avoid generic int/double/bool arrays (vParquet5's FetchSpans aliases their
	// value buffers across spans) and dedicated columns (vParquet5 misattributes a few spans
	// on this block: see TestCreateBlock/metrics-dedicated, which checks against the traces).
	`{ event.zone = "beta" } | count_over_time() by (span.kind)`,
	`{} | count_over_time() by (name)`,
	`{} | rate() by (resource.service.name)`,
}

func search(t *testing.T, blk common.BackendBlock, q string, limit, spss uint32, opts common.SearchOptions) []*tempopb.TraceSearchMetadata {
	f := traceql.NewSpansetFetcherWrapper(func(ctx context.Context, req traceql.FetchSpansRequest) (traceql.FetchSpansResponse, error) {
		return blk.Fetch(ctx, req, opts)
	})
	resp, err := traceql.NewEngine().ExecuteSearch(context.Background(), &tempopb.SearchRequest{Query: q, Limit: limit, SpansPerSpanSet: spss}, f)
	require.NoError(t, err, q)
	out := resp.Traces
	sort.Slice(out, func(i, j int) bool { return out[i].TraceID < out[j].TraceID })
	for _, tr := range out {
		for _, ss := range append(tr.SpanSets, tr.SpanSet) {
			if ss == nil {
				continue
			}
			sort.Slice(ss.Spans, func(i, j int) bool { return ss.Spans[i].SpanID < ss.Spans[j].SpanID })
			for _, sp := range ss.Spans {
				sort.Slice(sp.Attributes, func(i, j int) bool {
					if sp.Attributes[i].Key != sp.Attributes[j].Key {
						return sp.Attributes[i].Key < sp.Attributes[j].Key
					}
					return sp.Attributes[i].Value.String() < sp.Attributes[j].Value.String()
				})
			}
		}
	}
	return out
}

// sameSearch reports the first trace whose search metadata differs.
func sameSearch(t *testing.T, q string, want, got []*tempopb.TraceSearchMetadata) {
	t.Helper()
	byID := map[string]*tempopb.TraceSearchMetadata{}
	for _, tr := range got {
		byID[tr.TraceID] = tr
	}
	for _, w := range want {
		g := byID[w.TraceID]
		wj, _ := json.Marshal(w)
		gj, _ := json.Marshal(g)
		if !bytes.Equal(wj, gj) {
			t.Errorf("%s: trace %s\nvP5  %s\nvCG1 %s", q, w.TraceID, wj, gj)
			return
		}
	}
	assert.Len(t, got, len(want), q)
}

func metrics(t *testing.T, blk common.BackendBlock, meta *backend.BlockMeta, q string) map[string][]float64 {
	opts := common.DefaultSearchOptions()
	f := traceql.NewSpansetFetcherWrapperBoth(
		func(ctx context.Context, req traceql.FetchSpansRequest) (traceql.FetchSpansResponse, error) {
			return blk.Fetch(ctx, req, opts)
		},
		func(ctx context.Context, req traceql.FetchSpansRequest) (traceql.FetchSpansOnlyResponse, error) {
			return blk.FetchSpans(ctx, req, opts)
		})
	start, end, step := traceql.TrimToBlockOverlap(&tempopb.QueryRangeRequest{Query: q, End: math.MaxInt64, Step: uint64(5 * time.Minute)}, meta.StartTime, meta.EndTime)
	eval, err := traceql.NewEngine().CompileMetricsQueryRange(&tempopb.QueryRangeRequest{Query: q, Start: start, End: end, Step: step})
	require.NoError(t, err, q)
	require.NoError(t, eval.Do(context.Background(), f, uint64(meta.StartTime.UnixNano()), uint64(meta.EndTime.UnixNano()), 0), q)
	out := map[string][]float64{}
	for _, s := range eval.Results() {
		out[s.Labels.String()] = s.Values
	}
	return out
}

// TestCreateBlock writes a block-builder sized block through CreateBlock and Tempo's local
// backend and checks that it answers like the vParquet5 block of the same traces.
func TestCreateBlock(t *testing.T) {
	traces := cgtest.Traces(5000, 7)
	cgBlk, cgMeta, cgR, _ := create(t, vcg1.VersionString, traces, cgtest.Dedicated)
	vpBlk, vpMeta, _, _ := create(t, vparquet5.VersionString, traces, cgtest.Dedicated)
	ctx := context.Background()

	require.Equal(t, vcg1.VersionString, cgMeta.Version)
	require.Equal(t, int64(len(traces)), cgMeta.TotalObjects)
	require.Equal(t, vpMeta.DedicatedColumns, cgMeta.DedicatedColumns)
	require.Equal(t, vpMeta.StartTime, cgMeta.StartTime)
	require.Equal(t, vpMeta.EndTime, cgMeta.EndTime)
	require.Equal(t, vpMeta.BloomShardCount, cgMeta.BloomShardCount)
	require.Positive(t, cgMeta.TotalRecords)
	require.Positive(t, cgMeta.FooterSize)
	require.NoError(t, cgBlk.Validate(ctx))
	t.Logf("vCG1 %d bytes, footer %d, %d anchors; vParquet5 %d bytes", cgMeta.Size_, cgMeta.FooterSize, cgMeta.TotalRecords, vpMeta.Size_)

	t.Run("FindTraceByID", func(t *testing.T) {
		// Expected: vParquet5's own conversion of each trace (fresh rows). vParquet5's
		// CreateBlock reuses one row for every trace, and once sortTrace has swapped spans the
		// reused dedicated-column slices alias across positions, so its block can return
		// another span's dedicated value; those traces are counted, not compared.
		conv := vparquet5.NewTraceConverter(cgtest.Dedicated)
		vpBad := 0
		for i, tr := range traces {
			row, _ := conv.Convert(tr.ID, tr.Trace, nil)
			want := cgtest.Canon(vparquet5.ParquetTraceToTempopbTrace(cgMeta, row))
			got, err := cgBlk.FindTraceByID(ctx, tr.ID, common.DefaultSearchOptions())
			require.NoError(t, err)
			require.NotNil(t, got.Trace, "trace %d", i)
			require.Equal(t, want, cgtest.Canon(got.Trace), "trace %d %x", i, tr.ID)
			if i%5 == 0 {
				vp, err := vpBlk.FindTraceByID(ctx, tr.ID, common.DefaultSearchOptions())
				require.NoError(t, err)
				if !bytes.Equal(want, cgtest.Canon(vp.Trace)) {
					vpBad++
				}
			}
		}
		t.Logf("vParquet5 FindTraceByID differs from its own conversion on %d of %d sampled traces", vpBad, (len(traces)+4)/5)
		// an absent id: ruled out by the bloom filter (nil response, as vparquet5) or not found
		got, err := cgBlk.FindTraceByID(ctx, bytes.Repeat([]byte{0xfe}, 16), common.DefaultSearchOptions())
		require.NoError(t, err)
		require.True(t, got == nil || got.Trace == nil)
	})

	t.Run("bloom", func(t *testing.T) {
		// vParquet5's bloom readers: every trace id passes its shard
		for _, tr := range traces[:200] {
			shard := common.ShardKeyForTraceID(tr.ID, int(cgMeta.BloomShardCount))
			b, err := cgR.Read(ctx, common.BloomName(shard), uuid.UUID(cgMeta.BlockID), cgMeta.TenantID, nil)
			require.NoError(t, err)
			f := &bloom.BloomFilter{}
			_, err = f.ReadFrom(bytes.NewReader(b))
			require.NoError(t, err)
			require.True(t, f.Test(tr.ID))
		}
	})

	t.Run("search", func(t *testing.T) {
		for _, q := range searchQueries {
			if usesDedicated(q) {
				continue // vParquet5's block has wrong dedicated values (see FindTraceByID); TestCreateBlockGeneric covers these
			}
			want := search(t, vpBlk, q, 0, 1000, common.DefaultSearchOptions())
			got := search(t, cgBlk, q, 0, 1000, common.DefaultSearchOptions())
			sameSearch(t, q, want, got)
			sameSearch(t, q+" (limit 20, 3 spans)", search(t, vpBlk, q, 20, 3, common.DefaultSearchOptions()), search(t, cgBlk, q, 20, 3, common.DefaultSearchOptions()))
			t.Logf("%s: %d traces", q, len(got))
		}
	})

	t.Run("shards", func(t *testing.T) {
		// StartPage/TotalPages over anchors: the shards partition the block
		q := `{ span.node = "alpha" }`
		all := search(t, cgBlk, q, 0, 1000, common.DefaultSearchOptions())
		var parts []*tempopb.TraceSearchMetadata
		for p := 0; p < int(cgMeta.TotalRecords); p += 2 {
			opts := common.DefaultSearchOptions()
			opts.StartPage, opts.TotalPages = p, 2
			parts = append(parts, search(t, cgBlk, q, 0, 1000, opts)...)
		}
		sort.Slice(parts, func(i, j int) bool { return parts[i].TraceID < parts[j].TraceID })
		require.Equal(t, all, parts)
	})

	t.Run("metrics", func(t *testing.T) {
		for _, q := range metricsQueries {
			want, got := metrics(t, vpBlk, vpMeta, q), metrics(t, cgBlk, cgMeta, q)
			for k, v := range want {
				if !assert.Equal(t, v, got[k], "%s: series %s", q, k) {
					break
				}
			}
			assert.Len(t, got, len(want), q)
			t.Logf("%s: %d series", q, len(got))
		}
	})

	t.Run("metrics-dedicated", func(t *testing.T) {
		want := map[string]float64{}
		conv := vparquet5.NewTraceConverter(cgtest.Dedicated)
		for _, tr := range traces {
			row, _ := conv.Convert(tr.ID, tr.Trace, nil)
			for _, rs := range row.ResourceSpans {
				for _, ss := range rs.ScopeSpans {
					for _, sp := range ss.Spans {
						k := "<nil>"
						if v := sp.DedicatedAttributes.String01; len(v) == 1 { // http.route
							k = v[0]
						} else if len(v) > 1 {
							t.Fatal("unexpected multi-valued http.route")
						}
						want[`{"span.http.route"="`+k+`"}`]++
					}
				}
			}
		}
		got := map[string]float64{}
		for k, vs := range metrics(t, cgBlk, cgMeta, `{} | count_over_time() by (span.http.route)`) {
			for _, v := range vs {
				got[k] += v
			}
		}
		require.Equal(t, want, got)
	})

	t.Run("copy", func(t *testing.T) {
		rr, ww, _, err := local.New(&local.Config{Path: t.TempDir()})
		require.NoError(t, err)
		r2, w2 := backend.NewReader(rr), backend.NewWriter(ww)
		require.NoError(t, (vcg1.Encoding{}).CopyBlock(ctx, cgMeta, cgR, w2))
		m2, err := r2.BlockMeta(ctx, uuid.UUID(cgMeta.BlockID), cgMeta.TenantID)
		require.NoError(t, err)
		b2, err := encoding.OpenBlock(m2, r2)
		require.NoError(t, err)
		require.NoError(t, b2.Validate(ctx))
		got, err := b2.FindTraceByID(ctx, traces[10].ID, common.DefaultSearchOptions())
		require.NoError(t, err)
		require.NotNil(t, got.Trace)
	})
}

func usesDedicated(q string) bool {
	for _, d := range cgtest.Dedicated {
		if strings.Contains(q, "."+d.Name) {
			return true
		}
	}
	return false
}

// TestCreateBlockGeneric compares every query with vParquet5 on blocks without dedicated
// columns (vParquet5's CreateBlock writes them correctly then).
func TestCreateBlockGeneric(t *testing.T) {
	traces := cgtest.Traces(3000, 11)
	cgBlk, cgMeta, _, _ := create(t, vcg1.VersionString, traces, nil)
	vpBlk, vpMeta, _, _ := create(t, vparquet5.VersionString, traces, nil)
	ctx := context.Background()
	for i, tr := range traces {
		if i%3 != 0 {
			continue
		}
		want, err := vpBlk.FindTraceByID(ctx, tr.ID, common.DefaultSearchOptions())
		require.NoError(t, err)
		got, err := cgBlk.FindTraceByID(ctx, tr.ID, common.DefaultSearchOptions())
		require.NoError(t, err)
		require.Equal(t, cgtest.Canon(want.Trace), cgtest.Canon(got.Trace), "trace %d %x", i, tr.ID)
	}
	for _, q := range searchQueries {
		sameSearch(t, q, search(t, vpBlk, q, 0, 1000, common.DefaultSearchOptions()), search(t, cgBlk, q, 0, 1000, common.DefaultSearchOptions()))
		sameSearch(t, q+" (limit 20, 3 spans)", search(t, vpBlk, q, 20, 3, common.DefaultSearchOptions()), search(t, cgBlk, q, 20, 3, common.DefaultSearchOptions()))
	}
	for _, q := range append(metricsQueries, `{} | count_over_time() by (span.http.route)`) {
		want, got := metrics(t, vpBlk, vpMeta, q), metrics(t, cgBlk, cgMeta, q)
		require.Equal(t, want, got, q)
	}
}

func TestCreateBlockEmpty(t *testing.T) {
	blk, meta, _, _ := create(t, vcg1.VersionString, nil, cgtest.Dedicated)
	require.Zero(t, meta.TotalObjects)
	require.NoError(t, blk.Validate(context.Background()))
	got, err := blk.FindTraceByID(context.Background(), bytes.Repeat([]byte{1}, 16), common.DefaultSearchOptions())
	require.NoError(t, err)
	require.Nil(t, got, "ruled out by the (empty) bloom filter")
	require.Empty(t, search(t, blk, `{}`, 0, 3, common.DefaultSearchOptions()))
}

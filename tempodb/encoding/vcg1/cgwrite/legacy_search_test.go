package cgwrite_test

import (
	"context"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/grafana/tempo/v3/pkg/tempopb"
	v1 "github.com/grafana/tempo/v3/pkg/tempopb/common/v1"
	"github.com/grafana/tempo/v3/pkg/util"
	"github.com/grafana/tempo/v3/tempodb/encoding/common"
	"github.com/grafana/tempo/v3/tempodb/encoding/vcg1"
	"github.com/grafana/tempo/v3/tempodb/encoding/vcg1/internal/cgtest"
	"github.com/grafana/tempo/v3/tempodb/encoding/vparquet5"
)

// TestLegacySearch: a tag search (no TraceQL query) finds on vCG1 the traces vParquet5 finds,
// with the same metadata. A generic tag can miss a trace vParquet5 finds only where one span or
// resource repeats the key: TraceQL (vCG1's search) sees one value per key, vParquet5's tag
// search any of them.
func TestLegacySearch(t *testing.T) {
	traces := cgtest.Traces(400, 11)
	cgBlk, cgMeta, _, _ := create(t, vcg1.VersionString, traces, cgtest.Dedicated)
	vpBlk, _, _, _ := create(t, vparquet5.VersionString, traces, cgtest.Dedicated)
	start, end := uint32(cgMeta.StartTime.Unix()), uint32(cgMeta.EndTime.Unix())

	run := func(blk common.BackendBlock, req *tempopb.SearchRequest) []*tempopb.TraceSearchMetadata {
		resp, err := blk.Search(context.Background(), req, common.DefaultSearchOptions())
		require.NoError(t, err)
		out := resp.Traces
		sort.Slice(out, func(i, j int) bool { return out[i].TraceID < out[j].TraceID })
		return out
	}
	byID := map[string]cgtest.Trace{}
	for _, tr := range traces {
		byID[util.TraceIDToHexString(tr.ID)] = tr
	}
	repeats := func(tr cgtest.Trace, key string) bool {
		n := func(kvs []*v1.KeyValue) int {
			c := 0
			for _, kv := range kvs {
				if kv.Key == key {
					c++
				}
			}
			return c
		}
		for _, rs := range tr.Trace.ResourceSpans {
			if n(rs.Resource.Attributes) > 1 {
				return true
			}
			for _, ss := range rs.ScopeSpans {
				for _, sp := range ss.Spans {
					if n(sp.Attributes) > 1 {
						return true
					}
				}
			}
		}
		return false
	}
	for _, tc := range []struct {
		name    string
		req     *tempopb.SearchRequest
		generic string // a generic tag key: misses allowed where it repeats
	}{
		{"everything", &tempopb.SearchRequest{}, ""},
		{"service name substring", &tempopb.SearchRequest{Tags: map[string]string{"service.name": "ap"}}, ""},
		{"span name substring", &tempopb.SearchRequest{Tags: map[string]string{"name": "GET"}}, ""},
		{"status", &tempopb.SearchRequest{Tags: map[string]string{"status.code": "error"}}, ""},
		{"root service", &tempopb.SearchRequest{Tags: map[string]string{"root.service.name": "web"}}, ""},
		{"root name", &tempopb.SearchRequest{Tags: map[string]string{"root.name": "query"}}, ""},
		{"dedicated span column", &tempopb.SearchRequest{Tags: map[string]string{"http.route": "v1"}}, ""},
		{"dedicated resource column", &tempopb.SearchRequest{Tags: map[string]string{"cluster": "e"}}, ""},
		{"dedicated int column, non-int value", &tempopb.SearchRequest{Tags: map[string]string{"blockSize": "alpha"}}, ""},
		{"generic span attribute", &tempopb.SearchRequest{Tags: map[string]string{"node": "alpha"}}, "node"},
		{"generic resource attribute", &tempopb.SearchRequest{Tags: map[string]string{"zone": "beta"}}, "zone"},
		{"http status code as a string", &tempopb.SearchRequest{Tags: map[string]string{"http.status_code": "gamma"}}, "http.status_code"},
		{"no match", &tempopb.SearchRequest{Tags: map[string]string{"node": "nope"}}, ""},
		{"tag and span name", &tempopb.SearchRequest{Tags: map[string]string{"service.name": "api", "node": "delta"}}, "node"},
		{"duration", &tempopb.SearchRequest{MinDurationMs: 1, MaxDurationMs: 50}, ""},
		{"time range", &tempopb.SearchRequest{Start: start, End: start + (end-start)/2}, ""},
		{"limit", &tempopb.SearchRequest{Tags: map[string]string{"service.name": "a"}, Limit: 1000}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := run(vpBlk, tc.req)
			got := run(cgBlk, tc.req)
			if tc.generic != "" {
				found := map[string]*tempopb.TraceSearchMetadata{}
				for _, g := range got {
					found[g.TraceID] = g
				}
				kept := want[:0:0]
				for _, w := range want {
					if found[w.TraceID] == nil && repeats(byID[w.TraceID], tc.generic) {
						continue
					}
					kept = append(kept, w)
				}
				t.Logf("%d of %d traces missed where %q repeats", len(want)-len(kept), len(want), tc.generic)
				want = kept
			}
			require.Equal(t, want, got)
			t.Logf("%d traces", len(got))
		})
	}
	// a limit caps the count
	require.Len(t, run(cgBlk, &tempopb.SearchRequest{Limit: 7}), 7)
}

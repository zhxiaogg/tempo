package frontend

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/gogo/protobuf/jsonpb"
	"github.com/gogo/protobuf/proto"
	"github.com/gorilla/mux"
	"github.com/grafana/dskit/user"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/grafana/tempo/v3/modules/frontend/combiner"
	"github.com/grafana/tempo/v3/modules/frontend/pipeline"
	"github.com/grafana/tempo/v3/modules/overrides"
	"github.com/grafana/tempo/v3/pkg/api"
	pkgapi "github.com/grafana/tempo/v3/pkg/api"
	"github.com/grafana/tempo/v3/pkg/tempopb"
	"github.com/grafana/tempo/v3/pkg/traceql"
	"github.com/grafana/tempo/v3/tempodb/backend"
)

func TestResolveBlockFormat(t *testing.T) {
	hints := func(q string) *traceql.Hints {
		expr, err := traceql.ParseNoOptimizations(q)
		require.NoError(t, err)
		return expr.Hints
	}
	tcs := []struct {
		name    string
		hints   *traceql.Hints
		url     string
		def     string
		want    string
		wantErr string
	}{
		{name: "hint wins over param and default", hints: hints(`{} with(block_format="vParquet5")`), url: "/?blockFormat=vCG1", def: "vCG1", want: "vParquet5"},
		{name: "param without hint", url: "/?blockFormat=vParquet5", def: "vCG1", want: "vParquet5"},
		{name: "default", url: "/", def: "vCG1", want: "vCG1"},
		{name: "no default", url: "/", want: ""},
		{name: "nil request and hints", def: "vCG1", want: "vCG1"},
		{name: "unknown hint", hints: hints(`{} with(block_format="vparquet5")`), url: "/", wantErr: "vparquet5"},
		{name: "unknown param", url: "/?blockFormat=cg", wantErr: "cg"},
		{name: "empty hint", hints: hints(`{} with(block_format="")`), url: "/", def: "vCG1", wantErr: "block_format"},
		{name: "hint of another type", hints: hints(`{} with(block_format=5)`), url: "/", def: "vCG1", wantErr: "block_format"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			if tc.url != "" {
				r = httptest.NewRequest("GET", tc.url, nil)
			} else {
				r = nil
			}
			got, err := resolveBlockFormat(tc.hints, r, tc.def)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestWithBlockFormat(t *testing.T) {
	metas := []*backend.BlockMeta{{Version: "vParquet5"}, {Version: "vCG1"}}
	acceptAll := func(*backend.BlockMeta) bool { return true }
	rejectAll := func(*backend.BlockMeta) bool { return false }
	keep := func(f func(*backend.BlockMeta) bool) []string {
		var out []string
		for _, m := range metas {
			if f(m) {
				out = append(out, m.Version)
			}
		}
		return out
	}
	require.Equal(t, []string{"vParquet5", "vCG1"}, keep(withBlockFormat("", acceptAll)))
	require.Equal(t, []string{"vCG1"}, keep(withBlockFormat("vCG1", acceptAll)))
	require.Empty(t, keep(withBlockFormat("vCG1", rejectAll)))
}

// formatMetas is one vParquet5 and one vCG1 block of the same time range, ended before
// QueryBackendAfter so search and metrics read them from the backend.
func formatMetas(now time.Time) []*backend.BlockMeta {
	mk := func(id, version string) *backend.BlockMeta {
		return &backend.BlockMeta{
			BlockID:           backend.MustParse(id),
			TenantID:          "test-tenant",
			Version:           version,
			StartTime:         now.Add(-2 * time.Hour),
			EndTime:           now.Add(-90 * time.Minute),
			TotalRecords:      1,
			Size_:             1000,
			ReplicationFactor: backend.MetricsGeneratorReplicationFactor,
		}
	}
	return []*backend.BlockMeta{
		mk("00000000-0000-0000-0000-000000000001", "vParquet5"),
		mk("00000000-0000-0000-0000-000000000002", "vCG1"),
	}
}

// blockRecorder is a next round tripper that records the version of the block each
// backend job reads (by block id) and answers every job with an empty response.
type blockRecorder struct {
	mtx      sync.Mutex
	byID     map[string]string
	versions []string
}

func newBlockRecorder(metas []*backend.BlockMeta) *blockRecorder {
	r := &blockRecorder{byID: map[string]string{}}
	for _, m := range metas {
		r.byID[m.BlockID.String()] = m.Version
	}
	return r
}

func (b *blockRecorder) next(body string) pipeline.AsyncRoundTripper[combiner.PipelineResponse] {
	return pipeline.AsyncRoundTripperFunc[combiner.PipelineResponse](func(r pipeline.Request) (pipeline.Responses[combiner.PipelineResponse], error) {
		if id := r.HTTPRequest().URL.Query().Get("blockID"); id != "" {
			b.mtx.Lock()
			b.versions = append(b.versions, b.byID[id])
			b.mtx.Unlock()
		}
		return pipeline.NewHTTPToAsyncResponse(&http.Response{Body: io.NopCloser(strings.NewReader(body)), StatusCode: 200}), nil
	})
}

func (b *blockRecorder) read() []string {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	out := slices.Clone(b.versions)
	slices.Sort(out)
	return slices.Compact(out)
}

func drain(t *testing.T, resps pipeline.Responses[combiner.PipelineResponse]) *http.Response {
	t.Helper()
	for {
		r, done, err := resps.Next(context.Background())
		require.NoError(t, err)
		if r != nil && r.HTTPResponse() != nil && r.HTTPResponse().StatusCode == http.StatusBadRequest {
			return r.HTTPResponse()
		}
		if done {
			return nil
		}
	}
}

var blockFormatCases = []struct {
	name   string
	hint   string
	def    string
	want   []string
	status int
}{
	{name: "hint", hint: "vParquet5", def: "vCG1", want: []string{"vParquet5"}},
	{name: "default", def: "vCG1", want: []string{"vCG1"}},
	{name: "hint without default", hint: "vParquet5", want: []string{"vParquet5"}},
	{name: "neither", want: []string{"vCG1", "vParquet5"}},
	{name: "unknown hint", hint: "bogus", def: "vCG1", status: http.StatusBadRequest},
}

func withHint(q, hint string) string {
	if hint == "" {
		return q
	}
	return q + ` with(block_format="` + hint + `")`
}

func TestSearchSharderBlockFormat(t *testing.T) {
	o, err := overrides.NewOverrides(overrides.Config{}, nil, prometheus.NewRegistry())
	require.NoError(t, err)
	body, err := (&jsonpb.Marshaler{}).MarshalToString(&tempopb.SearchResponse{Metrics: &tempopb.SearchMetrics{}})
	require.NoError(t, err)

	for _, tc := range blockFormatCases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			metas := formatMetas(now)
			rec := newBlockRecorder(metas)
			sharder := newAsyncSearchSharder(&mockReader{metas: metas}, o, SearchSharderConfig{
				ConcurrentRequests:    defaultConcurrentRequests,
				TargetBytesPerRequest: defaultTargetBytesPerRequest,
				MostRecentShards:      defaultMostRecentShards,
				QueryBackendAfter:     time.Hour,
				IngesterShards:        1,
				DefaultBlockFormat:    tc.def,
			}, nil, newJobsPerQueryHistogram(), log.NewNopLogger())

			q := url.QueryEscape(withHint("{}", tc.hint))
			req := httptest.NewRequest("GET", fmt.Sprintf("/?start=%d&end=%d&q=%s", now.Add(-3*time.Hour).Unix(), now.Add(-80*time.Minute).Unix(), q), nil)
			req = req.WithContext(user.InjectOrgID(req.Context(), "test-tenant"))
			resps, err := sharder.Wrap(rec.next(body)).RoundTrip(pipeline.NewHTTPRequest(req))
			require.NoError(t, err)
			bad := drain(t, resps)
			if tc.status != 0 {
				require.NotNil(t, bad)
				return
			}
			require.Nil(t, bad)
			require.Equal(t, tc.want, rec.read())
		})
	}
}

func TestQueryRangeSharderBlockFormat(t *testing.T) {
	o, err := overrides.NewOverrides(overrides.Config{}, nil, prometheus.NewRegistry())
	require.NoError(t, err)
	body, err := (&jsonpb.Marshaler{}).MarshalToString(&tempopb.QueryRangeResponse{Metrics: &tempopb.SearchMetrics{}})
	require.NoError(t, err)

	for _, tc := range blockFormatCases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			metas := formatMetas(now)
			rec := newBlockRecorder(metas)
			sharder := newAsyncQueryRangeSharder(&mockReader{metas: metas}, o, QueryRangeSharderConfig{
				ConcurrentRequests:    defaultConcurrentRequests,
				TargetBytesPerRequest: defaultTargetBytesPerRequest,
				QueryBackendAfter:     time.Hour,
				DefaultBlockFormat:    tc.def,
			}, nil, false, nil, log.NewNopLogger())

			httpReq := api.BuildQueryRangeRequest(httptest.NewRequest("GET", "/", nil), &tempopb.QueryRangeRequest{
				Query: withHint("{} | rate()", tc.hint),
				Start: uint64(now.Add(-3 * time.Hour).UnixNano()),
				End:   uint64(now.UnixNano()),
				Step:  uint64(time.Minute),
			}, "")
			httpReq = httpReq.WithContext(user.InjectOrgID(httpReq.Context(), "test-tenant"))
			resps, err := sharder.Wrap(rec.next(body)).RoundTrip(pipeline.NewHTTPRequest(httpReq))
			require.NoError(t, err)
			bad := drain(t, resps)
			if tc.status != 0 {
				require.NotNil(t, bad)
				return
			}
			require.Nil(t, bad)
			require.Equal(t, tc.want, rec.read())
		})
	}
}

var paramFormatCases = []struct {
	name   string
	param  string
	def    string
	want   []string
	status int
}{
	{name: "param", param: "vParquet5", def: "vCG1", want: []string{"vParquet5"}},
	{name: "default", def: "vCG1", want: []string{"vCG1"}},
	{name: "param without default", param: "vParquet5", want: []string{"vParquet5"}},
	{name: "neither", want: []string{"vCG1", "vParquet5"}},
	{name: "unknown param", param: "bogus", def: "vCG1", status: http.StatusBadRequest},
}

func TestTagsSharderBlockFormat(t *testing.T) {
	o, err := overrides.NewOverrides(overrides.Config{}, nil, prometheus.NewRegistry())
	require.NoError(t, err)

	for _, api := range []struct {
		name  string
		path  string
		parse parseRequestFunction
		body  proto.Message
	}{
		{"tags", "/api/v2/search/tags", parseTagsRequest, &tempopb.SearchTagsV2Response{}},
		{"tag values", "/api/v2/search/tag/span.foo/values", parseTagValuesRequestV2, &tempopb.SearchTagValuesV2Response{}},
	} {
		body, err := (&jsonpb.Marshaler{}).MarshalToString(api.body)
		require.NoError(t, err)
		for _, tc := range paramFormatCases {
			t.Run(api.name+"/"+tc.name, func(t *testing.T) {
				now := time.Now()
				metas := formatMetas(now)
				rec := newBlockRecorder(metas)
				sharder := newAsyncTagSharder(&mockReader{metas: metas}, o, SearchSharderConfig{
					ConcurrentRequests:    defaultConcurrentRequests,
					TargetBytesPerRequest: defaultTargetBytesPerRequest,
					QueryBackendAfter:     time.Hour,
					DefaultBlockFormat:    tc.def,
				}, api.parse, newJobsPerQueryHistogram(), log.NewNopLogger())

				path := fmt.Sprintf("%s?start=%d&end=%d", api.path, now.Add(-3*time.Hour).Unix(), now.Add(-80*time.Minute).Unix())
				if tc.param != "" {
					path += "&blockFormat=" + tc.param
				}
				req := httptest.NewRequest("GET", path, nil)
				req = mux.SetURLVars(req, map[string]string{pkgapi.MuxVarTagName: "span.foo"})
				req = req.WithContext(user.InjectOrgID(req.Context(), "test-tenant"))
				resps, err := sharder.Wrap(rec.next(body)).RoundTrip(pipeline.NewHTTPRequest(req))
				require.NoError(t, err)
				bad := drain(t, resps)
				if tc.status != 0 {
					require.NotNil(t, bad)
					return
				}
				require.Nil(t, bad)
				require.Equal(t, tc.want, rec.read())
			})
		}
	}
}

func TestTraceIDSharderBlockFormat(t *testing.T) {
	for _, tc := range paramFormatCases {
		t.Run(tc.name, func(t *testing.T) {
			metas := formatMetas(time.Now())
			var (
				mtx   sync.Mutex
				modes []string
				seen  []string
			)
			next := pipeline.AsyncRoundTripperFunc[combiner.PipelineResponse](func(r pipeline.Request) (pipeline.Responses[combiner.PipelineResponse], error) {
				q := r.HTTPRequest().URL.Query()
				mtx.Lock()
				defer mtx.Unlock()
				modes = append(modes, q.Get("mode"))
				if q.Has(pkgapi.BlocksKey) {
					raw, err := base64.RawURLEncoding.DecodeString(q.Get(pkgapi.BlocksKey))
					require.NoError(t, err)
					blocks := &tempopb.TraceByIDBlocks{}
					require.NoError(t, blocks.Unmarshal(raw))
					for _, b := range blocks.Blocks {
						seen = append(seen, b.Version)
					}
				}
				return pipeline.NewHTTPToAsyncResponse(&http.Response{Body: io.NopCloser(strings.NewReader("")), StatusCode: 404}), nil
			})
			sharder := newAsyncTraceIDSharder(&TraceByIDConfig{QueryShards: 3, DefaultBlockFormat: tc.def}, 0, &mockReader{metas: metas}, newJobsPerQueryHistogram(), log.NewNopLogger())

			path := "/api/traces/00000000000000000000000000000001"
			if tc.param != "" {
				path += "?blockFormat=" + tc.param
			}
			req := httptest.NewRequest("GET", path, nil)
			req = mux.SetURLVars(req, map[string]string{"traceID": "00000000000000000000000000000001"})
			req = req.WithContext(user.InjectOrgID(req.Context(), "test-tenant"))
			resps, err := sharder.Wrap(next).RoundTrip(pipeline.NewHTTPRequest(req))
			require.NoError(t, err)
			bad := drain(t, resps)
			if tc.status != 0 {
				require.NotNil(t, bad)
				return
			}
			require.Nil(t, bad)
			mtx.Lock()
			defer mtx.Unlock()
			require.Contains(t, modes, "ingesters", "the live-store job is still sent")
			slices.Sort(seen)
			require.Equal(t, tc.want, slices.Compact(seen))
		})
	}
}

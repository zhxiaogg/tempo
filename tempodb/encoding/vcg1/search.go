package vcg1

import (
	"context"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/grafana/tempo/v3/pkg/tempopb"
	"github.com/grafana/tempo/v3/pkg/traceql"
	"github.com/grafana/tempo/v3/tempodb/backend"
	"github.com/grafana/tempo/v3/tempodb/encoding/common"
	"github.com/grafana/tempo/v3/tempodb/encoding/vparquet5"
)

// Search answers a tag search (a request without a TraceQL query) as vparquet5 does, by running
// the equivalent TraceQL query through Fetch:
//   - service.name, name, root.name, root.service.name and string dedicated columns match by
//     substring; status.code by status; int dedicated columns by value when the tag is an int;
//   - every other tag matches a string attribute of a span or its resource exactly;
//   - min/max duration bound the trace duration, start/end the trace time range.
//
// Each tag is its own spanset, so tags may match different spans of the trace. vparquet5 asks
// the generic tags to match within one resource's spans, so a search of several generic tags
// can find more traces here; and it matches any value of a key a span or resource repeats,
// where TraceQL sees one, so it can find more there.
func (b *backendBlock) Search(ctx context.Context, req *tempopb.SearchRequest, opts common.SearchOptions) (*tempopb.SearchResponse, error) {
	q := tagSearchQuery(req, b.meta.DedicatedColumns)
	f := traceql.NewSpansetFetcherWrapper(func(ctx context.Context, fr traceql.FetchSpansRequest) (traceql.FetchSpansResponse, error) {
		return b.Fetch(ctx, fr, opts)
	})
	limit := req.Limit
	if limit == 0 {
		limit = uint32(max(b.meta.TotalObjects, 1)) // 0: every match, as vparquet5
	}
	resp, err := traceql.NewEngine().ExecuteSearch(ctx, &tempopb.SearchRequest{Query: q, Start: req.Start, End: req.End, Limit: limit, SpansPerSpanSet: 1}, f)
	if err != nil {
		return nil, err
	}
	out := &tempopb.SearchResponse{Metrics: &tempopb.SearchMetrics{InspectedTraces: uint32(b.meta.TotalObjects)}}
	if resp.Metrics != nil {
		out.Metrics.InspectedBytes = resp.Metrics.InspectedBytes
	}
	for _, t := range resp.Traces {
		out.Traces = append(out.Traces, &tempopb.TraceSearchMetadata{
			TraceID:           t.TraceID,
			RootServiceName:   t.RootServiceName,
			RootTraceName:     t.RootTraceName,
			StartTimeUnixNano: t.StartTimeUnixNano,
			DurationMs:        t.DurationMs,
		})
	}
	return out, nil
}

// tagSearchQuery is the TraceQL query of a tag search (see Search).
func tagSearchQuery(req *tempopb.SearchRequest, dc backend.DedicatedColumns) string {
	keys := make([]string, 0, len(req.Tags))
	for k := range req.Tags {
		keys = append(keys, k)
	}
	sort.Strings(keys) // a stable query
	var conds []string
	for _, k := range keys {
		conds = append(conds, tagCondition(k, req.Tags[k], dc))
	}
	if req.MinDurationMs > 0 {
		conds = append(conds, "traceDuration >= "+strconv.FormatUint(uint64(req.MinDurationMs), 10)+"ms")
	}
	if req.MaxDurationMs > 0 {
		conds = append(conds, "traceDuration <= "+strconv.FormatUint(uint64(req.MaxDurationMs), 10)+"ms")
	}
	if len(conds) == 0 {
		return "{}"
	}
	sets := make([]string, len(conds))
	for i, c := range conds {
		sets[i] = "{ " + c + " }"
	}
	return strings.Join(sets, " && ")
}

func tagCondition(k, v string, dc backend.DedicatedColumns) string {
	for _, c := range dc {
		if c.Name != k || (c.Scope != backend.DedicatedColumnScopeSpan && c.Scope != backend.DedicatedColumnScopeResource) {
			continue
		}
		attr := string(c.Scope) + "." + quoteTraceQL(k)
		switch c.Type {
		case backend.DedicatedColumnTypeString:
			return attr + " =~ " + substring(v)
		case backend.DedicatedColumnTypeInt:
			if i, err := strconv.ParseInt(v, 10, 64); err == nil {
				return attr + " = " + strconv.FormatInt(i, 10)
			}
		}
		break // an int column with a non-int tag: the generic attributes
	}
	switch k {
	case vparquet5.LabelServiceName:
		return "resource.service.name =~ " + substring(v)
	case vparquet5.LabelName:
		return "name =~ " + substring(v)
	case vparquet5.LabelRootServiceName:
		return "rootServiceName =~ " + substring(v)
	case vparquet5.LabelRootSpanName:
		return "rootName =~ " + substring(v)
	case vparquet5.LabelStatusCode:
		switch v {
		case vparquet5.StatusCodeOK, vparquet5.StatusCodeError:
			return "status = " + v
		}
		return "status = unset" // vparquet5 maps an unknown status to unset
	}
	return "." + quoteTraceQL(k) + " = " + quoteTraceQL(v)
}

// substring is the TraceQL regex matching strings that contain v.
func substring(v string) string { return quoteTraceQL(".*" + regexp.QuoteMeta(v) + ".*") }

// quoteTraceQL is s as a TraceQL double-quoted string (or quoted attribute name).
func quoteTraceQL(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

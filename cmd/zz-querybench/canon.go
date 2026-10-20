// Canonical forms of the three benchmark responses (GRPC_SPEC §5).
//
// The client always runs this Go code, whatever server it talks to, so the canonical bytes
// and their digest are produced by one implementation for every server. A canonical document
// is plain JSON produced by encoding/json from the structs below (field order fixed by the
// struct, map keys sorted), so two responses with the same content serialise to the same
// bytes whatever order the server emitted repeated fields in.
//
// Value encoding (every attribute / label value), one key per object:
//
//	{"string": "..."} {"int": 123} {"double": "1.5"} {"bool": true} {"bytes": "<hex>"}
//	{"array": [<value>...]} {"kvlist": [<attr>...]} {"strindex": 7} {"empty": true}
//
// Doubles are strings holding Go's shortest round-trip rendering (strconv 'g', -1), so no
// JSON float formatting question can arise; NaN/Inf render as "NaN"/"+Inf"/"-Inf".
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/grafana/tempo/v3/pkg/tempopb"
	commonv1 "github.com/grafana/tempo/v3/pkg/tempopb/common/v1"
	tracev1 "github.com/grafana/tempo/v3/pkg/tempopb/trace/v1"
)

type cAttr struct {
	Key   string         `json:"key"`
	Value map[string]any `json:"value"`
}

func canonValue(v *commonv1.AnyValue) map[string]any {
	if v == nil || v.Value == nil {
		return map[string]any{"empty": true}
	}
	switch x := v.Value.(type) {
	case *commonv1.AnyValue_StringValue:
		return map[string]any{"string": x.StringValue}
	case *commonv1.AnyValue_BoolValue:
		return map[string]any{"bool": x.BoolValue}
	case *commonv1.AnyValue_IntValue:
		return map[string]any{"int": x.IntValue}
	case *commonv1.AnyValue_DoubleValue:
		return map[string]any{"double": formatFloat(x.DoubleValue)}
	case *commonv1.AnyValue_BytesValue:
		return map[string]any{"bytes": hex.EncodeToString(x.BytesValue)}
	case *commonv1.AnyValue_ArrayValue:
		vals := []map[string]any{}
		if x.ArrayValue != nil {
			for _, e := range x.ArrayValue.Values {
				vals = append(vals, canonValue(e))
			}
		}
		// Array element order is data, not presentation: kept as emitted.
		return map[string]any{"array": vals}
	case *commonv1.AnyValue_KvlistValue:
		var kvs []*commonv1.KeyValue
		if x.KvlistValue != nil {
			kvs = x.KvlistValue.Values
		}
		return map[string]any{"kvlist": canonAttrs(kvs)}
	case *commonv1.AnyValue_StringValueStrindex:
		return map[string]any{"strindex": x.StringValueStrindex}
	default:
		return map[string]any{"unknown": v.String()}
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// canonAttrs sorts by key, then by the value's canonical JSON (so a key that repeats — Tempo
// drops the scope from search attribute keys, so `span.x` and `resource.x` can both appear as
// `x` — is order-independent too).
func canonAttrs(kvs []*commonv1.KeyValue) []cAttr {
	out := make([]cAttr, 0, len(kvs))
	for _, kv := range kvs {
		if kv == nil {
			continue
		}
		out = append(out, cAttr{Key: kv.Key, Value: canonValue(kv.Value)})
	}
	sortAttrs(out)
	return out
}

func canonAttrVals(kvs []commonv1.KeyValue) []cAttr {
	out := make([]cAttr, 0, len(kvs))
	for i := range kvs {
		out = append(out, cAttr{Key: kvs[i].Key, Value: canonValue(kvs[i].Value)})
	}
	sortAttrs(out)
	return out
}

func sortAttrs(a []cAttr) {
	sort.SliceStable(a, func(i, j int) bool {
		if a[i].Key != a[j].Key {
			return a[i].Key < a[j].Key
		}
		return string(mustJSON(a[i].Value)) < string(mustJSON(a[j].Value))
	})
}

// padHex left-pads a hex id so ids sort numerically. Tempo's TraceIDToHexString /
// SpanIDToHexString trim leading zeros; the canonical form keeps the server's string as-is
// and only uses the padded form as the sort key.
func padHex(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return strings.Repeat("0", width-len(s)) + s
}

// ---------------------------------------------------------------- SearchResponse

type cSearchSpan struct {
	SpanID            string  `json:"spanID"`
	Name              string  `json:"name"`
	StartTimeUnixNano uint64  `json:"startTimeUnixNano"`
	DurationNanos     uint64  `json:"durationNanos"`
	Attributes        []cAttr `json:"attributes"`
}

type cSpanSet struct {
	Matched    uint32        `json:"matched"`
	Spans      []cSearchSpan `json:"spans"`
	Attributes []cAttr       `json:"attributes"`
}

type cServiceStats struct {
	SpanCount  uint32 `json:"spanCount"`
	ErrorCount uint32 `json:"errorCount"`
}

type cTraceMeta struct {
	TraceID           string                   `json:"traceID"`
	RootServiceName   string                   `json:"rootServiceName"`
	RootTraceName     string                   `json:"rootTraceName"`
	StartTimeUnixNano uint64                   `json:"startTimeUnixNano"`
	DurationMs        uint32                   `json:"durationMs"`
	SpanSet           *cSpanSet                `json:"spanSet"` // deprecated single field; Tempo fills it (== spanSets[0] for one spanset)
	SpanSets          []cSpanSet               `json:"spanSets"`
	ServiceStats      map[string]cServiceStats `json:"serviceStats"`
}

type cSearch struct {
	Traces []cTraceMeta `json:"traces"`
}

func canonSpanSet(ss *tempopb.SpanSet) cSpanSet {
	c := cSpanSet{Matched: ss.Matched, Spans: make([]cSearchSpan, 0, len(ss.Spans)), Attributes: canonAttrs(ss.Attributes)}
	for _, sp := range ss.Spans {
		if sp == nil {
			continue
		}
		c.Spans = append(c.Spans, cSearchSpan{
			SpanID: sp.SpanID, Name: sp.Name, StartTimeUnixNano: sp.StartTimeUnixNano,
			DurationNanos: sp.DurationNanos, Attributes: canonAttrs(sp.Attributes),
		})
	}
	sort.SliceStable(c.Spans, func(i, j int) bool { return padHex(c.Spans[i].SpanID, 16) < padHex(c.Spans[j].SpanID, 16) })
	return c
}

func canonTraceMeta(tr *tempopb.TraceSearchMetadata) cTraceMeta {
	c := cTraceMeta{
		TraceID: tr.TraceID, RootServiceName: tr.RootServiceName, RootTraceName: tr.RootTraceName,
		StartTimeUnixNano: tr.StartTimeUnixNano, DurationMs: tr.DurationMs,
		SpanSets: make([]cSpanSet, 0, len(tr.SpanSets)), ServiceStats: map[string]cServiceStats{},
	}
	if tr.SpanSet != nil {
		s := canonSpanSet(tr.SpanSet)
		c.SpanSet = &s
	}
	for _, ss := range tr.SpanSets {
		if ss != nil {
			c.SpanSets = append(c.SpanSets, canonSpanSet(ss))
		}
	}
	sort.SliceStable(c.SpanSets, func(i, j int) bool {
		return string(mustJSON(c.SpanSets[i])) < string(mustJSON(c.SpanSets[j]))
	})
	for k, v := range tr.ServiceStats {
		if v != nil {
			c.ServiceStats[k] = cServiceStats{SpanCount: v.SpanCount, ErrorCount: v.ErrorCount}
		}
	}
	return c
}

// canonSearch: traces sorted by (zero-padded) trace id = block order.
func canonSearch(resp *tempopb.SearchResponse) *cSearch {
	c := &cSearch{Traces: make([]cTraceMeta, 0, len(resp.GetTraces()))}
	for _, tr := range resp.GetTraces() {
		if tr != nil {
			c.Traces = append(c.Traces, canonTraceMeta(tr))
		}
	}
	sort.SliceStable(c.Traces, func(i, j int) bool { return padHex(c.Traces[i].TraceID, 32) < padHex(c.Traces[j].TraceID, 32) })
	return c
}

// ---------------------------------------------------------------- QueryRangeResponse

type cSample struct {
	T int64  `json:"t"`
	V string `json:"v"`
}

type cExemplar struct {
	Labels []cAttr `json:"labels"`
	T      int64   `json:"t"`
	V      string  `json:"v"`
}

type cSeries struct {
	Labels    []cAttr     `json:"labels"`
	Samples   []cSample   `json:"samples"`
	Exemplars []cExemplar `json:"exemplars"`
}

type cQueryRange struct {
	Status string    `json:"status"`
	Series []cSeries `json:"series"`
}

func canonQueryRange(resp *tempopb.QueryRangeResponse) *cQueryRange {
	c := &cQueryRange{Status: resp.Status.String(), Series: make([]cSeries, 0, len(resp.GetSeries()))}
	for _, ts := range resp.GetSeries() {
		if ts == nil {
			continue
		}
		s := cSeries{Labels: canonAttrVals(ts.Labels), Samples: make([]cSample, 0, len(ts.Samples)), Exemplars: []cExemplar{}}
		for _, sm := range ts.Samples {
			s.Samples = append(s.Samples, cSample{T: sm.TimestampMs, V: formatFloat(sm.Value)})
		}
		sort.SliceStable(s.Samples, func(i, j int) bool { return s.Samples[i].T < s.Samples[j].T })
		for _, ex := range ts.Exemplars {
			s.Exemplars = append(s.Exemplars, cExemplar{Labels: canonAttrVals(ex.Labels), T: ex.TimestampMs, V: formatFloat(ex.Value)})
		}
		sort.SliceStable(s.Exemplars, func(i, j int) bool {
			return string(mustJSON(s.Exemplars[i])) < string(mustJSON(s.Exemplars[j]))
		})
		c.Series = append(c.Series, s)
	}
	sort.SliceStable(c.Series, func(i, j int) bool {
		return string(mustJSON(c.Series[i].Labels)) < string(mustJSON(c.Series[j].Labels))
	})
	return c
}

// ---------------------------------------------------------------- TraceByIDResponse

type cStatus struct {
	Code    int32  `json:"code"`
	Message string `json:"message"`
}

type cEvent struct {
	TimeUnixNano           uint64  `json:"timeUnixNano"`
	Name                   string  `json:"name"`
	Attributes             []cAttr `json:"attributes"`
	DroppedAttributesCount uint32  `json:"droppedAttributesCount"`
}

type cLink struct {
	TraceID                string  `json:"traceId"`
	SpanID                 string  `json:"spanId"`
	TraceState             string  `json:"traceState"`
	Attributes             []cAttr `json:"attributes"`
	DroppedAttributesCount uint32  `json:"droppedAttributesCount"`
	Flags                  uint32  `json:"flags"`
}

type cOSpan struct {
	TraceID                string   `json:"traceId"`
	SpanID                 string   `json:"spanId"`
	TraceState             string   `json:"traceState"`
	ParentSpanID           string   `json:"parentSpanId"`
	Flags                  uint32   `json:"flags"`
	Name                   string   `json:"name"`
	Kind                   int32    `json:"kind"`
	StartTimeUnixNano      uint64   `json:"startTimeUnixNano"`
	EndTimeUnixNano        uint64   `json:"endTimeUnixNano"`
	Attributes             []cAttr  `json:"attributes"`
	DroppedAttributesCount uint32   `json:"droppedAttributesCount"`
	Events                 []cEvent `json:"events"`
	DroppedEventsCount     uint32   `json:"droppedEventsCount"`
	Links                  []cLink  `json:"links"`
	DroppedLinksCount      uint32   `json:"droppedLinksCount"`
	Status                 *cStatus `json:"status"`
}

type cScope struct {
	Name                   string  `json:"name"`
	Version                string  `json:"version"`
	Attributes             []cAttr `json:"attributes"`
	DroppedAttributesCount uint32  `json:"droppedAttributesCount"`
}

type cScopeSpans struct {
	Scope     *cScope  `json:"scope"`
	SchemaURL string   `json:"schemaUrl"`
	Spans     []cOSpan `json:"spans"`
}

type cResource struct {
	Attributes             []cAttr         `json:"attributes"`
	DroppedAttributesCount uint32          `json:"droppedAttributesCount"`
	EntityRefs             json.RawMessage `json:"entityRefs,omitempty"`
}

type cResourceSpans struct {
	Resource   *cResource    `json:"resource"`
	SchemaURL  string        `json:"schemaUrl"`
	ScopeSpans []cScopeSpans `json:"scopeSpans"`
}

type cTraceByID struct {
	Found         bool             `json:"found"`
	ResourceSpans []cResourceSpans `json:"resourceSpans"`
}

func canonOSpan(sp *tracev1.Span) cOSpan {
	c := cOSpan{
		TraceID: hex.EncodeToString(sp.TraceId), SpanID: hex.EncodeToString(sp.SpanId),
		TraceState: sp.TraceState, ParentSpanID: hex.EncodeToString(sp.ParentSpanId), Flags: sp.Flags,
		Name: sp.Name, Kind: int32(sp.Kind), StartTimeUnixNano: sp.StartTimeUnixNano, EndTimeUnixNano: sp.EndTimeUnixNano,
		Attributes: canonAttrs(sp.Attributes), DroppedAttributesCount: sp.DroppedAttributesCount,
		Events: make([]cEvent, 0, len(sp.Events)), DroppedEventsCount: sp.DroppedEventsCount,
		Links: make([]cLink, 0, len(sp.Links)), DroppedLinksCount: sp.DroppedLinksCount,
	}
	if sp.Status != nil {
		c.Status = &cStatus{Code: int32(sp.Status.Code), Message: sp.Status.Message}
	}
	for _, ev := range sp.Events {
		if ev != nil {
			c.Events = append(c.Events, cEvent{TimeUnixNano: ev.TimeUnixNano, Name: ev.Name, Attributes: canonAttrs(ev.Attributes), DroppedAttributesCount: ev.DroppedAttributesCount})
		}
	}
	// Events and links are sorted too (time, then content): OTLP gives their order no meaning.
	sort.SliceStable(c.Events, func(i, j int) bool {
		if c.Events[i].TimeUnixNano != c.Events[j].TimeUnixNano {
			return c.Events[i].TimeUnixNano < c.Events[j].TimeUnixNano
		}
		return string(mustJSON(c.Events[i])) < string(mustJSON(c.Events[j]))
	})
	for _, l := range sp.Links {
		if l != nil {
			c.Links = append(c.Links, cLink{TraceID: hex.EncodeToString(l.TraceId), SpanID: hex.EncodeToString(l.SpanId), TraceState: l.TraceState,
				Attributes: canonAttrs(l.Attributes), DroppedAttributesCount: l.DroppedAttributesCount, Flags: l.Flags})
		}
	}
	sort.SliceStable(c.Links, func(i, j int) bool { return string(mustJSON(c.Links[i])) < string(mustJSON(c.Links[j])) })
	return c
}

func canonTraceByID(resp *tempopb.TraceByIDResponse) *cTraceByID {
	c := &cTraceByID{ResourceSpans: []cResourceSpans{}}
	if resp == nil || resp.Trace == nil {
		return c
	}
	c.Found = true
	for _, rs := range resp.Trace.ResourceSpans {
		if rs == nil {
			continue
		}
		crs := cResourceSpans{SchemaURL: rs.SchemaUrl, ScopeSpans: make([]cScopeSpans, 0, len(rs.ScopeSpans))}
		if rs.Resource != nil {
			crs.Resource = &cResource{Attributes: canonAttrs(rs.Resource.Attributes), DroppedAttributesCount: rs.Resource.DroppedAttributesCount}
			if len(rs.Resource.EntityRefs) > 0 {
				crs.Resource.EntityRefs = mustJSON(rs.Resource.EntityRefs)
			}
		}
		for _, ss := range rs.ScopeSpans {
			if ss == nil {
				continue
			}
			css := cScopeSpans{SchemaURL: ss.SchemaUrl, Spans: make([]cOSpan, 0, len(ss.Spans))}
			if ss.Scope != nil {
				css.Scope = &cScope{Name: ss.Scope.Name, Version: ss.Scope.Version, Attributes: canonAttrs(ss.Scope.Attributes), DroppedAttributesCount: ss.Scope.DroppedAttributesCount}
			}
			for _, sp := range ss.Spans {
				if sp != nil {
					css.Spans = append(css.Spans, canonOSpan(sp))
				}
			}
			sort.SliceStable(css.Spans, func(i, j int) bool { return css.Spans[i].SpanID < css.Spans[j].SpanID })
			crs.ScopeSpans = append(crs.ScopeSpans, css)
		}
		sort.SliceStable(crs.ScopeSpans, func(i, j int) bool {
			return string(mustJSON(crs.ScopeSpans[i])) < string(mustJSON(crs.ScopeSpans[j]))
		})
		c.ResourceSpans = append(c.ResourceSpans, crs)
	}
	sort.SliceStable(c.ResourceSpans, func(i, j int) bool {
		return string(mustJSON(c.ResourceSpans[i])) < string(mustJSON(c.ResourceSpans[j]))
	})
	return c
}

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

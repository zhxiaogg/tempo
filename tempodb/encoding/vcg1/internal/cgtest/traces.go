// Package cgtest generates test traces that exercise every attribute shape the CG format
// stores (scalars, arrays, empty arrays, kvlist/bytes values, dedicated columns with matching
// and wrong types, single-element dedicated arrays), events, links and span trees.
package cgtest

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"sort"

	"github.com/gogo/protobuf/proto"
	"github.com/grafana/tempo/v3/pkg/tempopb"
	v1 "github.com/grafana/tempo/v3/pkg/tempopb/common/v1"
	v1_resource "github.com/grafana/tempo/v3/pkg/tempopb/resource/v1"
	v1_trace "github.com/grafana/tempo/v3/pkg/tempopb/trace/v1"
	"github.com/grafana/tempo/v3/tempodb/backend"
	"github.com/grafana/tempo/v3/tempodb/encoding/common"
)

// Dedicated is the dedicated-column config the generated attributes are aimed at.
var Dedicated = backend.DedicatedColumns{
	{Scope: backend.DedicatedColumnScopeSpan, Name: "http.route", Type: backend.DedicatedColumnTypeString},
	{Scope: backend.DedicatedColumnScopeSpan, Name: "blockSize", Type: backend.DedicatedColumnTypeInt},
	{Scope: backend.DedicatedColumnScopeResource, Name: "cluster", Type: backend.DedicatedColumnTypeString},
	{Scope: backend.DedicatedColumnScopeResource, Name: "process.pid", Type: backend.DedicatedColumnTypeInt},
	{Scope: backend.DedicatedColumnScopeEvent, Name: "exception.type", Type: backend.DedicatedColumnTypeString},
}

// Trace is one generated trace.
type Trace struct {
	ID    []byte
	Trace *tempopb.Trace
}

func str(s string) *v1.AnyValue { return &v1.AnyValue{Value: &v1.AnyValue_StringValue{StringValue: s}} }
func i64(i int64) *v1.AnyValue  { return &v1.AnyValue{Value: &v1.AnyValue_IntValue{IntValue: i}} }
func f64(f float64) *v1.AnyValue {
	return &v1.AnyValue{Value: &v1.AnyValue_DoubleValue{DoubleValue: f}}
}
func bl(b bool) *v1.AnyValue { return &v1.AnyValue{Value: &v1.AnyValue_BoolValue{BoolValue: b}} }

func arr(vs ...*v1.AnyValue) *v1.AnyValue {
	return &v1.AnyValue{Value: &v1.AnyValue_ArrayValue{ArrayValue: &v1.ArrayValue{Values: vs}}}
}

func kv(k string, v *v1.AnyValue) *v1.KeyValue { return &v1.KeyValue{Key: k, Value: v} }

type gen struct{ r *rand.Rand }

func (g *gen) pick(xs ...string) string { return xs[g.r.Intn(len(xs))] }

// value is a random attribute value of any shape.
func (g *gen) value() *v1.AnyValue {
	switch g.r.Intn(16) {
	case 0:
		return i64(g.r.Int63n(1000) - 500)
	case 1:
		return f64(g.r.NormFloat64())
	case 2:
		return bl(g.r.Intn(2) == 0)
	case 3:
		return arr(str(g.pick("a", "b", "c")), str(g.pick("x", "y")))
	case 4:
		return arr(i64(1), i64(g.r.Int63n(9)))
	case 5:
		return arr(f64(1.5), f64(g.r.Float64()))
	case 6:
		return arr(bl(true), bl(false))
	case 7:
		return arr()
	case 8:
		return &v1.AnyValue{Value: &v1.AnyValue_KvlistValue{KvlistValue: &v1.KeyValueList{Values: []*v1.KeyValue{kv("k", str("v"))}}}}
	case 9:
		return &v1.AnyValue{Value: &v1.AnyValue_BytesValue{BytesValue: []byte{1, 2, byte(g.r.Intn(256))}}}
	case 10:
		return arr(str("solo"))
	case 11:
		return str(fmt.Sprintf("uniq-%016x", g.r.Uint64())) // high cardinality: dictionary fallback, raw runs
	}
	return str(g.pick("alpha", "beta", "gamma", "delta", ""))
}

func (g *gen) attrs(n int, keys ...string) []*v1.KeyValue {
	var out []*v1.KeyValue
	for i := 0; i < n; i++ {
		out = append(out, kv(g.pick(keys...), g.value()))
	}
	return out
}

// Traces returns n traces sorted by trace id.
func Traces(n int, seed int64) []Trace {
	g := &gen{r: rand.New(rand.NewSource(seed))}
	out := make([]Trace, 0, n)
	base := uint64(1_700_000_000_000_000_000)
	for i := 0; i < n; i++ {
		id := make([]byte, 16)
		g.r.Read(id)
		if i%17 == 0 {
			copy(id, make([]byte, 8)) // short ids (64-bit) padded with zeros
		}
		tr := &tempopb.Trace{}
		start := base + uint64(g.r.Int63n(int64(3600e9)))
		nres := 1 + g.r.Intn(3)
		for r := 0; r < nres; r++ {
			res := &v1_resource.Resource{DroppedAttributesCount: uint32(g.r.Intn(2))}
			if g.r.Intn(10) > 0 {
				res.Attributes = append(res.Attributes, kv("service.name", str(g.pick("api", "db", "web", "grafana"))))
			} else if g.r.Intn(2) == 0 {
				res.Attributes = append(res.Attributes, kv("service.name", i64(7))) // not a string: generic
			}
			switch g.r.Intn(5) {
			case 0:
				res.Attributes = append(res.Attributes, kv("cluster", str(g.pick("eu", "us"))))
			case 1:
				res.Attributes = append(res.Attributes, kv("cluster", i64(3))) // wrong type: generic
			case 2:
				res.Attributes = append(res.Attributes, kv("cluster", arr(str("eu"), str("us"))))
			case 3:
				res.Attributes = append(res.Attributes, kv("cluster", arr())) // dropped by vParquet5
			}
			if g.r.Intn(2) == 0 {
				res.Attributes = append(res.Attributes, kv("process.pid", i64(g.r.Int63n(100000))))
			}
			res.Attributes = append(res.Attributes, g.attrs(g.r.Intn(4), "plan", "zone", "host", "env")...)
			rs := &v1_trace.ResourceSpans{Resource: res}
			nsc := 1 + g.r.Intn(2)
			var spans []*v1_trace.Span
			for c := 0; c < nsc; c++ {
				ss := &v1_trace.ScopeSpans{Scope: &v1.InstrumentationScope{Name: g.pick("otel", "lib", ""), Version: g.pick("1.0", "")}}
				if g.r.Intn(3) == 0 {
					ss.Scope.Attributes = g.attrs(1+g.r.Intn(2), "scope.a", "scope.b")
				}
				nsp := 1 + g.r.Intn(8)
				for s := 0; s < nsp; s++ {
					sp := &v1_trace.Span{
						TraceId:                id,
						SpanId:                 u64b(g.r.Uint64()),
						Name:                   g.pick("GET /", "POST /x", "query", "HTTP GET"),
						Kind:                   v1_trace.Span_SpanKind(g.r.Intn(6)),
						StartTimeUnixNano:      start + uint64(g.r.Int63n(1e9)),
						Status:                 &v1_trace.Status{Code: v1_trace.Status_StatusCode(g.r.Intn(3)), Message: g.pick("", "", "boom")},
						TraceState:             g.pick("", "", "ot=th:8"),
						DroppedAttributesCount: uint32(g.r.Intn(2)),
						DroppedEventsCount:     uint32(g.r.Intn(2)),
						DroppedLinksCount:      uint32(g.r.Intn(2)),
					}
					sp.EndTimeUnixNano = sp.StartTimeUnixNano + uint64(g.r.Int63n(5e8))
					if g.r.Intn(50) == 0 {
						sp.EndTimeUnixNano = sp.StartTimeUnixNano - 1 // negative duration wraps
					}
					switch {
					case len(spans) > 0 && g.r.Intn(6) > 0:
						sp.ParentSpanId = spans[g.r.Intn(len(spans))].SpanId
					case g.r.Intn(4) == 0:
						sp.ParentSpanId = u64b(g.r.Uint64()) // parent outside the trace
					}
					switch g.r.Intn(6) {
					case 0:
						sp.Attributes = append(sp.Attributes, kv("http.route", str(g.pick("/api/v1/a", "/api/v2/b"))))
					case 1:
						sp.Attributes = append(sp.Attributes, kv("http.route", i64(5)))
					case 2:
						sp.Attributes = append(sp.Attributes, kv("http.route", arr(str("/a"))))
					case 3:
						sp.Attributes = append(sp.Attributes, kv("blockSize", arr(i64(1), i64(2), i64(3))))
					case 4:
						sp.Attributes = append(sp.Attributes, kv("blockSize", i64(g.r.Int63n(1<<20))))
					}
					sp.Attributes = append(sp.Attributes, g.attrs(g.r.Intn(6), "node", "user", "txn", "http.method", "http.status_code", "id")...)
					for e := g.r.Intn(4); e > 0; e-- {
						ev := &v1_trace.Span_Event{TimeUnixNano: sp.StartTimeUnixNano + uint64(g.r.Int63n(1e6)), Name: g.pick("exception", "log", ""), DroppedAttributesCount: uint32(g.r.Intn(2))}
						if g.r.Intn(2) == 0 {
							ev.Attributes = append(ev.Attributes, kv("exception.type", str(g.pick("IOError", "ValueError", "x"))))
						}
						ev.Attributes = append(ev.Attributes, g.attrs(g.r.Intn(3), "zone", "msg")...)
						sp.Events = append(sp.Events, ev)
					}
					for l := g.r.Intn(3); l > 0; l-- {
						ln := &v1_trace.Span_Link{TraceId: id, SpanId: u64b(g.r.Uint64()), TraceState: g.pick("", "a=b"), DroppedAttributesCount: uint32(g.r.Intn(2))}
						if g.r.Intn(3) == 0 {
							ln.Attributes = append(ln.Attributes, kv("opentracing.ref_type", str(g.pick("child_of", "follows_from"))))
						}
						ln.Attributes = append(ln.Attributes, g.attrs(g.r.Intn(2), "link.a", "link.b")...)
						sp.Links = append(sp.Links, ln)
					}
					spans = append(spans, sp)
					ss.Spans = append(ss.Spans, sp)
				}
				rs.ScopeSpans = append(rs.ScopeSpans, ss)
			}
			tr.ResourceSpans = append(tr.ResourceSpans, rs)
		}
		out = append(out, Trace{ID: id, Trace: tr})
	}
	sort.Slice(out, func(a, b int) bool { return bytes.Compare(out[a].ID, out[b].ID) < 0 })
	return out
}

func u64b(v uint64) []byte {
	b := make([]byte, 8)
	for i := range b {
		b[i] = byte(v >> (8 * i))
	}
	return b
}

// Iter is a common.Iterator over traces.
type Iter struct {
	Traces []Trace
	i      int
}

func (it *Iter) Next(context.Context) (common.ID, *tempopb.Trace, error) {
	if it.i >= len(it.Traces) {
		return nil, nil, nil
	}
	t := it.Traces[it.i]
	it.i++
	return t.ID, t.Trace, nil
}

func (it *Iter) Close() {}

// Canon returns tr marshalled after sorting every attribute list (vParquet5 does not keep
// attribute order across generic and dedicated columns), for comparisons.
func Canon(tr *tempopb.Trace) []byte {
	if tr == nil {
		return nil
	}
	c := proto.Clone(tr).(*tempopb.Trace)
	srt := func(a []*v1.KeyValue) {
		sort.SliceStable(a, func(i, j int) bool {
			if a[i].Key != a[j].Key {
				return a[i].Key < a[j].Key
			}
			return a[i].Value.String() < a[j].Value.String()
		})
	}
	for _, rs := range c.ResourceSpans {
		if rs.Resource != nil {
			srt(rs.Resource.Attributes)
		}
		for _, ss := range rs.ScopeSpans {
			if ss.Scope != nil {
				srt(ss.Scope.Attributes)
			}
			for _, sp := range ss.Spans {
				srt(sp.Attributes)
				for _, e := range sp.Events {
					srt(e.Attributes)
				}
				for _, l := range sp.Links {
					srt(l.Attributes)
				}
			}
		}
	}
	b, err := c.Marshal()
	if err != nil {
		panic(err)
	}
	return b
}

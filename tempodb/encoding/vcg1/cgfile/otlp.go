package cgfile

import (
	"bytes"
	"fmt"

	"github.com/golang/protobuf/jsonpb" //nolint:all // same decoding as vparquet5
	"github.com/grafana/tempo/v3/pkg/tempopb"
	v1 "github.com/grafana/tempo/v3/pkg/tempopb/common/v1"
	v1_resource "github.com/grafana/tempo/v3/pkg/tempopb/resource/v1"
	v1_trace "github.com/grafana/tempo/v3/pkg/tempopb/trace/v1"
)

const labelServiceName = "service.name"

// rowGetter reads loaded rows with a sticky error.
type rowGetter struct {
	q   *Request
	err error
}

func (g *rowGetter) col(t, c int) *Column {
	col, err := g.q.Column(t, c)
	if err != nil && g.err == nil {
		g.err = err
	}
	return col
}

func (g *rowGetter) at(t, c int, r uint32) (*Values, int, *Column) {
	col := g.col(t, c)
	if col == nil {
		return nil, 0, nil
	}
	v, i, err := col.At(r)
	if err != nil {
		if g.err == nil {
			g.err = err
		}
		return nil, 0, nil
	}
	return v, i, col
}

func (g *rowGetter) int(t, c int, r uint32) int64 {
	v, i, _ := g.at(t, c, r)
	if v == nil || v.IsNull(i) {
		return 0
	}
	return v.Ints[i]
}

// bytes returns the value of a byte-array column (nil, false when null).
func (g *rowGetter) bytes(t, c int, r uint32) ([]byte, bool) {
	v, i, col := g.at(t, c, r)
	if v == nil || v.IsNull(i) {
		return nil, false
	}
	return v.Bytes(i, &col.dict), true
}

func (g *rowGetter) str(t, c int, r uint32) string {
	b, _ := g.bytes(t, c, r)
	return string(b)
}

// cloneBytes copies an id out of the request's buffers.
func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	return append(make([]byte, 0, len(b)), b...)
}

// assemble maps the trace's rows to OTLP the way vparquet5 returns it.
func (q *Request) assemble(rg *traceRows) (*tempopb.Trace, error) {
	g := &rowGetter{q: q}
	t := rg[TTraces].Lo
	traceID := cloneBytes(g.bytesOr(TTraces, TrTraceID, t))

	var attrs [NumLevels][][]*v1.KeyValue
	for l := 0; l < NumLevels; l++ {
		attrs[l] = q.levelAttrs(g, l, rg)
	}
	if g.err != nil {
		return nil, g.err
	}

	res, sc, sp := rg[TResources], rg[TScopes], rg[TSpans]
	// derived parent span ids: first span of the trace (trace order) per nested_left
	byLeft := make(map[int64][]byte, sp.Hi-sp.Lo)
	for s := sp.Lo; s < sp.Hi; s++ {
		left := g.int(TSpans, SpNestedLeft, s)
		if _, ok := byLeft[left]; !ok {
			byLeft[left], _ = g.bytes(TSpans, SpSpanID, s)
		}
	}

	tr := &tempopb.Trace{ResourceSpans: make([]*v1_trace.ResourceSpans, 0, res.Hi-res.Lo)}
	for r := res.Lo; r < res.Hi; r++ {
		ra := attrs[LResource][r-res.Lo]
		if name := g.str(TResources, ResServiceName, r); name != "" {
			ra = append(ra, &v1.KeyValue{Key: labelServiceName, Value: &v1.AnyValue{Value: &v1.AnyValue_StringValue{StringValue: name}}})
		}
		rs := &v1_trace.ResourceSpans{Resource: &v1_resource.Resource{
			Attributes:             ra,
			DroppedAttributesCount: uint32(g.int(TResources, ResDropped, r)),
		}}
		c0, c1 := q.children(g, TResources, ResFirstScope, r, res, sc)
		rs.ScopeSpans = make([]*v1_trace.ScopeSpans, 0, c1-c0)
		for c := c0; c < c1; c++ {
			ss := &v1_trace.ScopeSpans{Scope: &v1.InstrumentationScope{
				Name:                   g.str(TScopes, ScName, c),
				Version:                g.str(TScopes, ScVersion, c),
				DroppedAttributesCount: uint32(g.int(TScopes, ScDropped, c)),
				Attributes:             attrs[LScope][c-sc.Lo],
			}}
			s0, s1 := q.children(g, TScopes, ScFirstSpan, c, sc, sp)
			ss.Spans = make([]*v1_trace.Span, 0, s1-s0)
			for s := s0; s < s1; s++ {
				ss.Spans = append(ss.Spans, q.span(g, s, rg, traceID, byLeft, &attrs))
			}
			rs.ScopeSpans = append(rs.ScopeSpans, ss)
		}
		tr.ResourceSpans = append(tr.ResourceSpans, rs)
	}
	if g.err != nil {
		return nil, g.err
	}
	return tr, nil
}

func (g *rowGetter) bytesOr(t, c int, r uint32) []byte {
	b, _ := g.bytes(t, c, r)
	return b
}

// children is the child rows of parent row p (in parents), clipped to the trace's child rows.
func (q *Request) children(g *rowGetter, pt, c int, p uint32, parents, kids Interval) (uint32, uint32) {
	a := uint32(g.int(pt, c, p))
	b := kids.Hi
	if p+1 < parents.Hi {
		b = uint32(g.int(pt, c, p+1))
	}
	return max(a, kids.Lo), min(b, kids.Hi)
}

func (q *Request) span(g *rowGetter, s uint32, rg *traceRows, traceID []byte, byLeft map[int64][]byte, attrs *[NumLevels][][]*v1.KeyValue) *v1_trace.Span {
	sp := rg[TSpans]
	start := uint64(g.int(TSpans, SpStart, s))
	parent, ok := g.bytes(TSpans, SpParentSpanID, s)
	if !ok {
		parent = nil
		if pid := g.int(TSpans, SpParentID, s); pid > 0 {
			parent = byLeft[pid]
		}
	}
	out := &v1_trace.Span{
		TraceId:           traceID,
		SpanId:            cloneBytes(g.bytesOr(TSpans, SpSpanID, s)),
		TraceState:        g.str(TSpans, SpTraceState, s),
		Name:              g.str(TSpans, SpName, s),
		Kind:              v1_trace.Span_SpanKind(g.int(TSpans, SpKind, s)),
		ParentSpanId:      cloneBytes(parent),
		StartTimeUnixNano: start,
		EndTimeUnixNano:   start + uint64(g.int(TSpans, SpDuration, s)),
		Status: &v1_trace.Status{
			Message: g.str(TSpans, SpStatusMessage, s),
			Code:    v1_trace.Status_StatusCode(g.int(TSpans, SpStatusCode, s)),
		},
		Attributes:             attrs[LSpan][s-sp.Lo],
		DroppedAttributesCount: uint32(g.int(TSpans, SpDroppedAttrs, s)),
		DroppedEventsCount:     uint32(g.int(TSpans, SpDroppedEvents, s)),
		DroppedLinksCount:      uint32(g.int(TSpans, SpDroppedLinks, s)),
	}
	ev, ln := rg[TEvents], rg[TLinks]
	if e0, e1 := q.children(g, TSpans, SpFirstEvent, s, sp, ev); e1 > e0 {
		out.Events = make([]*v1_trace.Span_Event, 0, e1-e0)
		for e := e0; e < e1; e++ {
			out.Events = append(out.Events, &v1_trace.Span_Event{
				TimeUnixNano:           start + uint64(g.int(TEvents, EvTime, e)),
				Name:                   g.str(TEvents, EvName, e),
				Attributes:             attrs[LEvent][e-ev.Lo],
				DroppedAttributesCount: uint32(g.int(TEvents, EvDropped, e)),
			})
		}
	}
	if k0, k1 := q.children(g, TSpans, SpFirstLink, s, sp, ln); k1 > k0 {
		out.Links = make([]*v1_trace.Span_Link, 0, k1-k0)
		for k := k0; k < k1; k++ {
			out.Links = append(out.Links, &v1_trace.Span_Link{
				TraceId:                cloneBytes(g.bytesOr(TLinks, LnTraceID, k)),
				SpanId:                 cloneBytes(g.bytesOr(TLinks, LnSpanID, k)),
				TraceState:             g.str(TLinks, LnTraceState, k),
				Attributes:             attrs[LLink][k-ln.Lo],
				DroppedAttributesCount: uint32(g.int(TLinks, LnDropped, k)),
			})
		}
	}
	return out
}

// levelAttrs returns the attributes of every owner row of level l in the trace, indexed by
// owner row - first owner row.
func (q *Request) levelAttrs(g *rowGetter, l int, rg *traceRows) [][]*v1.KeyValue {
	own := rg[OwnerTable[l]]
	out := make([][]*v1.KeyValue, own.Hi-own.Lo)
	ta, te := AttrsTable(l), ElemsTable(l)
	ar, er := rg[ta], rg[te]
	if ar.Lo >= ar.Hi {
		return out
	}
	// element rows of each attribute row: [elStart[i], elStart[i+1])
	elStart := make([]uint32, ar.Hi-ar.Lo+1)
	for e := er.Lo; e < er.Hi; e++ {
		a := uint32(g.int(te, ElAttrRow, e))
		if a >= ar.Lo && a < ar.Hi {
			elStart[a-ar.Lo+1]++
		}
	}
	for i := 1; i < len(elStart); i++ {
		elStart[i] += elStart[i-1]
	}
	for a := ar.Lo; a < ar.Hi; a++ {
		o := uint32(g.int(ta, AtOwner, a))
		if o < own.Lo || o >= own.Hi {
			continue
		}
		var val v1.AnyValue
		switch vt := g.int(ta, AtVType, a); vt {
		case VTStr:
			b, ok := g.bytes(ta, AtStr, a)
			if !ok {
				b, _ = g.bytes(ta, AtStrRaw, a)
			}
			val.Value = &v1.AnyValue_StringValue{StringValue: string(b)}
		case VTInt:
			val.Value = &v1.AnyValue_IntValue{IntValue: g.int(ta, AtInt, a)}
		case VTDouble:
			val.Value = &v1.AnyValue_DoubleValue{DoubleValue: g.float(ta, AtDbl, a)}
		case VTBool:
			val.Value = &v1.AnyValue_BoolValue{BoolValue: g.bool(ta, AtBool, a)}
		case VTStrArr, VTIntArr, VTDoubleArr, VTBoolArr, VTEmptyArr:
			lo, hi := elStart[a-ar.Lo], elStart[a-ar.Lo+1]
			vals := make([]*v1.AnyValue, 0, hi-lo)
			for e := er.Lo + lo; e < er.Lo+hi; e++ {
				vals = append(vals, g.elem(te, e))
			}
			val.Value = &v1.AnyValue_ArrayValue{ArrayValue: &v1.ArrayValue{Values: vals}}
		case VTUnsupported:
			s, _ := g.bytes(ta, AtUnsupported, a)
			_ = jsonpb.Unmarshal(bytes.NewBuffer(s), &val)
		case VTEmpty:
			continue
		default:
			if g.err == nil {
				g.err = fmt.Errorf("cgfile: %s row %d: unknown vtype %d", TableName(ta), a, vt)
			}
			continue
		}
		key, _ := g.bytes(ta, AtKey, a)
		out[o-own.Lo] = append(out[o-own.Lo], &v1.KeyValue{Key: string(key), Value: &val})
	}
	return out
}

// elem is the first non-null value of an element row.
func (g *rowGetter) elem(te int, e uint32) *v1.AnyValue {
	if b, ok := g.bytes(te, ElStr, e); ok {
		return &v1.AnyValue{Value: &v1.AnyValue_StringValue{StringValue: string(b)}}
	}
	if b, ok := g.bytes(te, ElStrRaw, e); ok {
		return &v1.AnyValue{Value: &v1.AnyValue_StringValue{StringValue: string(b)}}
	}
	if v, i, _ := g.at(te, ElInt, e); v != nil && !v.IsNull(i) {
		return &v1.AnyValue{Value: &v1.AnyValue_IntValue{IntValue: v.Ints[i]}}
	}
	if v, i, _ := g.at(te, ElDbl, e); v != nil && !v.IsNull(i) {
		return &v1.AnyValue{Value: &v1.AnyValue_DoubleValue{DoubleValue: v.Floats[i]}}
	}
	if v, i, _ := g.at(te, ElBool, e); v != nil && !v.IsNull(i) {
		return &v1.AnyValue{Value: &v1.AnyValue_BoolValue{BoolValue: v.Bools[i]}}
	}
	return &v1.AnyValue{}
}

func (g *rowGetter) float(t, c int, r uint32) float64 {
	v, i, _ := g.at(t, c, r)
	if v == nil || v.IsNull(i) {
		return 0
	}
	return v.Floats[i]
}

func (g *rowGetter) bool(t, c int, r uint32) bool {
	v, i, _ := g.at(t, c, r)
	if v == nil || v.IsNull(i) {
		return false
	}
	return v.Bools[i]
}

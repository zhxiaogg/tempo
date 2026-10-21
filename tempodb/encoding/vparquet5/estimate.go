package vparquet5

import (
	tempoUtil "github.com/grafana/tempo/v3/pkg/util"
	"github.com/grafana/tempo/v3/tempodb/backend"
)

// SizeEstimator computes, from a *Trace, what estimateProtoSizeFromParquetRow returns for the
// trace's parquet row under a block's dedicated-column config (the compactor's MaxBytesPerTrace
// and TraceWritten estimate), without deconstructing the row. Encodings that compact vParquet5
// rows without parquet use it to make exactly the decisions the vParquet5 compactor makes.
//
// The row has one value per leaf column instance; a leaf under an empty list (or a nil
// optional) is one null value. Each value costs 1 (field id) plus its length for byte arrays,
// or 1 for every other kind, nulls included.
type SizeEstimator struct {
	res, span, event []dedicatedColumn // the scope's dedicated columns present in the schema
}

// NewSizeEstimator returns an estimator for blocks with the dedicated-column config dc.
func NewSizeEstimator(dc backend.DedicatedColumns) *SizeEstimator {
	cols := func(scope backend.DedicatedColumnScope) []dedicatedColumn {
		m := dedicatedColumnsToColumnMapping(dc, scope)
		var out []dedicatedColumn
		for _, c := range m.items() {
			out = append(out, c)
		}
		return out
	}
	return &SizeEstimator{
		res:   cols(backend.DedicatedColumnScopeResource),
		span:  cols(backend.DedicatedColumnScopeSpan),
		event: cols(backend.DedicatedColumnScopeEvent),
	}
}

const (
	nullCost      = 2
	attrLeaves    = 7  // Key, IsArray, Value, ValueInt, ValueDouble, ValueBool, ValueUnsupported
	linkLeaves    = 11 // TraceID, SpanID, TraceState, Attrs, DroppedAttributesCount
	statsLeaves   = 3
	scopeLeaves   = 3 + attrLeaves // Name, Version, Attrs, DroppedAttributesCount
	spanOwnLeaves = 13 + attrLeaves + 1 + 1 + 1 + 4
)

func (e *SizeEstimator) eventLeaves() int { return 3 + attrLeaves + len(e.event) }
func (e *SizeEstimator) spanLeaves() int {
	return spanOwnLeaves + e.eventLeaves() + linkLeaves + len(e.span)
}
func (e *SizeEstimator) scopeSpansLeaves() int { return scopeLeaves + e.spanLeaves() + 1 }
func (e *SizeEstimator) rsLeaves() int {
	return 1 + attrLeaves + 1 + len(e.res) + e.scopeSpansLeaves()
}

func bytesCost(n int) int { return 1 + n }

func strsCost(v []string) int {
	if len(v) == 0 {
		return nullCost
	}
	n := 0
	for _, s := range v {
		n += bytesCost(len(s))
	}
	return n
}

func numsCost(n int) int {
	if n == 0 {
		return nullCost
	}
	return 2 * n
}

func attrsCost(attrs []Attribute) int {
	if len(attrs) == 0 {
		return attrLeaves * nullCost
	}
	n := 0
	for i := range attrs {
		a := &attrs[i]
		n += bytesCost(len(a.Key)) + 2
		n += strsCost(a.Value) + numsCost(len(a.ValueInt)) + numsCost(len(a.ValueDouble)) + numsCost(len(a.ValueBool))
		if a.ValueUnsupported == nil {
			n += nullCost
		} else {
			n += bytesCost(len(*a.ValueUnsupported))
		}
	}
	return n
}

func dedicatedCost(cols []dedicatedColumn, d *DedicatedAttributes) int {
	n := 0
	for i := range cols {
		c := &cols[i]
		if c.Type == backend.DedicatedColumnTypeInt {
			n += numsCost(len(dedicatedInts(d, c.ColumnIndex)))
		} else {
			n += strsCost(dedicatedStrings(d, c.ColumnIndex))
		}
	}
	return n
}

// ProtoSize is estimateProtoSizeFromParquetRow of tr's row.
func (e *SizeEstimator) ProtoSize(tr *Trace) int {
	n := bytesCost(len(tr.TraceID)) + bytesCost(len(tr.TraceIDText)) + 3*2 +
		bytesCost(len(tr.RootServiceName)) + bytesCost(len(tr.RootSpanName))
	if len(tr.ServiceStats) == 0 {
		n += statsLeaves * nullCost
	}
	for i := range tr.ServiceStats {
		n += bytesCost(len(tr.ServiceStats[i].ServiceName)) + 2 + 2
	}
	if len(tr.ResourceSpans) == 0 {
		return n + e.rsLeaves()*nullCost
	}
	for i := range tr.ResourceSpans {
		rs := &tr.ResourceSpans[i]
		n += bytesCost(len(rs.Resource.ServiceName)) + attrsCost(rs.Resource.Attrs) + 2 + dedicatedCost(e.res, &rs.Resource.DedicatedAttributes)
		if len(rs.ScopeSpans) == 0 {
			n += e.scopeSpansLeaves() * nullCost
			continue
		}
		for j := range rs.ScopeSpans {
			ss := &rs.ScopeSpans[j]
			n += bytesCost(len(ss.Scope.Name)) + bytesCost(len(ss.Scope.Version)) + attrsCost(ss.Scope.Attrs) + 2 + 2
			if len(ss.Spans) == 0 {
				n += e.spanLeaves() * nullCost
				continue
			}
			for k := range ss.Spans {
				n += e.spanCost(&ss.Spans[k])
			}
		}
	}
	return n
}

func (e *SizeEstimator) spanCost(s *Span) int {
	// SpanID, ParentSpanID, Name, TraceState, StatusMessage; 8 numbers; 3 dropped counts; 4
	// rounded start times
	n := bytesCost(len(s.SpanID)) + bytesCost(len(s.ParentSpanID)) + bytesCost(len(s.Name)) +
		bytesCost(len(s.TraceState)) + bytesCost(len(s.StatusMessage)) + 8*2 + 3*2 + 4*2
	n += attrsCost(s.Attrs) + dedicatedCost(e.span, &s.DedicatedAttributes)
	if len(s.Events) == 0 {
		n += e.eventLeaves() * nullCost
	}
	for i := range s.Events {
		ev := &s.Events[i]
		n += 2 + bytesCost(len(ev.Name)) + attrsCost(ev.Attrs) + 2 + dedicatedCost(e.event, &ev.DedicatedAttributes)
	}
	if len(s.Links) == 0 {
		n += linkLeaves * nullCost
	}
	for i := range s.Links {
		l := &s.Links[i]
		n += bytesCost(len(l.TraceID)) + bytesCost(len(l.SpanID)) + bytesCost(len(l.TraceState)) + attrsCost(l.Attrs) + 2
	}
	return n
}

// CountSpans is countSpans of tr's row: the trace id as hex, the root names and the number of
// SpanID values (a scope or resource without spans, or a trace without resources, counts one).
func CountSpans(tr *Trace) (traceID, rootSpanName, rootServiceName string, spans int) {
	if len(tr.ResourceSpans) == 0 {
		spans = 1
	}
	for i := range tr.ResourceSpans {
		rs := &tr.ResourceSpans[i]
		if len(rs.ScopeSpans) == 0 {
			spans++
		}
		for j := range rs.ScopeSpans {
			spans += max(1, len(rs.ScopeSpans[j].Spans))
		}
	}
	return tempoUtil.TraceIDToHexString(tr.TraceID), tr.RootSpanName, tr.RootServiceName, spans
}

func dedicatedStrings(d *DedicatedAttributes, i int) []string {
	switch i {
	case 0:
		return d.String01
	case 1:
		return d.String02
	case 2:
		return d.String03
	case 3:
		return d.String04
	case 4:
		return d.String05
	case 5:
		return d.String06
	case 6:
		return d.String07
	case 7:
		return d.String08
	case 8:
		return d.String09
	case 9:
		return d.String10
	case 10:
		return d.String11
	case 11:
		return d.String12
	case 12:
		return d.String13
	case 13:
		return d.String14
	case 14:
		return d.String15
	case 15:
		return d.String16
	case 16:
		return d.String17
	case 17:
		return d.String18
	case 18:
		return d.String19
	case 19:
		return d.String20
	}
	return nil
}

func dedicatedInts(d *DedicatedAttributes, i int) []int64 {
	switch i {
	case 0:
		return d.Int01
	case 1:
		return d.Int02
	case 2:
		return d.Int03
	case 3:
		return d.Int04
	case 4:
		return d.Int05
	}
	return nil
}

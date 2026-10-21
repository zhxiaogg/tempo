package vcg1

import (
	"bytes"
	"math"
	"slices"

	"github.com/grafana/tempo/v3/tempodb/encoding/vparquet5"
)

// rowsEqual reports whether a and b are equal as vParquet5 parquet rows, as the vparquet5
// compactor compares the rows it reads: every stored field, nil and empty lists alike, doubles
// by their bits. It allocates nothing and stops at the first difference.
func rowsEqual(a, b *vparquet5.Trace) bool {
	if !bytes.Equal(a.TraceID, b.TraceID) || a.TraceIDText != b.TraceIDText ||
		a.StartTimeUnixNano != b.StartTimeUnixNano || a.EndTimeUnixNano != b.EndTimeUnixNano ||
		a.DurationNano != b.DurationNano || a.RootServiceName != b.RootServiceName ||
		a.RootSpanName != b.RootSpanName || !slices.Equal(a.ServiceStats, b.ServiceStats) ||
		len(a.ResourceSpans) != len(b.ResourceSpans) {
		return false
	}
	for i := range a.ResourceSpans {
		ra, rb := &a.ResourceSpans[i], &b.ResourceSpans[i]
		if ra.Resource.ServiceName != rb.Resource.ServiceName || ra.Resource.DroppedAttributesCount != rb.Resource.DroppedAttributesCount ||
			!attrsEqual(ra.Resource.Attrs, rb.Resource.Attrs) || !dedEqual(&ra.Resource.DedicatedAttributes, &rb.Resource.DedicatedAttributes) ||
			len(ra.ScopeSpans) != len(rb.ScopeSpans) {
			return false
		}
		for j := range ra.ScopeSpans {
			sa, sb := &ra.ScopeSpans[j], &rb.ScopeSpans[j]
			if sa.SpanCount != sb.SpanCount || sa.Scope.Name != sb.Scope.Name || sa.Scope.Version != sb.Scope.Version ||
				sa.Scope.DroppedAttributesCount != sb.Scope.DroppedAttributesCount || !attrsEqual(sa.Scope.Attrs, sb.Scope.Attrs) ||
				len(sa.Spans) != len(sb.Spans) {
				return false
			}
			for k := range sa.Spans {
				if !spanEqual(&sa.Spans[k], &sb.Spans[k]) {
					return false
				}
			}
		}
	}
	return true
}

func spanEqual(a, b *vparquet5.Span) bool {
	if !bytes.Equal(a.SpanID, b.SpanID) || !bytes.Equal(a.ParentSpanID, b.ParentSpanID) ||
		a.ParentID != b.ParentID || a.NestedSetLeft != b.NestedSetLeft || a.NestedSetRight != b.NestedSetRight ||
		a.ChildCount != b.ChildCount || a.Name != b.Name || a.Kind != b.Kind || a.TraceState != b.TraceState ||
		a.StartTimeUnixNano != b.StartTimeUnixNano || a.DurationNano != b.DurationNano ||
		a.StatusCode != b.StatusCode || a.StatusMessage != b.StatusMessage ||
		a.DroppedAttributesCount != b.DroppedAttributesCount || a.DroppedEventsCount != b.DroppedEventsCount ||
		a.DroppedLinksCount != b.DroppedLinksCount || a.StartTimeRounded15 != b.StartTimeRounded15 ||
		a.StartTimeRounded60 != b.StartTimeRounded60 || a.StartTimeRounded300 != b.StartTimeRounded300 ||
		a.StartTimeRounded3600 != b.StartTimeRounded3600 || !attrsEqual(a.Attrs, b.Attrs) ||
		!dedEqual(&a.DedicatedAttributes, &b.DedicatedAttributes) ||
		len(a.Events) != len(b.Events) || len(a.Links) != len(b.Links) {
		return false
	}
	for i := range a.Events {
		ea, eb := &a.Events[i], &b.Events[i]
		if ea.TimeSinceStartNano != eb.TimeSinceStartNano || ea.Name != eb.Name ||
			ea.DroppedAttributesCount != eb.DroppedAttributesCount || !attrsEqual(ea.Attrs, eb.Attrs) ||
			!dedEqual(&ea.DedicatedAttributes, &eb.DedicatedAttributes) {
			return false
		}
	}
	for i := range a.Links {
		la, lb := &a.Links[i], &b.Links[i]
		if !bytes.Equal(la.TraceID, lb.TraceID) || !bytes.Equal(la.SpanID, lb.SpanID) || la.TraceState != lb.TraceState ||
			la.DroppedAttributesCount != lb.DroppedAttributesCount || !attrsEqual(la.Attrs, lb.Attrs) {
			return false
		}
	}
	return true
}

func attrsEqual(a, b []vparquet5.Attribute) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := &a[i], &b[i]
		if x.Key != y.Key || x.IsArray != y.IsArray || !slices.Equal(x.Value, y.Value) ||
			!slices.Equal(x.ValueInt, y.ValueInt) || !slices.Equal(x.ValueBool, y.ValueBool) ||
			!floatsEqual(x.ValueDouble, y.ValueDouble) || (x.ValueUnsupported == nil) != (y.ValueUnsupported == nil) ||
			(x.ValueUnsupported != nil && *x.ValueUnsupported != *y.ValueUnsupported) {
			return false
		}
	}
	return true
}

func floatsEqual(a, b []float64) bool {
	return slices.EqualFunc(a, b, func(x, y float64) bool { return math.Float64bits(x) == math.Float64bits(y) })
}

func dedEqual(a, b *vparquet5.DedicatedAttributes) bool {
	return slices.Equal(a.String01, b.String01) && slices.Equal(a.String02, b.String02) &&
		slices.Equal(a.String03, b.String03) && slices.Equal(a.String04, b.String04) &&
		slices.Equal(a.String05, b.String05) && slices.Equal(a.String06, b.String06) &&
		slices.Equal(a.String07, b.String07) && slices.Equal(a.String08, b.String08) &&
		slices.Equal(a.String09, b.String09) && slices.Equal(a.String10, b.String10) &&
		slices.Equal(a.String11, b.String11) && slices.Equal(a.String12, b.String12) &&
		slices.Equal(a.String13, b.String13) && slices.Equal(a.String14, b.String14) &&
		slices.Equal(a.String15, b.String15) && slices.Equal(a.String16, b.String16) &&
		slices.Equal(a.String17, b.String17) && slices.Equal(a.String18, b.String18) &&
		slices.Equal(a.String19, b.String19) && slices.Equal(a.String20, b.String20) &&
		slices.Equal(a.Int01, b.Int01) && slices.Equal(a.Int02, b.Int02) && slices.Equal(a.Int03, b.Int03) &&
		slices.Equal(a.Int04, b.Int04) && slices.Equal(a.Int05, b.Int05)
}

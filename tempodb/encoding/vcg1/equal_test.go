package vcg1

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/grafana/tempo/v3/tempodb/encoding/vcg1/internal/cgtest"
	"github.com/grafana/tempo/v3/tempodb/encoding/vparquet5"
)

// TestRowsEqualMatchesParquet: rowsEqual decides as parquet row equality (what the vparquet5
// compactor compares) for equal rows, different traces and single-field changes.
func TestRowsEqualMatchesParquet(t *testing.T) {
	dc := cgtest.Dedicated
	sch, _, _ := vparquet5.SchemaWithDynamicChanges(dc)
	conv := vparquet5.NewTraceConverter(dc)
	traces := cgtest.Traces(150, 21)
	rows := func() []*vparquet5.Trace {
		out := make([]*vparquet5.Trace, len(traces))
		for i, tr := range traces {
			out[i], _ = conv.Convert(tr.ID, tr.Trace, nil)
		}
		return out
	}
	a, b := rows(), rows()
	check := func(x, y *vparquet5.Trace) bool {
		want := sch.Deconstruct(nil, x).Equal(sch.Deconstruct(nil, y))
		require.Equal(t, want, rowsEqual(x, y))
		return want
	}
	for i := range a {
		require.True(t, check(a[i], b[i]))
		if i > 0 {
			require.False(t, check(a[i-1], b[i]))
		}
	}

	// single-field changes
	mutations := []func(tr *vparquet5.Trace){
		func(tr *vparquet5.Trace) { tr.RootSpanName += "x" },
		func(tr *vparquet5.Trace) { tr.ServiceStats[0].SpanCount++ },
		func(tr *vparquet5.Trace) { tr.ResourceSpans[0].Resource.DroppedAttributesCount++ },
		func(tr *vparquet5.Trace) { tr.ResourceSpans[0].ScopeSpans[0].Spans[0].Name += "x" },
		func(tr *vparquet5.Trace) { tr.ResourceSpans[0].ScopeSpans[0].Spans[0].StartTimeRounded3600++ },
		func(tr *vparquet5.Trace) { tr.ResourceSpans[0].ScopeSpans[0].Spans[0].ParentID++ },
		func(tr *vparquet5.Trace) {
			sp := &tr.ResourceSpans[0].ScopeSpans[0].Spans[0]
			sp.Attrs = append(sp.Attrs, vparquet5.Attribute{Key: "k"})
		},
		func(tr *vparquet5.Trace) {
			sp := &tr.ResourceSpans[0].ScopeSpans[0].Spans[0]
			sp.DedicatedAttributes.String01 = append(sp.DedicatedAttributes.String01, "x")
		},
		func(tr *vparquet5.Trace) {
			sp := &tr.ResourceSpans[0].ScopeSpans[0].Spans[0]
			sp.Events = append(sp.Events, vparquet5.Event{})
		},
		func(tr *vparquet5.Trace) {
			s := "{}"
			tr.ResourceSpans[0].Resource.Attrs = append(tr.ResourceSpans[0].Resource.Attrs, vparquet5.Attribute{Key: "u", ValueUnsupported: &s})
		},
		func(tr *vparquet5.Trace) { tr.ResourceSpans[0].ScopeSpans[0].Scope.Version += "x" },
	}
	for mi, mut := range mutations {
		x, y := rows(), rows()
		for i := range x {
			mut(y[i])
			require.False(t, check(x[i], y[i]), "mutation %d trace %d", mi, i)
		}
	}
	// nil and empty lists are the same row
	x, y := rows(), rows()
	for i := range x {
		for r := range y[i].ResourceSpans {
			if y[i].ResourceSpans[r].Resource.Attrs == nil {
				y[i].ResourceSpans[r].Resource.Attrs = []vparquet5.Attribute{}
			}
		}
		require.True(t, check(x[i], y[i]))
	}
}

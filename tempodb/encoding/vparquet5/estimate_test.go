package vparquet5

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/grafana/tempo/v3/pkg/util/test"
	"github.com/grafana/tempo/v3/tempodb/backend"
)

func TestSizeEstimatorMatchesRowEstimate(t *testing.T) {
	dcs := []backend.DedicatedColumns{
		nil,
		test.MakeDedicatedColumns(),
		{
			{Scope: backend.DedicatedColumnScopeSpan, Name: "foo", Type: backend.DedicatedColumnTypeString},
			{Scope: backend.DedicatedColumnScopeSpan, Name: "bar", Type: backend.DedicatedColumnTypeInt},
			{Scope: backend.DedicatedColumnScopeResource, Name: "res.foo", Type: backend.DedicatedColumnTypeString, Options: backend.DedicatedColumnOptions{backend.DedicatedColumnOptionBlob}},
			{Scope: backend.DedicatedColumnScopeEvent, Name: "ev.foo", Type: backend.DedicatedColumnTypeString},
		},
	}
	for _, dc := range dcs {
		sch, _, _ := SchemaWithDynamicChanges(dc)
		est := NewSizeEstimator(dc)
		check := func(tr *Trace) {
			row := sch.Deconstruct(nil, tr)
			require.Equal(t, estimateProtoSizeFromParquetRow(row), est.ProtoSize(tr))
			id, rootSpan, rootService, spans := countSpans(sch, row)
			gid, grootSpan, grootService, gspans := CountSpans(tr)
			require.Equal(t, []any{id, rootSpan, rootService, spans}, []any{gid, grootSpan, grootService, gspans})
		}

		meta := &backend.BlockMeta{DedicatedColumns: dc}
		for i := 0; i < 50; i++ {
			id := test.ValidTraceID(nil)
			tr, _ := traceToParquet(meta, id, test.MakeTraceWithSpanCount(1+i%3, 1+i%7, id), nil)
			check(tr)
		}
		mixed, _ := mixedArrayTestTrace()
		check(mixed)
		check(fullyPopulatedTestTrace(test.ValidTraceID(nil)))

		// the dedicated columns, set and empty
		tr := fullyPopulatedTestTrace(test.ValidTraceID(nil))
		tr.ResourceSpans[0].Resource.DedicatedAttributes.String01 = []string{"a", "bb"}
		tr.ResourceSpans[0].ScopeSpans[0].Spans[0].DedicatedAttributes.String01 = []string{"x"}
		tr.ResourceSpans[0].ScopeSpans[0].Spans[0].DedicatedAttributes.Int01 = []int64{1, 2, 3}
		tr.ResourceSpans[0].ScopeSpans[0].Spans[0].DedicatedAttributes.String02 = []string{}
		check(tr)

		// empty lists at every level
		check(&Trace{TraceID: []byte{1}})
		check(&Trace{TraceID: []byte{1}, ResourceSpans: []ResourceSpans{{}}})
		check(&Trace{TraceID: []byte{1}, ResourceSpans: []ResourceSpans{{ScopeSpans: []ScopeSpans{{}, {Spans: []Span{{}}}}}}})
		check(&Trace{TraceID: []byte{1}, ResourceSpans: []ResourceSpans{{ScopeSpans: []ScopeSpans{{Spans: []Span{{
			Events: []Event{{}}, Links: []Link{{}}, Attrs: []Attribute{{Key: "k"}, {Key: "a", IsArray: true}},
		}}}}}}})
	}
}

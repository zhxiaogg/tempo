package cgfile

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func testGroups() [7][]SchemaGroup {
	var g [7][]SchemaGroup
	g[TTraces] = []SchemaGroup{
		{Name: "tid", Cols: []int{TrTraceID}, CutWith: -1},
		{Name: "tmeta", Cols: []int{TrStart, TrDuration, TrRootService, TrRootSpan, TrFirstStat, TrFirstResource, TrFirstSpan}, CutWith: -1},
		{Name: "trest", Cols: []int{TrEnd}, CutWith: -1},
		{Name: "tcuts", Cols: []int{TrFirstScope, TrFirstEvent, TrFirstLink}, CutWith: 1},
	}
	g[TServiceStats] = []SchemaGroup{{Name: "ss", Cols: []int{SsServiceName, SsSpanCount, SsErrorCount}, CutWith: -1}}
	g[TResources] = []SchemaGroup{{Name: "res", Cols: []int{ResServiceName, ResDropped, ResFirstScope, ResFirstSpan}, CutWith: -1}}
	g[TScopes] = []SchemaGroup{{Name: "scope", Cols: []int{ScName, ScVersion, ScDropped, ScFirstSpan}, CutWith: -1}}
	g[TSpans] = []SchemaGroup{{Name: "sall", Cols: StoredCols(TSpans), CutWith: -1}}
	g[TEvents] = []SchemaGroup{{Name: "ev", Cols: []int{EvTime, EvName, EvDropped}, CutWith: -1}}
	g[TLinks] = []SchemaGroup{{Name: "ln", Cols: []int{LnTraceID, LnSpanID, LnTraceState, LnDropped}, CutWith: -1}}
	return g
}

func TestFileSchemaRoundTrip(t *testing.T) {
	s := TracingSchema(testGroups())
	b := s.Append(nil)
	c := &cursor{b: b}
	d, err := decodeFileSchema(c, len(b))
	require.NoError(t, err)
	require.True(t, c.done())
	require.Equal(t, s, d)

	// tempo-rs cg-trace column order: stored columns, carry-only columns, then attrs
	res := d.Tables[TResources]
	var names []string
	for _, c := range res.Cols {
		names = append(names, c.Name)
	}
	require.Equal(t, []string{"service_name", "dropped_attrs", "first_scope", "first_span", "span_count", "attrs"}, names)
	require.Equal(t, []int{3, 4}, res.Groups[len(res.Groups)-1].Carry)
	require.Equal(t, "span", d.Tables[TEvents].Cols[3].Name)

	m, err := mapSchema(d)
	require.NoError(t, err)
	require.Equal(t, []int{TrFirstScope, TrFirstEvent, TrFirstLink}, m.groups[TTraces][3].Cols)
	require.Equal(t, StoredCols(TSpans), m.groups[TSpans][0].Cols)
	for l := 0; l < NumLevels; l++ {
		require.Equal(t, len(d.Tables[OwnerTable[l]].Groups)-2, m.rm[l])
		require.Equal(t, len(d.Tables[OwnerTable[l]].Groups)-1, m.km[l])
	}
}

func TestMapSchemaRejects(t *testing.T) {
	s := TracingSchema(testGroups())
	s.Tables[TTraces].Cols[TrStart].Phys = PhysInt32
	_, err := mapSchema(s)
	require.ErrorContains(t, err, "traces.start_ns")

	s = TracingSchema(testGroups())
	km := &s.Tables[TEvents].Groups[len(s.Tables[TEvents].Groups)-1]
	km.Carry = nil
	_, err = mapSchema(s)
	require.ErrorContains(t, err, "carries 0 columns")
}

func TestHull(t *testing.T) {
	ps := []pageSpan{{0, 10, 0, 3}, {10, 20, 3, 7}, {20, 30, 8, 9}}
	for _, c := range []struct{ lo, hi, a, b, keep uint32 }{
		{0, 3, 0, 10, 0},   // owner 3 spills into page 1, but [0, 3) stops before it
		{3, 4, 0, 20, 10},  // owner 3 is on pages 0 and 1
		{7, 8, 10, 20, 10}, // one page
		{4, 4, 10, 10, 10}, // empty
		{10, 12, 30, 30, 30},
		{0, 100, 0, 30, 20},
	} {
		a, b, keep := hull(ps, c.lo, c.hi)
		require.Equal(t, [3]uint32{c.a, c.b, c.keep}, [3]uint32{a, b, keep}, "[%d,%d)", c.lo, c.hi)
	}
}

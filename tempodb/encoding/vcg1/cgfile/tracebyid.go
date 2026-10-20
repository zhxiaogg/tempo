package cgfile

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	"github.com/grafana/tempo/v3/pkg/tempopb"
)

// FindTraceByID reads the footer and rebuilds the trace with id, or returns nil if the block
// does not hold it. Stats cover every backend request of the lookup, footer included.
func (r *Reader) FindTraceByID(ctx context.Context, id []byte) (*tempopb.Trace, Stats, error) {
	q, err := r.NewRequest(ctx)
	if err != nil {
		return nil, Stats{}, err
	}
	tr, err := q.FindTraceByID(ctx, id)
	return tr, q.Stats(), err
}

// traceRows are the file-row ranges of one trace in every table it touches.
type traceRows [NumTables]Interval

// FindTraceByID locates the trace row by the trace_id page zones, then walks down the tree
// one wave per level: the trace's columns, then its service stats, resources and spans (with
// resource / span attribute pages picked by owner-row zones), then scopes, events and links
// (with their attribute pages), then array elements by attribute-row zones. Only pages
// covering the trace's rows are read.
func (q *Request) FindTraceByID(ctx context.Context, id []byte) (*tempopb.Trace, error) {
	t, found, err := q.findTraceRow(ctx, id)
	if err != nil || !found {
		return nil, err
	}
	var rg traceRows
	rg[TTraces] = Interval{t, t + 1}

	// wave: the trace row and the next one (for the end of its child ranges)
	if err := q.LoadRows(ctx, []RowSel{{T: TTraces, Ivs: withNext(q, TTraces, rg[TTraces])}}); err != nil {
		return nil, err
	}
	for _, x := range []struct{ child, col int }{
		{TServiceStats, TrFirstStat}, {TResources, TrFirstResource}, {TSpans, TrFirstSpan},
	} {
		if rg[x.child], err = q.childRange(TTraces, x.col, rg[TTraces], x.child); err != nil {
			return nil, err
		}
	}

	// wave: service stats, resources, spans, resource and span attribute pages
	raT, saT := AttrsTable(LResource), AttrsTable(LSpan)
	raIv, err := q.ownerIntervals(raT, AtOwner, rg[TResources])
	if err != nil {
		return nil, err
	}
	saIv, err := q.ownerIntervals(saT, AtOwner, rg[TSpans])
	if err != nil {
		return nil, err
	}
	if err := q.LoadRows(ctx, []RowSel{
		{T: TServiceStats, Ivs: []Interval{rg[TServiceStats]}},
		{T: TResources, Ivs: withNext(q, TResources, rg[TResources])},
		{T: TSpans, Ivs: withNext(q, TSpans, rg[TSpans])},
		{T: raT, Ivs: raIv},
		{T: saT, Ivs: saIv},
	}); err != nil {
		return nil, err
	}
	if rg[TScopes], err = q.childRange(TResources, ResFirstScope, rg[TResources], TScopes); err != nil {
		return nil, err
	}
	if rg[TEvents], err = q.childRange(TSpans, SpFirstEvent, rg[TSpans], TEvents); err != nil {
		return nil, err
	}
	if rg[TLinks], err = q.childRange(TSpans, SpFirstLink, rg[TSpans], TLinks); err != nil {
		return nil, err
	}
	if rg[raT], err = q.ownerRows(raT, AtOwner, raIv, rg[TResources]); err != nil {
		return nil, err
	}
	if rg[saT], err = q.ownerRows(saT, AtOwner, saIv, rg[TSpans]); err != nil {
		return nil, err
	}

	// wave: scopes, events, links and their attribute pages
	sels := []RowSel{
		{T: TScopes, Ivs: withNext(q, TScopes, rg[TScopes])},
		{T: TEvents, Ivs: []Interval{rg[TEvents]}},
		{T: TLinks, Ivs: []Interval{rg[TLinks]}},
	}
	lvIv := map[int][]Interval{}
	for _, l := range []int{LScope, LEvent, LLink} {
		iv, err := q.ownerIntervals(AttrsTable(l), AtOwner, rg[OwnerTable[l]])
		if err != nil {
			return nil, err
		}
		lvIv[l] = iv
		sels = append(sels, RowSel{T: AttrsTable(l), Ivs: iv})
	}
	if err := q.LoadRows(ctx, sels); err != nil {
		return nil, err
	}
	for _, l := range []int{LScope, LEvent, LLink} {
		if rg[AttrsTable(l)], err = q.ownerRows(AttrsTable(l), AtOwner, lvIv[l], rg[OwnerTable[l]]); err != nil {
			return nil, err
		}
	}

	// wave: array elements by attribute-row zones
	sels = sels[:0]
	elIv := [NumLevels][]Interval{}
	for l := 0; l < NumLevels; l++ {
		if elIv[l], err = q.ownerIntervals(ElemsTable(l), ElAttrRow, rg[AttrsTable(l)]); err != nil {
			return nil, err
		}
		sels = append(sels, RowSel{T: ElemsTable(l), Ivs: elIv[l]})
	}
	if err := q.LoadRows(ctx, sels); err != nil {
		return nil, err
	}
	for l := 0; l < NumLevels; l++ {
		if rg[ElemsTable(l)], err = q.ownerRows(ElemsTable(l), ElAttrRow, elIv[l], rg[AttrsTable(l)]); err != nil {
			return nil, err
		}
	}
	return q.assemble(&rg)
}

// findTraceRow returns the traces row of id: candidate pages by their bytes zone, then a
// binary search (trace ids are sorted across the file).
func (q *Request) findTraceRow(ctx context.Context, id []byte) (uint32, bool, error) {
	c, err := q.Column(TTraces, TrTraceID)
	if err != nil || c.Group == nil {
		return 0, false, err
	}
	var pages []int
	for i := 0; i < c.NumPages(); i++ {
		if c.AllNull(i) {
			continue
		}
		z := c.Zone(i)
		if z.Kind == ZoneBytes && (bytes.Compare(id, z.BMin) < 0 || bytes.Compare(id, z.BMax) > 0) {
			continue
		}
		pages = append(pages, i)
	}
	if len(pages) == 0 {
		return 0, false, nil
	}
	if err := q.Load(ctx, []PageSel{{T: TTraces, C: TrTraceID, Pages: pages}}, nil); err != nil {
		return 0, false, err
	}
	for _, pi := range pages {
		v := c.Values(pi)
		k := sort.Search(v.N, func(i int) bool { return bytes.Compare(v.Bytes(i, &c.dict), id) >= 0 })
		if k < v.N && bytes.Equal(v.Bytes(k, &c.dict), id) {
			return v.First + uint32(k), true, nil
		}
	}
	return 0, false, nil
}

// withNext extends rows [lo, hi) by the next row (clamped), for the end of child ranges.
func withNext(q *Request, t int, iv Interval) []Interval {
	if iv.Lo >= iv.Hi {
		return nil
	}
	return []Interval{{iv.Lo, min(iv.Hi+1, q.Rows(t))}}
}

// childRange is [first[lo], first[hi]) of offset column (pt, c) over parent rows [lo, hi);
// first[hi] is the child table's row count past the last parent row.
func (q *Request) childRange(pt, c int, parents Interval, child int) (Interval, error) {
	if parents.Lo >= parents.Hi {
		return Interval{}, nil
	}
	col, err := q.Column(pt, c)
	if err != nil {
		return Interval{}, err
	}
	a, err := intAt(col, parents.Lo)
	if err != nil {
		return Interval{}, err
	}
	b := int64(q.Rows(child))
	if parents.Hi < q.Rows(pt) {
		if b, err = intAt(col, parents.Hi); err != nil {
			return Interval{}, err
		}
	}
	if a > b {
		return Interval{}, fmt.Errorf("cgfile: %s.%s: child range [%d,%d) is inverted", TableName(pt), col.Spec.Name, a, b)
	}
	return Interval{uint32(a), uint32(b)}, nil
}

func intAt(c *Column, r uint32) (int64, error) {
	v, ok, err := c.Int(r)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("cgfile: %s.%s: null at row %d", TableName(c.T), c.Spec.Name, r)
	}
	return v, nil
}

// ownerIntervals returns the row intervals of the pages of table t whose sorted row-reference
// column c may hold a value in [lo, hi).
func (q *Request) ownerIntervals(t, c int, want Interval) ([]Interval, error) {
	if want.Lo >= want.Hi || q.Rows(t) == 0 {
		return nil, nil
	}
	col, err := q.Column(t, c)
	if err != nil {
		return nil, err
	}
	return col.PageIntervals(func(i int) bool {
		if col.AllNull(i) {
			return false
		}
		z := col.Zone(i)
		return z.Kind != ZoneInt || (z.Min < int64(want.Hi) && z.Max >= int64(want.Lo))
	}), nil
}

// ownerRows returns the rows (within the loaded intervals) whose column c is in [lo, hi):
// contiguous, since the column is sorted.
func (q *Request) ownerRows(t, c int, ivs []Interval, want Interval) (Interval, error) {
	if len(ivs) == 0 || want.Lo >= want.Hi {
		return Interval{}, nil
	}
	col, err := q.Column(t, c)
	if err != nil {
		return Interval{}, err
	}
	a, b := uint32(1<<32-1), uint32(0)
	for _, iv := range ivs {
		for r := iv.Lo; r < iv.Hi; {
			v, i, err := col.At(r)
			if err != nil {
				return Interval{}, err
			}
			end := min(iv.Hi, v.First+uint32(v.N))
			for ; r < end; r, i = r+1, i+1 {
				if o := v.Ints[i]; !v.IsNull(i) && o >= int64(want.Lo) && o < int64(want.Hi) {
					a, b = min(a, r), max(b, r+1)
				}
			}
		}
	}
	if a >= b {
		return Interval{}, nil
	}
	return Interval{a, b}, nil
}

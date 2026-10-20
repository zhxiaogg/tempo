package cgfile

import (
	"context"
	"slices"
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFindTraceByIDEdgeCasesVsVP5 compares the traces holding the rarer shapes of the block
// (links, array elements, unsupported values, stored parent span ids, empty arrays / empty
// values, first and last rows of every table) plus traces spread over the block.
func TestFindTraceByIDEdgeCasesVsVP5(t *testing.T) {
	r := openTestBlock(t)
	vp := openVP5(t)
	ctx := context.Background()
	q, err := r.NewRequest(ctx)
	require.NoError(t, err)
	e := &edges{t: t, q: q, ctx: ctx, reasons: map[uint32][]string{}}

	n := q.Rows(TTraces)
	for i := uint32(0); i < 50; i++ {
		e.add("spread", i*(n/50))
	}
	e.add("last trace", n-1)
	for _, k := range ends(q.Rows(TLinks)) {
		e.add("link", e.spanTrace(e.parentOf(TSpans, SpFirstLink, k)))
	}
	for l := 0; l < NumLevels; l++ {
		for _, a := range ends(q.Rows(AttrsTable(l))) {
			e.add(TableName(AttrsTable(l)), e.ownerTrace(l, e.int(AttrsTable(l), AtOwner, a)))
		}
		for _, x := range ends(q.Rows(ElemsTable(l))) {
			a := e.int(ElemsTable(l), ElAttrRow, x)
			e.add(TableName(ElemsTable(l)), e.ownerTrace(l, e.int(AttrsTable(l), AtOwner, a)))
		}
		for _, a := range e.nonNull(AttrsTable(l), AtUnsupported, 3) {
			e.add("unsupported "+levelNames[l], e.ownerTrace(l, e.int(AttrsTable(l), AtOwner, a)))
		}
		for _, vt := range []int64{VTEmptyArr, VTUnsupported, VTEmpty, VTIntArr, VTDoubleArr, VTBoolArr} {
			for _, a := range e.vtypeRows(l, vt, 2) {
				e.add("vtype "+levelNames[l], e.ownerTrace(l, e.int(AttrsTable(l), AtOwner, a)))
			}
		}
	}
	for _, s := range e.nonNull(TSpans, SpParentSpanID, 5) {
		e.add("stored parent_span_id", e.spanTrace(s))
	}

	rows := make([]uint32, 0, len(e.reasons))
	for row := range e.reasons {
		rows = append(rows, row)
	}
	slices.Sort(rows)
	ids := make([][]byte, len(rows))
	for i, row := range rows {
		c := e.load(TTraces, TrTraceID, row)
		b, _, err := c.Bytes(row)
		require.NoError(t, err)
		ids[i] = append([]byte(nil), b...)
		t.Logf("trace row %d %x: %v", row, ids[i], e.reasons[row])
	}
	// absent ids: below, above and between stored ids
	flipped := append([]byte(nil), ids[len(ids)/2]...)
	flipped[15] ^= 1
	ids = append(ids, make([]byte, 16), []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, flipped)
	all := compareIDs(t, r, vp, ids)
	logStats(t, all)
}

// TestFindTraceByIDConcurrent runs lookups of one Reader from several goroutines (run with
// -race) and checks they match sequential lookups.
func TestFindTraceByIDConcurrent(t *testing.T) {
	r := openTestBlock(t)
	ids := sampleIDs(t)
	if len(ids) > 40 {
		ids = ids[:40]
	}
	ctx := context.Background()
	want := make([][]byte, len(ids))
	for i, id := range ids {
		tr, _, err := r.FindTraceByID(ctx, id)
		require.NoError(t, err)
		want[i] = marshal(t, tr)
	}
	got := make([][]byte, len(ids))
	errs := make([]error, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr, _, err := r.FindTraceByID(ctx, id)
			if errs[i] = err; err == nil {
				got[i], errs[i] = tr.Marshal()
			}
		}()
	}
	wg.Wait()
	for i := range ids {
		require.NoError(t, errs[i])
		require.Equal(t, want[i], got[i], "%x", ids[i])
	}
}

type edges struct {
	t       *testing.T
	q       *Request
	ctx     context.Context
	reasons map[uint32][]string
}

func ends(n uint32) []uint32 {
	if n == 0 {
		return nil
	}
	return []uint32{0, n / 2, n - 1}
}

func (e *edges) add(why string, traceRow uint32) {
	e.reasons[traceRow] = append(e.reasons[traceRow], why)
}

// load loads the page of column (t, c) holding row and returns the column.
func (e *edges) load(t, c int, row uint32) *Column {
	col, err := e.q.Column(t, c)
	require.NoError(e.t, err)
	require.NoError(e.t, e.q.Load(e.ctx, []PageSel{{T: t, C: c, Pages: []int{col.Group.PageOf(row)}}}, nil))
	return col
}

func (e *edges) int(t, c int, row uint32) uint32 {
	v, err := intAt(e.load(t, c, row), row)
	require.NoError(e.t, err)
	return uint32(v)
}

// parentOf is the parent row of child x through offset column (pt, c): the last p with
// first[p] <= x.
func (e *edges) parentOf(pt, c int, x uint32) uint32 {
	n := int(e.q.Rows(pt))
	p := sort.Search(n, func(i int) bool { return e.int(pt, c, uint32(i)) > x })
	return uint32(p - 1)
}

func (e *edges) spanTrace(s uint32) uint32 { return e.parentOf(TTraces, TrFirstSpan, s) }

func (e *edges) ownerTrace(l int, o uint32) uint32 {
	switch l {
	case LResource:
		return e.parentOf(TTraces, TrFirstResource, o)
	case LScope:
		return e.parentOf(TTraces, TrFirstResource, e.parentOf(TResources, ResFirstScope, o))
	case LSpan:
		return e.spanTrace(o)
	case LEvent:
		return e.spanTrace(e.parentOf(TSpans, SpFirstEvent, o))
	}
	return e.spanTrace(e.parentOf(TSpans, SpFirstLink, o))
}

// nonNull returns up to n rows (from different pages) where column (t, c) is not null.
func (e *edges) nonNull(t, c, n int) []uint32 {
	col, err := e.q.Column(t, c)
	require.NoError(e.t, err)
	var out []uint32
	for i := 0; i < col.NumPages() && len(out) < n; i++ {
		if col.AllNull(i) {
			continue
		}
		require.NoError(e.t, e.q.Load(e.ctx, []PageSel{{T: t, C: c, Pages: []int{i}}}, nil))
		v := col.Values(i)
		for j := range v.N {
			if !v.IsNull(j) {
				out = append(out, v.First+uint32(j))
				break
			}
		}
	}
	return out
}

// vtypeRows returns up to n attribute rows of level l with value type vt, reading only the
// chunks whose vtype dictionary holds it.
func (e *edges) vtypeRows(l int, vt int64, n int) []uint32 {
	ta := AttrsTable(l)
	col, err := e.q.Column(ta, AtVType)
	require.NoError(e.t, err)
	var out []uint32
	for ci := range col.Group.Chunks {
		ch := &col.Group.Chunks[ci]
		if len(ch.Pages) == 0 || !ch.Dicts[col.K].Present() {
			continue
		}
		p0 := col.Group.PageOf(ch.Pages[0].First)
		require.NoError(e.t, e.q.Load(e.ctx, []PageSel{{T: ta, C: AtVType, Pages: []int{p0}}}, nil))
		if !slices.Contains(col.chunks[ci].ints, vt) {
			continue
		}
		for pi := p0; pi < p0+len(ch.Pages) && len(out) < n; pi++ {
			require.NoError(e.t, e.q.Load(e.ctx, []PageSel{{T: ta, C: AtVType, Pages: []int{pi}}}, nil))
			v := col.Values(pi)
			for j, x := range v.Ints {
				if x == vt {
					out = append(out, v.First+uint32(j))
					break
				}
			}
		}
		if len(out) >= n {
			break
		}
	}
	return out
}

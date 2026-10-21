package cgfile

import (
	"context"
	"errors"
	"fmt"
	"unsafe"

	"github.com/grafana/tempo/v3/pkg/util"
	"github.com/grafana/tempo/v3/tempodb/backend"
	"github.com/grafana/tempo/v3/tempodb/encoding/vparquet5"
)

// DefaultIterWindowSpans is the target span count of a TraceIterator window.
const DefaultIterWindowSpans = 1 << 16

// TraceIterator walks the traces of a block in file (trace-ID) order and rebuilds each as the
// vParquet5 row it was written from (the row cgwrite.Writer.Add took): stored order, the
// precomputed fields, and the dedicated attributes folded back into DedicatedAttributes under
// the block's dedicated-column config.
//
// It reads the block window by window (runs of anchors holding about WindowSpans spans): one
// Request per window, which loads every point-copy table over the window's rows in one wave
// and is dropped when the window is done, so memory is bounded by the window, not the block.
// The key-major (scan copy) tables are not read.
//
// Rows alias the window's decoded buffers (strings and byte slices are not copied): they stay
// valid for as long as they are referenced, but must not be modified in place. Slices are
// capacity-limited, so appending to them (as vparquet5.Combiner does) never overwrites another
// row. Not safe for concurrent use.
type TraceIterator struct {
	rd   *Reader
	f    *Footer
	ded  [3][]DedSlot // resource, span, event
	wins [][2]int
	wi   int

	q      *Request
	lo, hi [NumTables]uint32
	t      uint32 // next trace row
	ap, ep [NumLevels]uint32
	cols   [NumTables][]colCur
	stats  Stats
	err    error

	byLeft map[int32][]byte

	attrs  slab[vparquet5.Attribute]
	strs   slab[string]
	ints   slab[int64]
	dbls   slab[float64]
	bools  slab[bool]
	sstats slab[vparquet5.ServiceStats]
	rss    slab[vparquet5.ResourceSpans]
	sss    slab[vparquet5.ScopeSpans]
	spans  slab[vparquet5.Span]
	events slab[vparquet5.Event]
	links  slab[vparquet5.Link]
}

// NewTraceIterator returns an iterator over the block of f. dc is the block's dedicated-column
// config (the block meta's, as given to cgwrite); windowSpans <= 0 is DefaultIterWindowSpans.
func (r *Reader) NewTraceIterator(f *Footer, dc backend.DedicatedColumns, windowSpans int) *TraceIterator {
	if windowSpans <= 0 {
		windowSpans = DefaultIterWindowSpans
	}
	it := &TraceIterator{rd: r, f: f, byLeft: map[int32][]byte{}}
	it.ded[0] = DedicatedSlots(dc, backend.DedicatedColumnScopeResource)
	it.ded[1] = DedicatedSlots(dc, backend.DedicatedColumnScopeSpan)
	it.ded[2] = DedicatedSlots(dc, backend.DedicatedColumnScopeEvent)
	for a, n := 0, len(f.Anchors); a < n; {
		e := a + 1
		for e < n && f.Anchor(e, TSpans)-f.Anchor(a, TSpans) < uint32(windowSpans) {
			e++
		}
		it.wins = append(it.wins, [2]int{a, e})
		a = e
	}
	return it
}

// Stats returns the backend requests of the windows read so far.
func (it *TraceIterator) Stats() Stats {
	s := it.stats
	if it.q != nil {
		s.Add(it.q.Stats())
	}
	return s
}

// Close drops the current window.
func (it *TraceIterator) Close() { it.drop() }

func (it *TraceIterator) drop() {
	it.resetSlabs()
	if it.q != nil {
		it.stats.Add(it.q.Stats())
		it.q = nil
		it.cols = [NumTables][]colCur{}
	}
}

// pointTables are the tables a TraceIterator reads.
var pointTables = func() []int {
	out := []int{TTraces, TServiceStats, TResources, TScopes, TSpans, TEvents, TLinks}
	for l := 0; l < NumLevels; l++ {
		out = append(out, AttrsTable(l), ElemsTable(l))
	}
	return out
}()

// ensure loads the window of the next trace; false at the end of the block.
func (it *TraceIterator) ensure(ctx context.Context) (bool, error) {
	if it.err != nil {
		return false, it.err
	}
	for it.q == nil || it.t >= it.hi[TTraces] {
		if it.q != nil {
			it.drop()
			it.wi++
		}
		if it.wi >= len(it.wins) {
			return false, nil
		}
		if err := ctx.Err(); err != nil {
			it.err = err
			return false, err
		}
		if err := it.load(ctx); err != nil {
			it.err = err
			return false, err
		}
	}
	return true, nil
}

func (it *TraceIterator) load(ctx context.Context) error {
	a, b := it.wins[it.wi][0], it.wins[it.wi][1]
	q := it.rd.NewRequestWithFooter(it.f)
	var sels []RowSel
	for _, t := range pointTables {
		it.lo[t], it.hi[t] = it.f.Anchor(a, t), it.f.Anchor(b, t)
		if it.lo[t] < it.hi[t] {
			sels = append(sels, RowSel{T: t, Ivs: []Interval{{it.lo[t], it.hi[t]}}})
		}
	}
	if err := q.LoadRows(ctx, sels); err != nil {
		it.stats.Add(q.Stats())
		return err
	}
	for _, t := range pointTables {
		cs := Cols(t)
		it.cols[t] = make([]colCur, len(cs))
		for _, c := range StoredCols(t) {
			col, err := q.Column(t, c)
			if err != nil {
				return err
			}
			it.cols[t][c].c = col
		}
	}
	for l := 0; l < NumLevels; l++ {
		it.ap[l], it.ep[l] = it.lo[AttrsTable(l)], it.lo[ElemsTable(l)]
	}
	// New backing arrays per window: rows alias the window's decoded pages, so a backing array
	// shared with the previous window's rows would keep that window alive.
	it.resetSlabs()
	it.q, it.t = q, it.lo[TTraces]
	return nil
}

// Peek returns the id of the next trace (nil at the end) without consuming it. The id aliases
// the window's buffers.
func (it *TraceIterator) Peek(ctx context.Context) ([]byte, error) {
	ok, err := it.ensure(ctx)
	if !ok || err != nil {
		return nil, err
	}
	b, _ := it.bytes(TTraces, TrTraceID, it.t)
	return b, it.err
}

// Skip consumes the next trace.
func (it *TraceIterator) Skip(ctx context.Context) error {
	ok, err := it.ensure(ctx)
	if !ok || err != nil {
		return err
	}
	it.trace(it.t) // the attribute positions only advance by rebuilding
	if it.err != nil {
		return it.err
	}
	it.t++
	return nil
}

// Next rebuilds and consumes the next trace (nil at the end).
func (it *TraceIterator) Next(ctx context.Context) (*vparquet5.Trace, error) {
	ok, err := it.ensure(ctx)
	if !ok || err != nil {
		return nil, err
	}
	tr := it.trace(it.t)
	if it.err != nil {
		return nil, it.err
	}
	it.t++
	return tr, nil
}

func (it *TraceIterator) resetSlabs() {
	it.attrs.reset()
	it.strs.reset()
	it.ints.reset()
	it.dbls.reset()
	it.bools.reset()
	it.sstats.reset()
	it.rss.reset()
	it.sss.reset()
	it.spans.reset()
	it.events.reset()
	it.links.reset()
}

// colCur reads a loaded column by file row, caching the current page.
type colCur struct {
	c      *Column
	v      *Values
	lo, hi uint32
}

var errNotLoaded = errors.New("cgfile: column not loaded")

func (it *TraceIterator) at(t, c int, r uint32) (*Values, int, *Column) {
	k := &it.cols[t][c]
	if k.v == nil || r < k.lo || r >= k.hi {
		if k.c == nil {
			it.fail(fmt.Errorf("%w: %s.%d", errNotLoaded, TableName(t), c))
			return nil, 0, nil
		}
		v, i, err := k.c.At(r)
		if err != nil {
			it.fail(err)
			return nil, 0, nil
		}
		k.v, k.lo, k.hi = v, v.First, v.First+uint32(v.N)
		return v, i, k.c
	}
	return k.v, int(r - k.lo), k.c
}

func (it *TraceIterator) fail(err error) {
	if it.err == nil {
		it.err = err
	}
}

func (it *TraceIterator) i64(t, c int, r uint32) int64 {
	v, i, _ := it.at(t, c, r)
	if v == nil || v.IsNull(i) {
		return 0
	}
	return v.Ints[i]
}

func (it *TraceIterator) bytes(t, c int, r uint32) ([]byte, bool) {
	v, i, col := it.at(t, c, r)
	if v == nil || v.IsNull(i) {
		return nil, false
	}
	return v.Bytes(i, &col.dict), true
}

func (it *TraceIterator) str(t, c int, r uint32) string {
	b, _ := it.bytes(t, c, r)
	return zstr(b)
}

func (it *TraceIterator) f64(t, c int, r uint32) (float64, bool) {
	v, i, _ := it.at(t, c, r)
	if v == nil || v.IsNull(i) {
		return 0, false
	}
	return v.Floats[i], true
}

func (it *TraceIterator) bool(t, c int, r uint32) (bool, bool) {
	v, i, _ := it.at(t, c, r)
	if v == nil || v.IsNull(i) {
		return false, false
	}
	return v.Bools[i], true
}

func (it *TraceIterator) isNull(t, c int, r uint32) bool {
	v, i, _ := it.at(t, c, r)
	return v == nil || v.IsNull(i)
}

// zstr is a zero-copy string over immutable decoded bytes.
func zstr(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(unsafe.SliceData(b), len(b))
}

// kids returns the child rows [first[p], end) of parent row p of table pt through its offset
// column c: end is the next parent row's offset, or the window end of the child table.
func (it *TraceIterator) kids(pt, c int, p uint32, child int) (uint32, uint32) {
	a := uint32(it.i64(pt, c, p))
	b := it.hi[child]
	if p+1 < it.hi[pt] {
		b = uint32(it.i64(pt, c, p+1))
	}
	if a > b || a < it.lo[child] || b > it.hi[child] {
		it.fail(fmt.Errorf("cgfile: %s row %d: child range [%d,%d) outside window [%d,%d) of %s", TableName(pt), p, a, b, it.lo[child], it.hi[child], TableName(child)))
		return 0, 0
	}
	return a, b
}

func (it *TraceIterator) trace(t uint32) *vparquet5.Trace {
	tr := &vparquet5.Trace{
		StartTimeUnixNano: uint64(it.i64(TTraces, TrStart, t)),
		EndTimeUnixNano:   uint64(it.i64(TTraces, TrEnd, t)),
		DurationNano:      uint64(it.i64(TTraces, TrDuration, t)),
		RootServiceName:   it.str(TTraces, TrRootService, t),
		RootSpanName:      it.str(TTraces, TrRootSpan, t),
	}
	tr.TraceID, _ = it.bytes(TTraces, TrTraceID, t)
	tr.TraceIDText = util.TraceIDToHexString(tr.TraceID)

	if a, b := it.kids(TTraces, TrFirstStat, t, TServiceStats); b > a {
		for r := a; r < b; r++ {
			it.sstats.push(vparquet5.ServiceStats{
				ServiceName: it.str(TServiceStats, SsServiceName, r),
				SpanCount:   uint32(it.i64(TServiceStats, SsSpanCount, r)),
				ErrorCount:  uint32(it.i64(TServiceStats, SsErrorCount, r)),
			})
		}
		tr.ServiceStats = it.sstats.end()
	}

	// derived parent span ids: the first span of the trace (trace order) per nested_left
	if len(it.byLeft) > 1024 {
		// clear costs the map's capacity, which a big trace leaves large for every later trace
		it.byLeft = make(map[int32][]byte)
	} else {
		clear(it.byLeft)
	}
	s0, s1 := it.kids(TTraces, TrFirstSpan, t, TSpans)
	for s := s0; s < s1; s++ {
		left := int32(it.i64(TSpans, SpNestedLeft, s))
		if _, ok := it.byLeft[left]; !ok {
			it.byLeft[left], _ = it.bytes(TSpans, SpSpanID, s)
		}
	}

	r0, r1 := it.kids(TTraces, TrFirstResource, t, TResources)
	if it.err != nil {
		return nil
	}
	ap0 := it.ap[LSpan]
	// Resources are built in the slab first, then filled in place (their scopes use the
	// slabs too).
	for r := r0; r < r1; r++ {
		it.rss.push(vparquet5.ResourceSpans{})
	}
	rss := it.rss.end()
	for r := r0; r < r1; r++ {
		rs := &rss[r-r0]
		rs.Resource.ServiceName = it.str(TResources, ResServiceName, r)
		rs.Resource.DroppedAttributesCount = int32(it.i64(TResources, ResDropped, r))
		rs.Resource.Attrs = it.ownerAttrs(LResource, r, &rs.Resource.DedicatedAttributes)

		c0, c1 := it.kids(TResources, ResFirstScope, r, TScopes)
		for c := c0; c < c1; c++ {
			it.sss.push(vparquet5.ScopeSpans{})
		}
		sss := it.sss.end()
		for c := c0; c < c1; c++ {
			ss := &sss[c-c0]
			ss.Scope.Name = it.str(TScopes, ScName, c)
			ss.Scope.Version = it.str(TScopes, ScVersion, c)
			ss.Scope.DroppedAttributesCount = int32(it.i64(TScopes, ScDropped, c))
			ss.Scope.Attrs = it.ownerAttrs(LScope, c, nil)

			p0, p1 := it.kids(TScopes, ScFirstSpan, c, TSpans)
			for s := p0; s < p1; s++ {
				it.spans.push(vparquet5.Span{})
			}
			sps := it.spans.end()
			for s := p0; s < p1; s++ {
				it.span(s, &sps[s-p0])
			}
			ss.Spans = sps
			ss.SpanCount = int32(len(sps))
		}
		rs.ScopeSpans = sss
	}
	tr.ResourceSpans = rss
	// A big trace gets backing arrays of its own: the next traces' rows must not share (and so
	// keep alive) the arrays holding it.
	if s1-s0 > slabN/4 || it.ap[LSpan]-ap0 > slabN {
		it.resetSlabs()
	}
	return tr
}

func (it *TraceIterator) span(s uint32, sp *vparquet5.Span) {
	sp.SpanID, _ = it.bytes(TSpans, SpSpanID, s)
	sp.ParentID = int32(it.i64(TSpans, SpParentID, s))
	if b, ok := it.bytes(TSpans, SpParentSpanID, s); ok {
		sp.ParentSpanID = b
	} else if sp.ParentID > 0 {
		sp.ParentSpanID = it.byLeft[sp.ParentID]
	}
	sp.NestedSetLeft = int32(it.i64(TSpans, SpNestedLeft, s))
	sp.NestedSetRight = int32(it.i64(TSpans, SpNestedRight, s))
	sp.ChildCount = int32(it.i64(TSpans, SpChildCount, s))
	sp.Name = it.str(TSpans, SpName, s)
	sp.Kind = int(it.i64(TSpans, SpKind, s))
	sp.TraceState = it.str(TSpans, SpTraceState, s)
	sp.StartTimeUnixNano = uint64(it.i64(TSpans, SpStart, s))
	sp.DurationNano = uint64(it.i64(TSpans, SpDuration, s))
	sp.StatusCode = int(it.i64(TSpans, SpStatusCode, s))
	sp.StatusMessage = it.str(TSpans, SpStatusMessage, s)
	sp.DroppedAttributesCount = int32(it.i64(TSpans, SpDroppedAttrs, s))
	sp.DroppedEventsCount = int32(it.i64(TSpans, SpDroppedEvents, s))
	sp.DroppedLinksCount = int32(it.i64(TSpans, SpDroppedLinks, s))
	sp.StartTimeRounded15 = uint32(it.i64(TSpans, SpRounded15, s))
	sp.StartTimeRounded60 = uint32(it.i64(TSpans, SpRounded60, s))
	sp.StartTimeRounded300 = uint32(it.i64(TSpans, SpRounded300, s))
	sp.StartTimeRounded3600 = uint32(it.i64(TSpans, SpRounded3600, s))
	sp.Attrs = it.ownerAttrs(LSpan, s, &sp.DedicatedAttributes)

	if e0, e1 := it.kids(TSpans, SpFirstEvent, s, TEvents); e1 > e0 {
		for e := e0; e < e1; e++ {
			it.events.push(vparquet5.Event{})
		}
		evs := it.events.end()
		for e := e0; e < e1; e++ {
			ev := &evs[e-e0]
			ev.TimeSinceStartNano = uint64(it.i64(TEvents, EvTime, e))
			ev.Name = it.str(TEvents, EvName, e)
			ev.DroppedAttributesCount = int32(it.i64(TEvents, EvDropped, e))
			ev.Attrs = it.ownerAttrs(LEvent, e, &ev.DedicatedAttributes)
		}
		sp.Events = evs
	}
	if k0, k1 := it.kids(TSpans, SpFirstLink, s, TLinks); k1 > k0 {
		for k := k0; k < k1; k++ {
			it.links.push(vparquet5.Link{})
		}
		lns := it.links.end()
		for k := k0; k < k1; k++ {
			ln := &lns[k-k0]
			ln.TraceID, _ = it.bytes(TLinks, LnTraceID, k)
			ln.SpanID, _ = it.bytes(TLinks, LnSpanID, k)
			ln.TraceState = it.str(TLinks, LnTraceState, k)
			ln.DroppedAttributesCount = int32(it.i64(TLinks, LnDropped, k))
			ln.Attrs = it.ownerAttrs(LLink, k, nil)
		}
		sp.Links = lns
	}
}

// ownerAttrs rebuilds the attributes of owner row o of level l (the attribute rows are sorted
// by owner and read in order) and folds the dedicated ones into ded.
func (it *TraceIterator) ownerAttrs(l int, o uint32, ded *vparquet5.DedicatedAttributes) []vparquet5.Attribute {
	ta, te := AttrsTable(l), ElemsTable(l)
	for it.ap[l] < it.hi[ta] {
		a := it.ap[l]
		owner := uint32(it.i64(ta, AtOwner, a))
		if owner > o {
			break
		}
		if owner < o {
			it.fail(fmt.Errorf("cgfile: %s row %d: owner %d before owner %d", TableName(ta), a, owner, o))
			return nil
		}
		it.attrs.push(it.attr(l, a, te))
		it.ap[l]++
	}
	attrs := it.attrs.end()
	if ded == nil || len(attrs) == 0 {
		return attrs
	}
	var slots []DedSlot
	switch l {
	case LResource:
		slots = it.ded[0]
	case LSpan:
		slots = it.ded[1]
	case LEvent:
		slots = it.ded[2]
	}
	return foldDedicated(attrs, slots, ded)
}

// foldDedicated moves the dedicated attributes the writer appended after the generic ones
// (in slot order, only the non-empty ones; one value as a scalar, several as an array) back
// into their spare columns.
func foldDedicated(attrs []vparquet5.Attribute, slots []DedSlot, ded *vparquet5.DedicatedAttributes) []vparquet5.Attribute {
	j := len(attrs)
	for k := len(slots) - 1; k >= 0 && j > 0; k-- {
		s, a := slots[k], &attrs[j-1]
		if a.Key != s.Name || a.ValueUnsupported != nil {
			continue
		}
		if s.IsInt {
			if n := len(a.ValueInt); len(a.Value) == 0 && len(a.ValueDouble) == 0 && len(a.ValueBool) == 0 && ((n == 1 && !a.IsArray) || (n > 1 && a.IsArray)) {
				*DedInt(ded, s.Slot) = a.ValueInt
				j--
			}
			continue
		}
		if n := len(a.Value); len(a.ValueInt) == 0 && len(a.ValueDouble) == 0 && len(a.ValueBool) == 0 && ((n == 1 && !a.IsArray) || (n > 1 && a.IsArray)) {
			*DedStr(ded, s.Slot) = a.Value
			j--
		}
	}
	if j == 0 {
		return nil
	}
	return attrs[:j:j]
}

// attr rebuilds attribute row a of level l (consuming its element rows).
func (it *TraceIterator) attr(l int, a uint32, te int) vparquet5.Attribute {
	ta := AttrsTable(l)
	at := vparquet5.Attribute{Key: it.str(ta, AtKey, a)}
	switch vt := it.i64(ta, AtVType, a); vt {
	case VTStr:
		b, ok := it.bytes(ta, AtStr, a)
		if !ok {
			b, _ = it.bytes(ta, AtStrRaw, a)
		}
		it.strs.push(zstr(b))
		at.Value = it.strs.end()
	case VTInt:
		it.ints.push(it.i64(ta, AtInt, a))
		at.ValueInt = it.ints.end()
	case VTDouble:
		v, _ := it.f64(ta, AtDbl, a)
		it.dbls.push(v)
		at.ValueDouble = it.dbls.end()
	case VTBool:
		v, _ := it.bool(ta, AtBool, a)
		it.bools.push(v)
		at.ValueBool = it.bools.end()
	case VTStrArr, VTIntArr, VTDoubleArr, VTBoolArr, VTEmptyArr:
		at.IsArray = true
		for it.ep[l] < it.hi[te] {
			e := it.ep[l]
			ar := uint32(it.i64(te, ElAttrRow, e))
			if ar > a {
				break
			}
			if ar < a {
				it.fail(fmt.Errorf("cgfile: %s row %d: attribute row %d before %d", TableName(te), e, ar, a))
				return at
			}
			switch vt {
			case VTStrArr:
				b, ok := it.bytes(te, ElStr, e)
				if !ok {
					b, _ = it.bytes(te, ElStrRaw, e)
				}
				it.strs.push(zstr(b))
			case VTIntArr:
				it.ints.push(it.i64(te, ElInt, e))
			case VTDoubleArr:
				v, _ := it.f64(te, ElDbl, e)
				it.dbls.push(v)
			case VTBoolArr:
				v, _ := it.bool(te, ElBool, e)
				it.bools.push(v)
			}
			it.ep[l]++
		}
		switch vt {
		case VTStrArr:
			at.Value = it.strs.end()
		case VTIntArr:
			at.ValueInt = it.ints.end()
		case VTDoubleArr:
			at.ValueDouble = it.dbls.end()
		case VTBoolArr:
			at.ValueBool = it.bools.end()
		}
	case VTUnsupported:
		b, _ := it.bytes(ta, AtUnsupported, a)
		s := zstr(b)
		at.ValueUnsupported = &s
	case VTEmpty:
	default:
		it.fail(fmt.Errorf("cgfile: %s row %d: unknown vtype %d", TableName(ta), a, vt))
	}
	return at
}

const slabN = 4096

// slab hands out capacity-limited sub-slices of shared backing arrays, so rebuilding a trace
// costs a few large allocations instead of one per list.
type slab[T any] struct {
	buf   []T
	start int
}

func (s *slab[T]) push(v T) {
	if len(s.buf) == cap(s.buf) {
		n := make([]T, 0, max(slabN, 2*(len(s.buf)-s.start)))
		n = append(n, s.buf[s.start:]...)
		s.buf, s.start = n, 0
	}
	s.buf = append(s.buf, v)
}

// end returns the values pushed since the last end (nil if none). A backing array grown past
// slabN for one big list is not reused: kept as the current array, it would keep alive every
// later row cut from it (and all that those rows reference).
func (s *slab[T]) end() []T {
	if len(s.buf) == s.start {
		return nil
	}
	out := s.buf[s.start:len(s.buf):len(s.buf)]
	s.start = len(s.buf)
	if cap(s.buf) > slabN {
		s.reset()
	}
	return out
}

// reset drops the backing array (the rows cut from it keep it as long as they need it).
func (s *slab[T]) reset() { s.buf, s.start = nil, 0 }

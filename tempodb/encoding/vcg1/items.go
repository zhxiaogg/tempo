package vcg1

import (
	"sort"

	"github.com/parquet-go/parquet-go"

	"github.com/grafana/tempo/v3/pkg/parquetquery"
	"github.com/grafana/tempo/v3/pkg/traceql"
	"github.com/grafana/tempo/v3/pkg/util"
	cg "github.com/grafana/tempo/v3/tempodb/encoding/vcg1/cgfile"
)

// An item is one of vparquet5's iterators at one level: a column (with the OR of the
// predicates on it) or the level's generic attribute iterator. emit appends what the iterator
// would yield for owner row o and reports whether it yielded anything (an inner-join match).
type item interface {
	need(p *pass, ld *loader) error
	prepare(p *pass) error
	emit(o uint32, e *emitter) bool
	// emitNull is emit for the null row of a span without events or links.
	emitNull(e *emitter) bool
}

// emitter is the destination of one owner's values.
type emitter struct {
	attrs *[]attrVal
	sp    *span
	tr    *traceOut
}

func (e *emitter) add(a traceql.Attribute, s traceql.Static) {
	*e.attrs = append(*e.attrs, attrVal{a: a, s: s})
}

// traceOut is the trace-level data of one trace.
type traceOut struct {
	id          []byte
	start, dur  uint64
	rootSpan    string
	rootService string
	stats       map[string]traceql.ServiceStats
	attrs       []attrVal
}

// colItem is a fixed column of an owner table.
type colItem struct {
	t, c int
	pred parquetquery.Predicate // nil: every row
	put  func(e *emitter, v parquet.Value)
	// sparse: second-pass span/trace columns load only the driver's rows
	cur  *cursor
	memo strMemo
	// derive, if set, replaces a null value (spans.parent_span_id)
	derive func(r uint32) []byte
	parent *parentIDs
}

func (c *colItem) need(p *pass, ld *loader) error {
	ld.col(c.t, c.c, p.rows(c.t))
	if c.t == cg.TSpans && c.c == cg.SpParentSpanID {
		for _, x := range []int{cg.SpParentID, cg.SpNestedLeft, cg.SpSpanID} {
			ld.allRows(cg.TSpans, x)
		}
		ld.allRows(cg.TTraces, cg.TrFirstSpan)
	}
	return nil
}

func (c *colItem) prepare(p *pass) (err error) {
	if c.cur, err = p.w.cursor(c.t, c.c); err != nil {
		return err
	}
	if c.t == cg.TSpans && c.c == cg.SpParentSpanID {
		if c.parent, err = newParentIDs(p.w); err != nil {
			return err
		}
		c.derive = c.parent.derive
	}
	return nil
}

// parentIDs derives the parent span id the CG writer left null: the span id of the first span
// of the trace whose nested-set left equals the span's parent_id (empty for a root).
type parentIDs struct {
	w             *window
	pid, left, id *cursor
	first         []uint32
	tr            uint32
	byLeft        map[int64][]byte
}

func newParentIDs(w *window) (*parentIDs, error) {
	d := &parentIDs{w: w, tr: none}
	var err error
	if d.pid, err = w.cursor(cg.TSpans, cg.SpParentID); err != nil {
		return nil, err
	}
	if d.left, err = w.cursor(cg.TSpans, cg.SpNestedLeft); err != nil {
		return nil, err
	}
	if d.id, err = w.cursor(cg.TSpans, cg.SpSpanID); err != nil {
		return nil, err
	}
	d.first, err = w.firsts(cg.TTraces, cg.TrFirstSpan, cg.TSpans)
	return d, err
}

func (d *parentIDs) derive(o uint32) []byte {
	pid := d.pid.int(o)
	if pid <= 0 {
		return []byte{}
	}
	s := o - d.w.lo[cg.TSpans]
	t := uint32(sort.Search(len(d.first), func(i int) bool { return d.first[i] > s }) - 1)
	if t != d.tr {
		d.tr, d.byLeft = t, map[int64][]byte{}
		for r := d.first[t]; r < d.first[t+1]; r++ {
			l := d.left.int(d.w.lo[cg.TSpans] + r)
			if _, ok := d.byLeft[l]; !ok {
				d.byLeft[l], _ = d.id.bytes(d.w.lo[cg.TSpans] + r)
			}
		}
	}
	if b, ok := d.byLeft[pid]; ok {
		return b
	}
	return []byte{}
}

func (c *colItem) emit(o uint32, e *emitter) bool {
	vs, i := c.cur.at(o)
	var v parquet.Value
	switch {
	case vs.IsNull(i) && c.derive != nil:
		v = parquet.ByteArrayValue(c.derive(o))
	case c.pred != nil && vs.Dict && !vs.IsNull(i):
		if !c.memo.keep(vs.Codes[i], func() bool { return c.pred.KeepValue(valueOf(vs, i, c.cur.c.Dict())) }) {
			return false
		}
		v = valueOf(vs, i, c.cur.c.Dict())
		c.put(e, v)
		return true
	default:
		v = valueOf(vs, i, c.cur.c.Dict())
	}
	if c.pred != nil && !c.pred.KeepValue(v) {
		return false
	}
	c.put(e, v)
	return true
}

func (c *colItem) emitNull(e *emitter) bool {
	if c.pred != nil && !c.pred.KeepValue(parquet.Value{}) {
		return false
	}
	c.put(e, parquet.Value{})
	return true
}

// dedItem is a dedicated column: the values of the owner's attribute of the column's type,
// from the key's scan-copy runs. A missing value is a null row ("nil" when the predicate keeps
// nulls), as vparquet5's list column yields.
type dedItem struct {
	k      *kmKey
	vt     uint8 // VTStr or VTInt
	pred   parquetquery.Predicate
	nilOp  bool // OpNotExists: only owners without a value, as "nil"
	attr   traceql.Attribute
	cur    int
	memo   []strMemo
	hasNil bool
}

func (d *dedItem) need(p *pass, ld *loader) error { return d.k.need(p.w, ld) }

func (d *dedItem) prepare(*pass) error {
	d.cur = 0
	d.memo = d.memo[:0]
	d.hasNil = d.pred == nil || d.pred.KeepValue(parquet.Value{})
	return nil
}

func (d *dedItem) emit(o uint32, e *emitter) bool {
	ents := d.k.owner(o, &d.cur)
	found, matched := false, false
	for _, en := range ents {
		if d.k.vtype(en) != d.vt {
			continue
		}
		found = true
		if d.nilOp {
			break
		}
		v := d.k.value(en)
		if !d.keep(en, v) {
			continue
		}
		matched = true
		if d.vt == cg.VTInt {
			e.add(d.attr, traceql.NewStaticInt(int(v.Int64())))
		} else {
			e.add(d.attr, traceql.NewStaticString(bstr(v.ByteArray())))
		}
	}
	if !found && (d.nilOp || d.hasNil) {
		e.add(d.attr, traceql.NewStaticString("nil"))
		return true
	}
	return matched
}

func (d *dedItem) emitNull(e *emitter) bool {
	if d.nilOp || d.hasNil {
		e.add(d.attr, traceql.NewStaticString("nil"))
		return true
	}
	return false
}

func (d *dedItem) keep(en kmEnt, v parquet.Value) bool {
	if d.pred == nil {
		return true
	}
	rd := d.k.rds[en.run]
	if rd.Vals != nil && rd.Vals.Dict && !v.IsNull() {
		for int(en.run) >= len(d.memo) {
			d.memo = append(d.memo, nil)
		}
		return d.memo[en.run].keep(rd.Vals.Codes[en.i], func() bool { return d.pred.KeepValue(v) })
	}
	return d.pred.KeepValue(v)
}

// genItem is a level's generic attribute iterator (createAttributeIterator): attributes whose
// key is one of keys, valued by their elements that pass the predicate of their type.
type genItem struct {
	level int
	keys  []*kmKey
	ded   []int // per key: dedicated scalar vtype excluded from generic attributes, or -1
	preds [4]parquetquery.Predicate
	has   [4]bool
	join  bool // allConditions with one value type: only attributes with a passing element
	attr  func(string) traceql.Attribute

	hits     []hit
	cur      int
	runMemos map[[2]int]*strMemo // (key, run) -> string predicate per run dictionary code
}

type hit struct {
	o  uint32
	av attrVal
}

const (
	tStr = iota
	tInt
	tFlt
	tBool
)

func vtClass(vt uint8) int {
	switch vt {
	case cg.VTStr, cg.VTStrArr:
		return tStr
	case cg.VTInt, cg.VTIntArr:
		return tInt
	case cg.VTDouble, cg.VTDoubleArr:
		return tFlt
	case cg.VTBool, cg.VTBoolArr:
		return tBool
	}
	return -1
}

func (g *genItem) need(p *pass, ld *loader) error {
	for _, k := range g.keys {
		if err := k.need(p.w, ld); err != nil {
			return err
		}
	}
	return nil
}

func (g *genItem) prepare(p *pass) error {
	g.hits, g.cur = g.hits[:0], 0
	clear(g.runMemos)
	var multi []uint32
	for ki, k := range g.keys {
		ents := k.ents
		for i := 0; i < len(ents); {
			j, n, single := i, 0, -1
			for ; j < len(ents) && ents[j].o == ents[i].o; j++ {
				if g.ded[ki] >= 0 && int(k.vtype(ents[j])) == g.ded[ki] {
					continue
				}
				n++
				single = j
			}
			switch {
			case n == 1:
				en := ents[single]
				v := k.value(en)
				var buf [4][]parquet.Value
				if c := vtClass(k.vtype(en)); c >= 0 && g.has[c] && !v.IsNull() && g.keepRun(ki, en, c, v) {
					buf[c] = []parquet.Value{v}
				}
				if s, ok := g.static(&buf); ok || !g.join {
					g.hits = append(g.hits, hit{o: en.o, av: attrVal{a: g.attr(k.key), s: s}})
				}
			case n > 1:
				if len(multi) == 0 || multi[len(multi)-1] != ents[i].o {
					multi = append(multi, ents[i].o)
				}
			}
			i = j
		}
	}
	if len(multi) > 0 {
		sort.Slice(multi, func(i, j int) bool { return multi[i] < multi[j] })
		multi = dedupe(multi)
		pc, err := pointCopy(p.ctx, p.w, g.level, multi, true)
		if err != nil {
			return err
		}
		for _, o := range multi {
			for _, a := range pc[o] {
				ki := g.keyIndex(a.key)
				if ki < 0 || (g.ded[ki] >= 0 && vtClass(a.vt) == vtClass(uint8(g.ded[ki]))) {
					continue
				}
				var buf [4][]parquet.Value
				if c := vtClass(a.vt); c >= 0 && g.has[c] {
					for _, v := range a.vals {
						if g.preds[c] == nil || g.preds[c].KeepValue(v) {
							buf[c] = append(buf[c], v)
						}
					}
				}
				if s, ok := g.static(&buf); ok || !g.join {
					g.hits = append(g.hits, hit{o: o, av: attrVal{a: g.attr(a.key), s: s}})
				}
			}
		}
	}
	sort.SliceStable(g.hits, func(i, j int) bool { return g.hits[i].o < g.hits[j].o })
	return nil
}

func (g *genItem) keyIndex(key string) int {
	for i, k := range g.keys {
		if k.key == key {
			return i
		}
	}
	return -1
}

func (g *genItem) keepRun(ki int, en kmEnt, c int, v parquet.Value) bool {
	p := g.preds[c]
	if p == nil {
		return true
	}
	rd := g.keys[ki].rds[en.run]
	if c == tStr && rd.Vals.Dict {
		return g.runMemo(ki, int(en.run)).keep(rd.Vals.Codes[en.i], func() bool { return p.KeepValue(v) })
	}
	return p.KeepValue(v)
}

// runMemo is the string memo of run ri of key ki (runs are few; a small slice per key).
func (g *genItem) runMemo(ki, ri int) *strMemo {
	if g.runMemos == nil {
		g.runMemos = map[[2]int]*strMemo{}
	}
	m := g.runMemos[[2]int{ki, ri}]
	if m == nil {
		m = &strMemo{}
		g.runMemos[[2]int{ki, ri}] = m
	}
	return m
}

// static builds the value of an attribute from its passing elements (attributeCollector):
// one element is a scalar, several an array; none is nil (ok = false).
func (g *genItem) static(buf *[4][]parquet.Value) (traceql.Static, bool) {
	switch {
	case len(buf[tStr]) == 1:
		return traceql.NewStaticString(bstr(buf[tStr][0].ByteArray())), true
	case len(buf[tInt]) == 1:
		return traceql.NewStaticInt(int(buf[tInt][0].Int64())), true
	case len(buf[tFlt]) == 1:
		return traceql.NewStaticFloat(buf[tFlt][0].Double()), true
	case len(buf[tBool]) == 1:
		return traceql.NewStaticBool(buf[tBool][0].Boolean()), true
	case len(buf[tStr]) > 1:
		s := make([]string, len(buf[tStr]))
		for i, v := range buf[tStr] {
			s[i] = bstr(v.ByteArray())
		}
		return traceql.NewStaticStringArray(s), true
	case len(buf[tInt]) > 1:
		s := make([]int, len(buf[tInt]))
		for i, v := range buf[tInt] {
			s[i] = int(v.Int64())
		}
		return traceql.NewStaticIntArray(s), true
	case len(buf[tFlt]) > 1:
		s := make([]float64, len(buf[tFlt]))
		for i, v := range buf[tFlt] {
			s[i] = v.Double()
		}
		return traceql.NewStaticFloatArray(s), true
	case len(buf[tBool]) > 1:
		s := make([]bool, len(buf[tBool]))
		for i, v := range buf[tBool] {
			s[i] = v.Boolean()
		}
		return traceql.NewStaticBooleanArray(s), true
	}
	return traceql.Static{}, false
}

func (g *genItem) emitNull(*emitter) bool { return false }

func (g *genItem) emit(o uint32, e *emitter) bool {
	i := g.cur
	if i > len(g.hits) || (i > 0 && g.hits[i-1].o >= o) {
		i = sort.Search(len(g.hits), func(j int) bool { return g.hits[j].o >= o })
	}
	for i < len(g.hits) && g.hits[i].o < o {
		i++
	}
	m := false
	for ; i < len(g.hits) && g.hits[i].o == o; i++ {
		*e.attrs = append(*e.attrs, g.hits[i].av)
		m = true
	}
	g.cur = i
	return m
}

// genNilItem is `attr = nil` on a generic attribute: owners that have generic attributes but
// none with the key yield "nil" (vparquet5's nil iterator over the key column).
type genNilItem struct {
	level int
	key   string
	attr  traceql.Attribute
	state map[uint32]bool // owner -> has the key (only owners with generic attributes)
}

func (n *genNilItem) need(*pass, *loader) error { return nil }

func (n *genNilItem) prepare(p *pass) error {
	t := cg.OwnerTable[n.level]
	owners := make([]uint32, 0, p.w.n(t))
	for o := p.w.lo[t]; o < p.w.hi[t]; o++ {
		owners = append(owners, o)
	}
	pc, err := pointCopy(p.ctx, p.w, n.level, owners, false)
	if err != nil {
		return err
	}
	n.state = make(map[uint32]bool, len(pc))
	for o, as := range pc {
		has, any := false, false
		for _, a := range as {
			if dv, ok := p.plan.ded[n.level][a.key]; ok && vtClass(a.vt) == vtClass(dv) {
				continue
			}
			any = true
			has = has || a.key == n.key
		}
		if any {
			n.state[o] = has
		}
	}
	return nil
}

func (n *genNilItem) emitNull(*emitter) bool { return false }

func (n *genNilItem) emit(o uint32, e *emitter) bool {
	if has, ok := n.state[o]; ok && !has {
		e.add(n.attr, traceql.NewStaticString("nil"))
		return true
	}
	return false
}

// serviceStatsItem joins a trace's service_stats rows into the spanset's ServiceStats.
type serviceStatsItem struct {
	first          *cursor
	name, cnt, err *cursor
	hiTr, hiSS     uint32
}

func (s *serviceStatsItem) need(p *pass, ld *loader) error {
	ld.col(cg.TTraces, cg.TrFirstStat, p.rowsNext(cg.TTraces))
	for _, c := range []int{cg.SsServiceName, cg.SsSpanCount, cg.SsErrorCount} {
		ld.allRows(cg.TServiceStats, c)
	}
	return nil
}

func (s *serviceStatsItem) prepare(p *pass) (err error) {
	w := p.w
	s.hiTr, s.hiSS = w.hi[cg.TTraces], w.hi[cg.TServiceStats]
	if s.first, err = w.cursor(cg.TTraces, cg.TrFirstStat); err != nil {
		return err
	}
	if s.name, err = w.cursor(cg.TServiceStats, cg.SsServiceName); err != nil {
		return err
	}
	if s.cnt, err = w.cursor(cg.TServiceStats, cg.SsSpanCount); err != nil {
		return err
	}
	s.err, err = w.cursor(cg.TServiceStats, cg.SsErrorCount)
	return err
}

func (s *serviceStatsItem) emitNull(*emitter) bool { return false }

func (s *serviceStatsItem) emit(o uint32, e *emitter) bool {
	a, b := uint32(s.first.int(o)), s.hiSS
	if o+1 < s.hiTr {
		b = uint32(s.first.int(o + 1))
	}
	if a >= b {
		return false
	}
	if e.tr.stats == nil {
		e.tr.stats = make(map[string]traceql.ServiceStats, b-a)
	}
	for r := a; r < b; r++ {
		e.tr.stats[s.name.str(r)] = traceql.ServiceStats{SpanCount: uint32(s.cnt.int(r)), ErrorCount: uint32(s.err.int(r))}
	}
	return true
}

func dedupe(xs []uint32) []uint32 {
	out := xs[:0]
	for i, x := range xs {
		if i == 0 || x != xs[i-1] {
			out = append(out, x)
		}
	}
	return out
}

func spanIDHex(b []byte) string  { return util.SpanIDToHexString(b) }
func traceIDHex(b []byte) string { return util.TraceIDToHexString(b) }

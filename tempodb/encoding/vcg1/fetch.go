package vcg1

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/parquet-go/parquet-go"

	"github.com/grafana/tempo/v3/pkg/traceql"
	"github.com/grafana/tempo/v3/tempodb/encoding/common"
	cg "github.com/grafana/tempo/v3/tempodb/encoding/vcg1/cgfile"
)

// Fetch follows vparquet5's Fetch: a first pass over the conditions (one spanset per trace, in
// block order, spans in stored order), the engine's SecondPass on each spanset, and a second
// pass loading SecondPassConditions for the spans the engine kept. The requested range is
// processed in windows cut at footer anchors; each window's reads are batched per stage.
func (b *backendBlock) Fetch(ctx context.Context, req traceql.FetchSpansRequest, opts common.SearchOptions) (traceql.FetchSpansResponse, error) {
	if err := checkConditions(req.Conditions); err != nil {
		return traceql.FetchSpansResponse{}, fmt.Errorf("conditions invalid: %w", err)
	}
	coalesceConditions(&req)
	q, err := b.request(ctx)
	if err != nil {
		return traceql.FetchSpansResponse{}, err
	}
	a, e, err := b.anchorRange(ctx, q.F, opts)
	if err != nil {
		return traceql.FetchSpansResponse{}, err
	}
	f := &fetcher{ctx: ctx, q: q, req: req, wins: windows(q.F, a, e), live: map[cg.RunRef]struct{}{}}
	if f.p1, err = compile(q.F, req.Conditions, req.AllConditions, req.StartTimeUnixNanos, req.EndTimeUnixNanos, b.meta.DedicatedColumns, false, false); err != nil {
		return traceql.FetchSpansResponse{}, err
	}
	if req.SecondPass != nil {
		if f.p2, err = compile(q.F, req.SecondPassConditions, false, 0, 0, b.meta.DedicatedColumns, req.SecondPassSelectAll, false); err != nil {
			return traceql.FetchSpansResponse{}, err
		}
	}
	return traceql.FetchSpansResponse{
		Results: f,
		Stats:   func() traceql.FetchSpansStats { return traceql.FetchSpansStats{Bytes: uint64(q.Stats().Bytes)} },
	}, nil
}

type fetcher struct {
	ctx    context.Context
	q      *cg.Request
	req    traceql.FetchSpansRequest
	p1, p2 *plan
	wins   [][2]int
	wi     int
	w      *window
	pend   [][]*traceql.Spanset // per first-pass trace: the engine's spansets awaiting the second pass
	pi     int
	batch  int
	out    []*traceql.Spanset
	oi     int
	done   bool
	live   map[cg.RunRef]struct{} // decoded runs not yet dropped
}

// firstBatch is the first second-pass batch of a window, in traces; batches grow 4x, so a
// limited search stops after reading little.
const firstBatch = 32

var _ traceql.SpansetIterator = (*fetcher)(nil)

func (f *fetcher) Next(context.Context) (*traceql.Spanset, error) {
	for f.oi >= len(f.out) {
		f.out, f.oi = f.out[:0], 0
		if f.pi < len(f.pend) {
			if err := f.second(); err != nil {
				return nil, err
			}
			continue
		}
		if f.w != nil {
			f.release(f.w)
			f.w = nil
		}
		if f.done || f.wi >= len(f.wins) {
			return nil, nil
		}
		f.w = newWindow(f.q, f.wins[f.wi][0], f.wins[f.wi][1])
		f.wi++
		if err := f.first(); err != nil {
			return nil, err
		}
	}
	ss := f.out[f.oi]
	f.out[f.oi] = nil
	f.oi++
	return ss, nil
}

func (f *fetcher) Close() {}

// first runs the first pass on the current window and the engine's SecondPass on each trace.
func (f *fetcher) first() error {
	p := &pass{plan: f.p1, ctx: f.ctx, w: f.w}
	sets, err := p.first()
	if err != nil {
		return err
	}
	if f.p2 == nil {
		f.out = append(f.out, sets...)
		return nil
	}
	f.pend, f.pi, f.batch = f.pend[:0], 0, firstBatch
	for _, ss := range sets {
		res, err := f.req.SecondPass(ss)
		if errors.Is(err, io.EOF) {
			f.done = true
			break
		}
		if err != nil {
			return err
		}
		if len(res) == 0 {
			putSpansetAndSpans(ss)
			continue
		}
		f.pend = append(f.pend, res)
	}
	return nil
}

// second runs the second pass for the next batch of pending traces.
func (f *fetcher) second() error {
	n := min(f.batch, len(f.pend)-f.pi)
	f.batch *= 4
	var drv []*span
	for _, res := range f.pend[f.pi : f.pi+n] {
		for _, ss := range res {
			for i, s := range ss.Spans {
				sp := s.(*span)
				sp.cbSpansetFinal = i == len(ss.Spans)-1
				sp.cbSpanset = ss
				drv = append(drv, sp)
			}
		}
	}
	clear(f.pend[f.pi : f.pi+n])
	f.pi += n
	sort.SliceStable(drv, func(i, j int) bool { return drv[i].row < drv[j].row })
	k := 0
	for i, sp := range drv {
		if i == 0 || sp != drv[k-1] {
			drv[k] = sp
			k++
		}
	}
	p2 := &pass{plan: f.p2, ctx: f.ctx, w: f.w, drv: drv[:k]}
	out, err := p2.second()
	if err != nil {
		return err
	}
	f.out = append(f.out, out...)
	return nil
}

// release drops what no later window needs: decoded pages ending in this window and runs of
// flushes this window finished.
func (f *fetcher) release(w *window) {
	for t := 0; t < cg.NumTables; t++ {
		f.q.DropPagesBefore(t, w.hi[t])
	}
	for _, p := range []*plan{f.p1, f.p2} {
		if p == nil {
			continue
		}
		for _, k := range p.keys {
			for _, ref := range k.refs {
				f.live[ref] = struct{}{}
			}
		}
	}
	for ref := range f.live {
		km, err := f.q.Km(ref.Level)
		if err != nil {
			continue
		}
		fl := km[ref.Flush]
		if fl.Owner0+fl.Owners <= w.hi[cg.OwnerTable[ref.Level]] {
			f.q.DropRun(ref)
			delete(f.live, ref)
		}
	}
}

// pass is a plan bound to one window (and, for the second pass, to the engine's spans).
type pass struct {
	*plan
	ctx context.Context
	w   *window
	drv []*span // second pass: the spans to fill, ascending rows

	trFirst, resFirst, scFirst, evFirst, lnFirst []uint32

	// levels mapped to spans through their scan-copy runs' links instead of offset columns
	linked              [nLv]bool
	resOfSpan, scOfSpan []uint32 // local owner per local span, or none
	evOff, evList       []uint32 // events with entries per local span (CSR)
	noneRes, noneSc     upper

	act  [nLv]bool
	none bool       // provably no span of the window matches: nothing was read
	em   [4]emitter // reused (an emitter passed to an item escapes)
}

const (
	emGroup = iota
	emTrace
	emSpan
	emKid
)

func (p *pass) emitter(k int, attrs *[]attrVal, sp *span, tr *traceOut) *emitter {
	e := &p.em[k]
	e.attrs, e.sp, e.tr = attrs, sp, tr
	return e
}

const none = ^uint32(0)

// linkable reports whether level l can be mapped to spans by its runs' links alone: every
// iterator reads scan-copy runs and (for events) yields nothing for an owner without entries.
func (p *pass) linkable(l int) bool {
	lv := p.lv[l]
	if l != lvRes && l != lvScope && l != lvEvent {
		return false
	}
	if lv.gen == nil && len(lv.items) == 0 {
		return false
	}
	for _, it := range lv.items {
		d, ok := it.(*dedItem)
		if !ok || (l == lvEvent && (d.nilOp || d.pred == nil || d.pred.KeepValue(parquet.Value{}))) {
			return false
		}
	}
	return true
}

func (p *pass) levelKeys(l int) []*kmKey {
	lv := p.lv[l]
	var ks []*kmKey
	if lv.gen != nil {
		ks = append(ks, lv.gen.keys...)
	}
	for _, it := range lv.items {
		if d, ok := it.(*dedItem); ok {
			ks = append(ks, d.k)
		}
	}
	return ks
}

// link builds level l's span mapping from the links of its keys' run entries.
func (p *pass) link(l int) {
	w := p.w
	ns := w.n(cg.TSpans)
	t := cg.OwnerTable[cgLevel[l]]
	if l == lvEvent {
		type pair struct{ s, e uint32 }
		var ps []pair
		for _, k := range p.levelKeys(l) {
			for _, en := range k.ents {
				ps = append(ps, pair{k.rds[en.run].Links[0][en.i] - w.lo[cg.TSpans], en.o - w.lo[t]})
			}
		}
		sort.Slice(ps, func(i, j int) bool { return ps[i].s < ps[j].s || (ps[i].s == ps[j].s && ps[i].e < ps[j].e) })
		p.evOff = make([]uint32, ns+1)
		p.evList = p.evList[:0]
		for i, x := range ps {
			if i > 0 && x == ps[i-1] {
				continue
			}
			p.evOff[x.s+1]++
			p.evList = append(p.evList, x.e)
		}
		for i := 1; i < len(p.evOff); i++ {
			p.evOff[i] += p.evOff[i-1]
		}
		return
	}
	of := make([]uint32, ns)
	for i := range of {
		of[i] = none
	}
	for _, k := range p.levelKeys(l) {
		for _, en := range k.ents {
			rd := k.rds[en.run]
			s0 := rd.Links[0][en.i] - w.lo[cg.TSpans]
			for s := s0; s < s0+rd.Links[1][en.i]; s++ {
				of[s] = en.o - w.lo[t]
			}
		}
	}
	if l == lvRes {
		p.resOfSpan = of
	} else {
		p.scOfSpan = of
	}
}

// owner is the local resource or scope of local span s.
func (p *pass) owner(l int, s uint32, po *parentOf) uint32 {
	if p.linked[l] {
		if l == lvRes {
			return p.resOfSpan[s]
		}
		return p.scOfSpan[s]
	}
	return po.of(s)
}

func (p *pass) rows(t int) []cg.Interval {
	if p.drv != nil {
		switch t {
		case cg.TSpans:
			rows := make([]uint32, len(p.drv))
			for i, sp := range p.drv {
				rows[i] = p.w.lo[t] + sp.row
			}
			return rowIvs(rows)
		case cg.TTraces:
			return rowIvs(p.drvTraces(0))
		}
	}
	return p.w.all(t)
}

// rowsNext is rows plus each row's successor in the window (for child range ends).
func (p *pass) rowsNext(t int) []cg.Interval {
	if p.drv != nil && t == cg.TTraces {
		return rowIvs(p.drvTraces(1))
	}
	return p.w.all(t)
}

func (p *pass) drvTraces(next uint32) []uint32 {
	var rows []uint32
	for _, sp := range p.drv {
		r := p.w.lo[cg.TTraces] + sp.trace
		for k := uint32(0); k <= next && r+k < p.w.hi[cg.TTraces]; k++ {
			if n := len(rows); n == 0 || rows[n-1] < r+k {
				rows = append(rows, r+k)
			}
		}
	}
	return rows
}

func (p *pass) active(l int) bool { return p.act[l] }

func (p *pass) isActive(l int) bool {
	lv := p.lv[l]
	switch l {
	case lvEvent, lvLink:
		return lv.n > 0
	case lvTrace:
		return lv.gen != nil || len(lv.items) > 0 || p.timeStart != nil
	}
	return lv.gen != nil || len(lv.items) > 0
}

// load reads the pass's data for the window: every item's rows and runs and the parent
// offsets the levels map through, in one wave (plus point-copy waves for owners with several
// entries of a key).
func (p *pass) load() error {
	ld := newLoader(p.w)
	if err := p.need(ld); err != nil {
		return err
	}
	if p.empty() {
		p.none = true
		return nil
	}
	if err := ld.exec(p.ctx); err != nil {
		return err
	}
	return p.prepare()
}

func (p *pass) need(ld *loader) error {
	for l := range p.lv {
		p.act[l] = p.isActive(l)
	}
	for l := range p.lv {
		if !p.active(l) {
			continue
		}
		lv := p.lv[l]
		if lv.gen != nil {
			if err := lv.gen.need(p, ld); err != nil {
				return err
			}
		}
		for _, it := range lv.items {
			if err := it.need(p, ld); err != nil {
				return err
			}
		}
	}
	if p.timeStart != nil {
		_ = p.timeStart.need(p, ld)
		_ = p.timeEnd.need(p, ld)
	}
	if p.drv == nil && (!p.spanOnly || p.active(lvTrace)) {
		ld.allRows(cg.TTraces, cg.TrFirstSpan)
	}
	for _, l := range []int{lvRes, lvScope, lvEvent} {
		p.linked[l] = p.active(l) && p.linkable(l)
	}
	if p.active(lvRes) && !p.linked[lvRes] {
		ld.allRows(cg.TResources, cg.ResFirstSpan)
	}
	if p.active(lvScope) && !p.linked[lvScope] {
		ld.allRows(cg.TScopes, cg.ScFirstSpan)
	}
	if p.active(lvEvent) && !p.linked[lvEvent] {
		ld.allRows(cg.TSpans, cg.SpFirstEvent)
	}
	if p.active(lvLink) {
		ld.allRows(cg.TSpans, cg.SpFirstLink)
	}
	return nil
}

func (p *pass) prepare() error {
	w := p.w
	for _, k := range p.keys {
		k.prepare(w)
	}
	for l := range p.lv {
		if !p.active(l) {
			continue
		}
		lv := p.lv[l]
		if lv.gen != nil {
			if err := lv.gen.prepare(p); err != nil {
				return err
			}
		}
		for _, it := range lv.items {
			if err := it.prepare(p); err != nil {
				return err
			}
		}
	}
	if p.timeStart != nil {
		if err := p.timeStart.prepare(p); err != nil {
			return err
		}
		if err := p.timeEnd.prepare(p); err != nil {
			return err
		}
	}
	var err error
	if p.drv == nil && (!p.spanOnly || p.active(lvTrace)) {
		if p.trFirst, err = w.firsts(cg.TTraces, cg.TrFirstSpan, cg.TSpans); err != nil {
			return err
		}
	}
	if p.linked[lvRes] {
		p.link(lvRes)
	} else if p.active(lvRes) {
		if p.resFirst, err = w.firsts(cg.TResources, cg.ResFirstSpan, cg.TSpans); err != nil {
			return err
		}
	}
	if p.linked[lvScope] {
		p.link(lvScope)
	} else if p.active(lvScope) {
		if p.scFirst, err = w.firsts(cg.TScopes, cg.ScFirstSpan, cg.TSpans); err != nil {
			return err
		}
	}
	if p.linked[lvEvent] {
		p.link(lvEvent)
	} else if p.active(lvEvent) {
		if p.evFirst, err = w.firsts(cg.TSpans, cg.SpFirstEvent, cg.TEvents); err != nil {
			return err
		}
	}
	if p.active(lvLink) {
		if p.lnFirst, err = w.firsts(cg.TSpans, cg.SpFirstLink, cg.TLinks); err != nil {
			return err
		}
	}
	return nil
}

// emit runs a level's iterators for owner o: under AllConditions every one must yield (else
// the owner is dropped and its values discarded), otherwise each yields what it has.
// empty reports, from the footer alone, that under AllConditions some required iterator has
// no scan-copy run to match in the window.
func (p *pass) empty() bool {
	if !p.all {
		return false
	}
	for l := range p.lv {
		if !p.act[l] {
			continue
		}
		lv := p.lv[l]
		if g := lv.gen; g != nil && g.join && !anyRuns(g.keys) {
			return true
		}
		for _, it := range lv.items {
			if d, ok := it.(*dedItem); ok && !d.nilOp && d.pred != nil && !d.pred.KeepValue(parquet.Value{}) && len(d.k.refs) == 0 {
				return true
			}
		}
	}
	return false
}

func anyRuns(ks []*kmKey) bool {
	for _, k := range ks {
		if len(k.refs) > 0 {
			return true
		}
	}
	return false
}

func (p *pass) emit(l int, o uint32, e *emitter) bool {
	lv := p.lv[l]
	start := len(*e.attrs)
	if lv.gen != nil && !lv.gen.emit(o, e) && p.all {
		*e.attrs = (*e.attrs)[:start]
		return false
	}
	for i, it := range lv.items {
		if !it.emit(o, e) && p.all && !lv.opt[i] {
			*e.attrs = (*e.attrs)[:start]
			return false
		}
	}
	return true
}

// upper is the memoized outcome of a scope, resource or trace for the spans under it.
type upper struct {
	idx   uint32
	set   bool
	keep  bool
	attrs []attrVal
	tr    traceOut
}

func (u *upper) at(i uint32) bool {
	if u.set && u.idx == i {
		return true
	}
	u.set, u.idx, u.keep, u.attrs = true, i, false, u.attrs[:0]
	u.tr = traceOut{}
	return false
}

// group evaluates scope or resource l for local owner i (collector minimums included).
func (p *pass) group(l int, u *upper, i uint32) bool {
	if i == none && !(u.set && u.idx == none) {
		n := &p.noneRes
		if l == lvScope {
			n = &p.noneSc
		}
		if !n.set {
			p.groupAt(l, n, i)
		}
		u.set, u.idx, u.keep, u.attrs = true, none, n.keep, append(u.attrs[:0], n.attrs...)
		return u.keep
	}
	return p.groupAt(l, u, i)
}

func (p *pass) groupAt(l int, u *upper, i uint32) bool {
	if u.at(i) {
		return u.keep
	}
	e := p.emitter(emGroup, &u.attrs, nil, nil)
	t := cg.OwnerTable[cgLevel[l]]
	o := p.w.hi[t] // an owner without entries
	if i != none {
		o = p.w.lo[t] + i
	}
	u.keep = p.emit(l, o, e) && (!p.all || p.spanOnly || len(u.attrs) >= p.lv[l].min)
	return u.keep
}

func (p *pass) trace(u *upper, i uint32) bool {
	if u.at(i) {
		return u.keep
	}
	o := p.w.lo[cg.TTraces] + i
	e := p.emitter(emTrace, &u.tr.attrs, nil, &u.tr)
	u.keep = true
	if p.timeStart != nil {
		u.keep = p.timeStart.emit(o, e) && p.timeEnd.emit(o, e)
	}
	u.keep = u.keep && p.emit(lvTrace, o, e)
	return u.keep
}

// spanLevel fills span s (window-local) from the span, event and link iterators and reports
// whether it is kept (spanCollector).
func (p *pass) spanLevel(s uint32, sp *span) bool {
	lv := p.lv[lvSpan]
	o := p.w.lo[cg.TSpans] + s
	e := p.emitter(emSpan, &sp.spanAttrs, sp, nil)
	if lv.gen != nil && !lv.gen.emit(o, e) && p.all {
		return false
	}
	if p.active(lvEvent) && !p.children(lvEvent, p.evFirst, s, &sp.eventAttrs) && p.all {
		return false
	}
	if p.active(lvLink) && !p.children(lvLink, p.lnFirst, s, &sp.linkAttrs) && p.all {
		return false
	}
	for i, it := range lv.items {
		if !it.emit(o, e) && p.all && !lv.opt[i] {
			return false
		}
	}
	if p.spanOnly {
		return true
	}
	return !p.all || lv.min == 0 || sp.attributesMatched() >= lv.min
}

// children runs the event or link iterator over span s's events/links, appending the values
// of those kept (eventCollector/linkCollector). A span without any joins as one empty row.
func (p *pass) children(l int, first []uint32, s uint32, dst *[]attrVal) bool {
	t := cg.OwnerTable[cgLevel[l]]
	var kids kids
	if p.linked[l] {
		kids.list = p.evList[p.evOff[s]:p.evOff[s+1]]
	} else {
		kids.a, kids.b = first[s], first[s+1]
	}
	if kids.n() == 0 {
		return p.nullChild(l, dst)
	}
	if p.spanOnly {
		return p.childrenSpanOnly(l, t, kids, dst)
	}
	kept := false
	for i := 0; i < kids.n(); i++ {
		start := len(*dst)
		e := p.emitter(emKid, dst, nil, nil)
		if !p.emit(l, p.w.lo[t]+kids.at(i), e) {
			continue
		}
		if p.all && len(*dst)-start < p.lv[l].min {
			*dst = (*dst)[:start]
			continue
		}
		kept = true
	}
	return kept || !p.all
}

// kids are a span's events or links: a row range, or the rows listed by run links.
type kids struct {
	a, b uint32
	list []uint32
}

func (k *kids) n() int {
	if k.list != nil {
		return len(k.list)
	}
	return int(k.b - k.a)
}

func (k *kids) at(i int) uint32 {
	if k.list != nil {
		return k.list[i]
	}
	return k.a + uint32(i)
}

// nullChild is the one null event/link row a span without any joins with: only iterators
// whose predicate keeps nulls yield (vparquet5 reads them as "" or "nil").
func (p *pass) nullChild(l int, dst *[]attrVal) bool {
	lv := p.lv[l]
	start := len(*dst)
	e := p.emitter(emKid, dst, nil, nil)
	if lv.gen != nil && p.all {
		return false
	}
	for i, it := range lv.items {
		if !it.emitNull(e) && p.all && !lv.opt[i] {
			*dst = (*dst)[:start]
			return false
		}
	}
	if p.all && !p.spanOnly && len(*dst)-start < lv.min {
		*dst = (*dst)[:start]
		return false
	}
	return true
}

// childrenSpanOnly is the span-level join of FetchSpans: each event/link iterator yields the
// values of every row of the span it matches, and a required one must match some row.
func (p *pass) childrenSpanOnly(l, t int, kids kids, dst *[]attrVal) bool {
	lv := p.lv[l]
	e := p.emitter(emKid, dst, nil, nil)
	if lv.gen != nil {
		m := false
		for i := 0; i < kids.n(); i++ {
			m = lv.gen.emit(p.w.lo[t]+kids.at(i), e) || m
		}
		if !m && p.all {
			return false
		}
	}
	for i, it := range lv.items {
		m := false
		for j := 0; j < kids.n(); j++ {
			m = it.emit(p.w.lo[t]+kids.at(j), e) || m
		}
		if !m && p.all && !lv.opt[i] {
			return false
		}
	}
	return true
}

// first runs the first pass over every span of the window and returns one spanset per trace.
func (p *pass) first() ([]*traceql.Spanset, error) {
	if err := p.load(); err != nil || p.none {
		return nil, err
	}
	var (
		out               []*traceql.Spanset
		cur               *traceql.Spanset
		curTr             uint32
		tu, ru, su        upper
		trOf, resOf, scOf = parentOf{first: p.trFirst}, parentOf{first: p.resFirst}, parentOf{first: p.scFirst}
		sp                *span
	)
	flush := func() {
		if cur != nil && len(cur.Spans) > 0 {
			out = append(out, cur)
		}
		cur = nil
	}
	for s := uint32(0); s < p.w.n(cg.TSpans); s++ {
		t := trOf.of(s)
		if !p.trace(&tu, t) {
			continue
		}
		var res, sc uint32
		if p.active(lvRes) {
			if res = p.owner(lvRes, s, &resOf); !p.group(lvRes, &ru, res) {
				continue
			}
		}
		if p.active(lvScope) {
			if sc = p.owner(lvScope, s, &scOf); !p.group(lvScope, &su, sc) {
				continue
			}
		}
		if sp == nil {
			sp = getSpan()
		}
		sp.row, sp.trace = s, t
		if !p.spanLevel(s, sp) {
			resetSpan(sp)
			continue
		}
		if p.active(lvScope) {
			sp.setInstrumentationAttrs(su.attrs)
		}
		if p.active(lvRes) {
			sp.setResourceAttrs(ru.attrs)
		}
		if p.requireOne && sp.attributesMatched() == 0 {
			resetSpan(sp)
			continue
		}
		sp.setTraceAttrs(tu.tr.attrs)
		if cur == nil || curTr != t {
			flush()
			cur, curTr = getSpanset(), t
			tr := &tu.tr
			cur.TraceID, cur.StartTimeUnixNanos, cur.DurationNanos = tr.id, tr.start, tr.dur
			cur.RootSpanName, cur.RootServiceName = tr.rootSpan, tr.rootService
			if len(tr.stats) > 0 {
				cur.ServiceStats = tr.stats
			}
		}
		cur.Spans = append(cur.Spans, sp)
		sp = nil
	}
	flush()
	if sp != nil {
		putSpan(sp)
	}
	return out, nil
}

// resetSpan clears a span for reuse without returning it to the pool.
func resetSpan(sp *span) {
	row, tr := sp.row, sp.trace
	sp.id, sp.startTimeUnixNanos, sp.durationNanos = nil, 0, 0
	sp.nestedSetParent, sp.nestedSetLeft, sp.nestedSetRight = 0, 0, 0
	sp.spanAttrs, sp.resourceAttrs, sp.traceAttrs = sp.spanAttrs[:0], sp.resourceAttrs[:0], sp.traceAttrs[:0]
	sp.eventAttrs, sp.linkAttrs, sp.instrumentationAttrs = sp.eventAttrs[:0], sp.linkAttrs[:0], sp.instrumentationAttrs[:0]
	sp.row, sp.trace = row, tr
}

// second fills the engine's spans with the second-pass data and returns the engine's
// spansets in the order vparquet5's rebatching emits them (by their last span).
func (p *pass) second() ([]*traceql.Spanset, error) {
	needs := false
	for l := range p.lv {
		needs = needs || p.isActive(l)
	}
	if needs || p.requireOne {
		if err := p.load(); err != nil {
			return nil, err
		}
	}
	var (
		tu, ru, su  upper
		resOf, scOf = parentOf{first: p.resFirst}, parentOf{first: p.scFirst}
		out         []*traceql.Spanset
	)
	for _, sp := range p.drv {
		s := sp.row
		p.trace(&tu, sp.trace)
		if p.active(lvRes) {
			p.group(lvRes, &ru, p.owner(lvRes, s, &resOf))
		}
		if p.active(lvScope) {
			p.group(lvScope, &su, p.owner(lvScope, s, &scOf))
		}
		p.spanLevel(s, sp)
		if p.active(lvScope) {
			sp.setInstrumentationAttrs(su.attrs)
		}
		if p.active(lvRes) {
			sp.setResourceAttrs(ru.attrs)
		}
		if p.requireOne && sp.attributesMatched() == 0 {
			continue
		}
		sp.setTraceAttrs(tu.tr.attrs)
		if !sp.cbSpansetFinal || sp.cbSpanset == nil {
			continue
		}
		ss, tr := sp.cbSpanset, &tu.tr
		if ss.DurationNanos == 0 {
			ss.DurationNanos = tr.dur
		}
		if len(ss.TraceID) == 0 {
			ss.TraceID = tr.id
		}
		if len(ss.RootSpanName) == 0 {
			ss.RootSpanName = tr.rootSpan
		}
		if len(ss.RootServiceName) == 0 {
			ss.RootServiceName = tr.rootService
		}
		if ss.StartTimeUnixNanos == 0 {
			ss.StartTimeUnixNanos = tr.start
		}
		if len(ss.ServiceStats) == 0 {
			if ss.ServiceStats == nil {
				ss.ServiceStats = map[string]traceql.ServiceStats{}
			}
			for k, v := range tr.stats {
				ss.ServiceStats[k] = v
			}
		}
		out = append(out, ss)
	}
	return out, nil
}

package vcg1

import (
	"context"
	"errors"
	"io"

	"github.com/grafana/tempo/v3/pkg/traceql"
	"github.com/grafana/tempo/v3/tempodb/encoding/common"
	cg "github.com/grafana/tempo/v3/tempodb/encoding/vcg1/cgfile"
)

// FetchSpans follows vparquet5's span-only fetch (block_traceql_fetch.go): every level joined
// at span level, the engine's SecondPass called on a one-span spanset, the second-pass values
// added to the same span, and one reused span returned per Next.
func (b *backendBlock) FetchSpans(ctx context.Context, req traceql.FetchSpansRequest, opts common.SearchOptions) (traceql.FetchSpansOnlyResponse, error) {
	q, err := b.request(ctx, opts)
	if err != nil {
		return traceql.FetchSpansOnlyResponse{}, err
	}
	a, e, err := b.anchorRange(ctx, q.F, opts)
	if err != nil {
		return traceql.FetchSpansOnlyResponse{}, err
	}
	f := &spanFetcher{fetcher: fetcher{ctx: ctx, q: q, req: req, wins: windows(q.F, a, e), live: map[cg.RunRef]struct{}{}}}
	if f.p1, err = compile(q.F, req.Conditions, req.AllConditions, req.TraceStartTimeUnixNanos, req.TraceEndTimeUnixNanos, b.meta.DedicatedColumns, false, true); err != nil {
		return traceql.FetchSpansOnlyResponse{}, err
	}
	if len(req.SecondPassConditions) > 0 || req.SecondPassSelectAll {
		if f.p2, err = compile(q.F, req.SecondPassConditions, false, 0, 0, b.meta.DedicatedColumns, req.SecondPassSelectAll, true); err != nil {
			return traceql.FetchSpansOnlyResponse{}, err
		}
	}
	f.buf.Spans = []traceql.Span{&f.sp}
	return traceql.FetchSpansOnlyResponse{
		Results: f,
		Stats:   func() traceql.FetchSpansStats { return traceql.FetchSpansStats{Bytes: uint64(q.Stats().Bytes)} },
	}, nil
}

type spanFetcher struct {
	fetcher
	pp1, pp2 *pass
	s, n     uint32
	runs     []cg.Interval // window-local spans to evaluate (nil: every span of the window)
	ri       int
	sp       span
	buf      traceql.Spanset

	tu, ru, su    upper // first pass
	tu2, ru2, su2 upper // second pass
	trOf, resOf   parentOf
	scOf          parentOf
	trOf2, resOf2 parentOf
	scOf2         parentOf
}

var _ traceql.SpanIterator = (*spanFetcher)(nil)

func (f *spanFetcher) Next(context.Context) (traceql.Span, error) {
	for {
		if f.w == nil || f.s >= f.n {
			if f.w != nil {
				f.release(f.w)
				f.w = nil
			}
			if f.done || f.wi >= len(f.wins) {
				return nil, nil
			}
			if err := f.open(); err != nil {
				return nil, err
			}
			continue
		}
		if f.runs != nil {
			for f.ri < len(f.runs) && f.s >= f.runs[f.ri].Hi {
				f.ri++
			}
			if f.ri == len(f.runs) {
				f.s = f.n
				continue
			}
			f.s = max(f.s, f.runs[f.ri].Lo)
		}
		s := f.s
		f.s++
		ok, err := f.span(s)
		if err != nil {
			return nil, err
		}
		if ok {
			return &f.sp, nil
		}
		if f.done {
			return nil, nil
		}
	}
}

// open starts the next window: both passes' data for the whole window in one wave. Span
// columns the first pass only emits (deferred) are left out of it: once the other data has
// picked the candidate spans, they are read in a second wave at those spans only.
func (f *spanFetcher) open() error {
	w, err := newWindow(f.q, f.wins[f.wi][0], f.wins[f.wi][1])
	if err != nil {
		return err
	}
	f.w = w
	f.wi++
	f.s, f.n, f.runs, f.ri = 0, f.w.n(cg.TSpans), nil, 0
	f.pp1 = &pass{plan: f.p1, ctx: f.ctx, w: f.w}
	lvs := f.pp1.lv[lvSpan]
	def := f.pp1.deferrable()
	saved := make([]item, len(def))
	for k, i := range def {
		saved[k], lvs.items[i] = lvs.items[i], passItem{}
	}
	restored := false
	restore := func() {
		if !restored {
			for k, i := range def {
				lvs.items[i] = saved[k]
			}
			restored = true
		}
	}
	defer restore()
	ld := newLoader(f.w)
	if err := f.pp1.need(ld); err != nil {
		return err
	}
	f.pp2 = nil
	if f.p2 != nil {
		f.pp2 = &pass{plan: f.p2, ctx: f.ctx, w: f.w}
		if err := f.pp2.need(ld); err != nil {
			return err
		}
	}
	if f.pp1.empty() {
		f.n = 0
		return nil
	}
	if err := ld.exec(f.ctx); err != nil {
		return err
	}
	if err := f.pp1.prepare(); err != nil {
		return err
	}
	f.tu, f.ru, f.su = upper{}, upper{}, upper{}
	f.trOf, f.resOf, f.scOf = parentOf{first: f.pp1.trFirst}, parentOf{first: f.pp1.resFirst}, parentOf{first: f.pp1.scFirst}
	if f.pp2 != nil {
		if err := f.pp2.prepare(); err != nil {
			return err
		}
		f.tu2, f.ru2, f.su2 = upper{}, upper{}, upper{}
		f.trOf2, f.resOf2, f.scOf2 = parentOf{first: f.pp2.trFirst}, parentOf{first: f.pp2.resFirst}, parentOf{first: f.pp2.scFirst}
	}
	// with the deferred items stubbed and nothing else at span level that can reject, the
	// candidates are the narrowed spans themselves: no span needs evaluating twice
	trivial := len(def) > 0 && !lvs.rejects() && !f.pp1.active(lvEvent) && !f.pp1.active(lvLink)
	runs, narrowed, exact := f.pp1.spanRuns(trivial)
	if narrowed && len(runs) == 0 {
		f.n = 0
		return nil
	}
	if len(def) == 0 {
		f.runs = runs
		return nil
	}
	var cruns []cg.Interval
	if trivial && exact {
		cruns = runs
		if !narrowed {
			cruns = []cg.Interval{{Lo: 0, Hi: f.n}}
		}
	} else {
		cruns = rowIvs(f.candidates(runs, narrowed))
	}
	restore()
	if len(cruns) == 0 {
		f.n = 0
		return nil
	}
	ivs := make([]cg.Interval, len(cruns))
	for i, r := range cruns {
		ivs[i] = cg.Interval{Lo: f.w.lo[cg.TSpans] + r.Lo, Hi: f.w.lo[cg.TSpans] + r.Hi}
	}
	ld = newLoader(f.w)
	for _, it := range saved {
		c := it.(*colItem)
		ld.col(c.t, c.c, ivs)
	}
	if err := ld.exec(f.ctx); err != nil {
		return err
	}
	for _, it := range saved {
		if err := it.prepare(f.pp1); err != nil {
			return err
		}
	}
	f.tu, f.ru, f.su = upper{}, upper{}, upper{}
	f.trOf, f.resOf, f.scOf = parentOf{first: f.pp1.trFirst}, parentOf{first: f.pp1.resFirst}, parentOf{first: f.pp1.scFirst}
	f.runs = cruns
	return nil
}

// candidates are the window's spans the first pass keeps with its deferred columns stubbed:
// a superset of the spans it keeps (the collector minimum, which the deferred columns count
// towards, is not checked), exact otherwise, since under AllConditions a column without a
// predicate cannot reject a span.
func (f *spanFetcher) candidates(runs []cg.Interval, narrowed bool) []uint32 {
	f.pp1.noMin = true
	defer func() { f.pp1.noMin = false }()
	if !narrowed {
		runs = []cg.Interval{{Lo: 0, Hi: f.n}}
	}
	var out []uint32
	for _, r := range runs {
		for s := r.Lo; s < r.Hi; s++ {
			resetSpan(&f.sp)
			f.sp.row = s
			if f.level(f.pp1, s, &f.tu, &f.ru, &f.su, &f.trOf, &f.resOf, &f.scOf) {
				out = append(out, s)
			}
		}
	}
	return out
}

// passItem stands in for a deferred item while candidates are picked: it yields nothing and
// rejects nothing.
type passItem struct{}

func (passItem) need(*pass, *loader) error  { return nil }
func (passItem) prepare(*pass) error        { return nil }
func (passItem) emit(uint32, *emitter) bool { return true }
func (passItem) emitNull(*emitter) bool     { return true }

// deferrable returns the span-level items of the pass read only for their values (span
// columns without a predicate), when the pass requires all its conditions and something else
// filters: those columns can then wait until the candidate spans are known.
func (p *pass) deferrable() []int {
	if !p.all {
		return nil
	}
	// the trace time range keeps nearly every span: not a filter worth a second wave
	filter := false
	var out []int
	for l, lv := range p.lv {
		if l != lvSpan {
			filter = filter || (p.isActive(l) && lv.rejects())
			continue
		}
		filter = filter || lv.gen != nil
		for i, it := range lv.items {
			if c, ok := it.(*colItem); ok && c.t == cg.TSpans && c.pred == nil && c.c != cg.SpParentSpanID && !lv.opt[i] {
				out = append(out, i)
				continue
			}
			filter = filter || (!lv.opt[i] && rejects(it))
		}
	}
	if !filter {
		return nil
	}
	return out
}

// span evaluates window-local span s through both passes into the reused span.
func (f *spanFetcher) span(s uint32) (bool, error) {
	sp := &f.sp
	resetSpan(sp)
	sp.row = s
	if !f.level(f.pp1, s, &f.tu, &f.ru, &f.su, &f.trOf, &f.resOf, &f.scOf) {
		return false, nil
	}
	if f.req.SecondPass != nil {
		f.buf.Spans = f.buf.Spans[:1]
		f.buf.Spans[0] = sp
		f.buf.StartTimeUnixNanos = f.tu.tr.start
		f.buf.ServiceStats = f.tu.tr.stats
		out, err := f.req.SecondPass(&f.buf)
		if errors.Is(err, io.EOF) {
			f.done = true
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if len(out) == 0 {
			return false, nil
		}
	}
	if f.pp2 != nil && !f.level(f.pp2, s, &f.tu2, &f.ru2, &f.su2, &f.trOf2, &f.resOf2, &f.scOf2) {
		return false, nil
	}
	return true, nil
}

func (f *spanFetcher) level(p *pass, s uint32, tu, ru, su *upper, trOf, resOf, scOf *parentOf) bool {
	sp := &f.sp
	if p.active(lvTrace) {
		if !p.trace(tu, trOf.of(s)) {
			return false
		}
	}
	if p.active(lvRes) && !p.group(lvRes, ru, p.owner(lvRes, s, resOf)) {
		return false
	}
	if p.active(lvScope) && !p.group(lvScope, su, p.owner(lvScope, s, scOf)) {
		return false
	}
	if !p.spanLevel(s, sp) {
		return false
	}
	if p.active(lvTrace) {
		sp.setTraceAttrs(tu.tr.attrs)
	}
	if p.active(lvRes) {
		sp.setResourceAttrs(ru.attrs)
	}
	if p.active(lvScope) {
		sp.setInstrumentationAttrs(su.attrs)
	}
	return p.noMin || p.spanMin == 0 || sp.hasAtLeast(p.spanMin)
}

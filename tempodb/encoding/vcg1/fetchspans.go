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
	q, err := b.request(ctx)
	if err != nil {
		return traceql.FetchSpansOnlyResponse{}, err
	}
	a, e, err := b.anchorRange(ctx, q.F, opts)
	if err != nil {
		return traceql.FetchSpansOnlyResponse{}, err
	}
	f := &spanFetcher{fetcher: fetcher{ctx: ctx, q: q, req: req, wins: windows(q.F, a, e), live: map[cg.RunRef]struct{}{}}}
	if f.p1, err = compile(q.F, req.Conditions, req.AllConditions, req.StartTimeUnixNanos, req.EndTimeUnixNanos, b.meta.DedicatedColumns, false, true); err != nil {
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

// open starts the next window: both passes' data for the whole window in one wave.
func (f *spanFetcher) open() error {
	f.w = newWindow(f.q, f.wins[f.wi][0], f.wins[f.wi][1])
	f.wi++
	f.s, f.n = 0, f.w.n(cg.TSpans)
	f.pp1 = &pass{plan: f.p1, ctx: f.ctx, w: f.w}
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
	return nil
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
	return p.spanMin == 0 || sp.hasAtLeast(p.spanMin)
}

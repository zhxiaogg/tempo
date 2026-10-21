package cgfile

import (
	"context"
	"fmt"
)

// Helpers for tag search: the distinct values of a column over a row range, read from chunk
// dictionaries where possible, and the values of key-major runs read without their owner and
// link buffers.

// runHeaderProbe is how many leading bytes of a run are read to parse its header (a header is
// a few dozen bytes; a longer one is read in a second wave).
const runHeaderProbe = 128

// ColumnSource is one source of a column's values in a row range: a chunk dictionary (every
// value of a chunk whose dictionary-coded pages all lie in the range), or rows [Lo, Hi) of a
// decoded page (Dict resolves its codes).
type ColumnSource struct {
	Entries [][]byte
	Page    *Values
	Lo, Hi  int
	Dict    *Dict
}

// VisitColumn calls visit with the sources of column (t, c) over file rows [lo, hi), a batch of
// chunks at a time (decoded pages are dropped after their batch). visit returning true stops.
// useDicts: a chunk wholly in the range contributes its dictionary instead of its
// dictionary-coded pages (only its other pages are decoded).
func (q *Request) VisitColumn(ctx context.Context, t, c int, lo, hi uint32, useDicts bool, visit func(ColumnSource) bool) error {
	col, err := q.Column(t, c)
	if err != nil || col.Group == nil || lo >= hi {
		return err
	}
	g := col.Group
	p0 := g.PageOf(lo)
	if p0 < 0 {
		return nil
	}
	const batchPages, batchDicts = 64, 16
	for p := p0; p < len(g.Pages) && g.Pages[p].First < hi; {
		// one batch: the pages of whole chunks, up to about batchPages pages to decode
		var dictChunks, pages []int
		for p < len(g.Pages) && g.Pages[p].First < hi && len(pages) < batchPages && len(dictChunks) < batchDicts {
			ci := int(g.Pages[p].Chunk)
			ch := &g.Chunks[ci]
			last := &ch.Pages[len(ch.Pages)-1]
			whole := useDicts && ch.Dicts[col.K].Present() && ch.Pages[0].First >= lo && last.First+last.Rows <= hi
			if whole {
				dictChunks = append(dictChunks, ci)
			}
			for ; p < len(g.Pages) && int(g.Pages[p].Chunk) == ci && g.Pages[p].First < hi; p++ {
				pg := &g.Pages[p]
				b := &pg.Bufs[col.K]
				if b.Nulls == pg.Rows || (whole && b.Encoding() == EncRLEDict) {
					continue
				}
				pages = append(pages, p)
			}
		}
		// a dictionary is read alone (exactly its bytes) unless pages of its chunk are read too
		withPages := map[int]bool{}
		for _, pi := range pages {
			withPages[int(g.Pages[pi].Chunk)] = true
		}
		var ranges []Range
		exact := map[int][]byte{}
		for _, ci := range dictChunks {
			ch := &g.Chunks[ci]
			d := ch.Dicts[col.K]
			if withPages[ci] {
				ranges = append(ranges, Range{Off: d.Off, Len: int64(d.Len), Region: ch.Region()})
				continue
			}
			b, err := q.fetch.ReadExact(ctx, d.Off, int(d.Len))
			if err != nil {
				return err
			}
			exact[ci] = b
		}
		if err := q.fetch.Fetch(ctx, ranges); err != nil {
			return err
		}
		if len(pages) > 0 {
			if err := q.Load(ctx, []PageSel{{T: t, C: c, Pages: pages}}, nil); err != nil {
				return err
			}
		}
		for _, ci := range dictChunks {
			ch := &g.Chunks[ci]
			d := ch.Dicts[col.K]
			b, ok := exact[ci]
			if !ok {
				var err error
				if b, err = q.bytes(d.Off, d.Len, ch.Region()); err != nil {
					return err
				}
			}
			page, err := q.dec.dictPage(b)
			if err != nil {
				return err
			}
			var entries [][]byte
			if col.Spec.Phys == PhysBytes {
				if entries, err = plainByteArrays(page, d.Entries); err != nil {
					return err
				}
			}
			if visit(ColumnSource{Entries: entries}) {
				return nil
			}
		}
		for _, pi := range pages {
			v := col.pages[pi]
			pg := &g.Pages[pi]
			a, e := 0, int(pg.Rows)
			if pg.First < lo {
				a = int(lo - pg.First)
			}
			if pg.First+pg.Rows > hi {
				e = int(hi - pg.First)
			}
			stop := visit(ColumnSource{Page: v, Lo: a, Hi: e, Dict: &col.dict})
			col.pages[pi] = nil
			if stop {
				return nil
			}
		}
	}
	return nil
}

// RunValues is what a key-major run holds as values, read without its owner and link
// buffers: its dictionary when the value buffer is dictionary-coded, else the decoded value
// buffer (Vals nil and Entries nil: the run has no values).
type RunValues struct {
	Ref     RunRef
	Meta    Run
	Entries [][]byte
	Vals    *Values
	Dict    *Dict
}

// RunDistinct reads the values of runs in two waves: their headers, then their dictionaries
// or value buffers.
func (q *Request) RunDistinct(ctx context.Context, refs []RunRef) ([]RunValues, error) {
	type head struct {
		run *Run
		reg Region
		n   int64
	}
	heads := make([]head, len(refs))
	ranges := make([]Range, 0, len(refs))
	for i, ref := range refs {
		run, reg, err := q.runMeta(ref)
		if err != nil {
			return nil, err
		}
		heads[i] = head{run: run, reg: reg, n: min(int64(run.Len), runHeaderProbe)}
		ranges = append(ranges, Range{Off: run.Off, Len: heads[i].n, Region: reg})
	}
	if err := q.fetch.Fetch(ctx, ranges); err != nil {
		return nil, err
	}
	bodies := make([]*RunBody, len(refs))
	ranges = ranges[:0]
	for i, h := range heads {
		b, err := q.bytes(h.run.Off, uint32(h.n), h.reg)
		if err != nil {
			return nil, err
		}
		rb, need, err := parseRunHeaderPrefix(b, h.run.Off, int64(h.run.Len))
		if err != nil {
			return nil, fmt.Errorf("cgfile: run %+v: %w", refs[i], err)
		}
		if rb == nil { // header longer than the probe
			if err := q.fetch.Fetch(ctx, []Range{{Off: h.run.Off, Len: need, Region: h.reg}}); err != nil {
				return nil, err
			}
			if b, err = q.bytes(h.run.Off, uint32(need), h.reg); err != nil {
				return nil, err
			}
			if rb, _, err = parseRunHeaderPrefix(b, h.run.Off, int64(h.run.Len)); err != nil || rb == nil {
				return nil, fmt.Errorf("cgfile: run %+v: bad header: %v", refs[i], err)
			}
		}
		bodies[i] = rb
		if rb.Val == nil {
			continue
		}
		if rb.Val.Encoding() == EncRLEDict && rb.Dict.Present() {
			ranges = append(ranges, Range{Off: rb.Dict.Off, Len: int64(rb.Dict.Len), Region: h.reg})
		} else {
			ranges = append(ranges, Range{Off: rb.Val.Off, Len: int64(rb.Val.Len), Region: h.reg})
		}
	}
	if err := q.fetch.Fetch(ctx, ranges); err != nil {
		return nil, err
	}
	out := make([]RunValues, len(refs))
	for i, h := range heads {
		rb := bodies[i]
		rv := RunValues{Ref: refs[i], Meta: *h.run, Dict: &Dict{}}
		if rb.Val != nil {
			if h.run.VCol >= uint8(len(VColPhys)) {
				return nil, fmt.Errorf("cgfile: run %+v has values but vcol %d", refs[i], h.run.VCol)
			}
			var dict [][]byte
			if rb.Dict.Present() {
				b, err := q.bytes(rb.Dict.Off, rb.Dict.Len, h.reg)
				if err != nil {
					return nil, err
				}
				page, err := q.dec.dictPage(b)
				if err != nil {
					return nil, err
				}
				if dict, err = plainByteArrays(page, rb.Dict.Entries); err != nil {
					return nil, err
				}
			}
			if rb.Val.Encoding() == EncRLEDict && rb.Dict.Present() {
				rv.Entries = dict
			} else {
				b, err := q.bytes(rb.Val.Off, rb.Val.Len, h.reg)
				if err != nil {
					return nil, err
				}
				rv.Dict.Values = dict
				if rv.Vals, err = q.dec.decodeBuf(b, rb.Val, int(h.run.Rows), VColPhys[h.run.VCol], len(dict), nil); err != nil {
					return nil, fmt.Errorf("cgfile: run %+v values: %w", refs[i], err)
				}
			}
		}
		out[i] = rv
	}
	return out, nil
}

// parseRunHeaderPrefix parses a run header from the run's leading bytes b (the run starts at
// file offset off and is runLen bytes). It returns a nil body and the bytes needed when b is
// too short to hold the header.
func parseRunHeaderPrefix(b []byte, off, runLen int64) (*RunBody, int64, error) {
	c := &cursor{b: b}
	hl := c.uvarint()
	if c.err != nil {
		if int64(len(b)) < runLen && len(b) < 10 {
			return nil, min(runLen, 16), nil
		}
		return nil, 0, fmt.Errorf("run header length: %w", c.err)
	}
	if need := int64(c.p) + int64(hl); need > int64(len(b)) {
		if need > runLen {
			return nil, 0, fmt.Errorf("run header of %d bytes exceeds the run (%d)", hl, runLen)
		}
		return nil, need, nil
	}
	h := &cursor{b: b[c.p : c.p+int(hl)]}
	o := off + int64(c.p) + int64(hl)
	rb := &RunBody{}
	runBuf(h, &rb.Owner, &o)
	if h.u8() != 0 {
		rb.Dict = DictDesc{Off: o, Len: h.u32(), Entries: h.u32()}
		o += int64(rb.Dict.Len)
	}
	misc := h.u8()
	if misc&1 != 0 {
		rb.Val = &Buf{}
		runBuf(h, rb.Val, &o)
	}
	if n := int(misc >> 1); n > 0 {
		rb.Links = make([]Buf, n)
		for i := range rb.Links {
			runBuf(h, &rb.Links[i], &o)
		}
	}
	if h.err != nil {
		return nil, 0, fmt.Errorf("run header: %w", h.err)
	}
	if !h.done() {
		return nil, 0, fmt.Errorf("run header has %d trailing bytes", len(h.b)-h.p)
	}
	if o != off+runLen {
		return nil, 0, fmt.Errorf("run buffers end at %d, run ends at %d", o, off+runLen)
	}
	return rb, 0, nil
}

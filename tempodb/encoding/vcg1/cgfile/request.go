package cgfile

import (
	"context"
	"fmt"
	"sort"
)

// Request is the state of one request on a block: the footer it read, the bytes it fetched,
// and what it decoded. Not safe for concurrent use.
type Request struct {
	rd    *Reader
	F     *Footer
	fetch *Fetcher
	dec   decoder

	groups [NumTables][]*Group
	km     [NumLevels][]KmFlush
	kmDone [NumLevels]bool
	cols   map[colKey]*Column
	runs   map[RunRef]*RunData

	ranges []Range
}

type colKey struct{ t, c int }

// Stats returns the backend requests issued so far.
func (q *Request) Stats() Stats { return q.fetch.Stats }

// CountRead adds a read of n bytes made outside the request (the footer, read through a
// cache) to its stats.
func (q *Request) CountRead(n int) {
	q.fetch.Stats.Waves++
	q.fetch.Stats.Requests++
	q.fetch.Stats.Bytes += int64(n)
}

// Rows is the row count of table t.
func (q *Request) Rows(t int) uint32 { return q.F.Rows[t] }

// Group returns group gi of table t, decoded once per request.
func (q *Request) Group(t, gi int) (*Group, error) {
	if q.groups[t] == nil {
		q.groups[t] = make([]*Group, len(q.F.Groups[t]))
	}
	if gi < 0 || gi >= len(q.groups[t]) {
		return nil, fmt.Errorf("cgfile: no group %d in %s", gi, TableName(t))
	}
	if g := q.groups[t][gi]; g != nil {
		return g, nil
	}
	g, err := q.F.DecodeGroup(t, gi)
	if err != nil {
		return nil, err
	}
	q.groups[t][gi] = g
	return g, nil
}

// Km returns the key-major flushes of level l, decoded once per request.
func (q *Request) Km(l int) ([]KmFlush, error) {
	if !q.kmDone[l] {
		k, err := q.F.DecodeKm(l)
		if err != nil {
			return nil, err
		}
		q.km[l], q.kmDone[l] = k, true
	}
	return q.km[l], nil
}

// PageSel selects pages (indices into the column's group pages) of one column.
type PageSel struct {
	T, C  int
	Pages []int
}

// Load reads, in ONE wave, the selected pages (with the dictionaries of their chunks) and
// runs that the request does not hold yet, then decodes them.
func (q *Request) Load(ctx context.Context, pages []PageSel, runs []RunRef) error {
	type dictNeed struct {
		col   *Column
		chunk int
	}
	var dicts []dictNeed
	q.ranges = q.ranges[:0]
	for _, s := range pages {
		c, err := q.Column(s.T, s.C)
		if err != nil {
			return err
		}
		if c.Group == nil {
			continue
		}
		for _, pi := range s.Pages {
			if c.pages[pi] != nil {
				continue
			}
			p := &c.Group.Pages[pi]
			ch := &c.Group.Chunks[p.Chunk]
			if d := ch.Dicts[c.K]; d.Present() && !c.chunks[p.Chunk].loaded && !c.chunks[p.Chunk].wanted {
				c.chunks[p.Chunk].wanted = true
				dicts = append(dicts, dictNeed{c, int(p.Chunk)})
				q.ranges = append(q.ranges, Range{Off: d.Off, Len: int64(d.Len), Region: ch.Region()})
			}
			b := &p.Bufs[c.K]
			q.ranges = append(q.ranges, Range{Off: b.Off, Len: int64(b.Len), Region: ch.Region()})
		}
	}
	for _, ref := range runs {
		if q.runs[ref] != nil {
			continue
		}
		run, reg, err := q.runMeta(ref)
		if err != nil {
			return err
		}
		q.ranges = append(q.ranges, Range{Off: run.Off, Len: int64(run.Len), Region: reg})
	}
	if err := q.fetch.Fetch(ctx, q.ranges); err != nil {
		return err
	}
	for _, d := range dicts {
		d.col.chunks[d.chunk].wanted = false
		if err := q.loadDict(d.col, d.chunk); err != nil {
			return err
		}
	}
	for _, s := range pages {
		c := q.cols[colKey{s.T, s.C}]
		if c.Group == nil {
			continue
		}
		for _, pi := range s.Pages {
			if c.pages[pi] == nil {
				if err := q.decodePage(c, pi); err != nil {
					return fmt.Errorf("cgfile: %s.%s page %d: %w", TableName(c.T), c.Spec.Name, pi, err)
				}
			}
		}
	}
	for _, ref := range runs {
		if q.runs[ref] != nil {
			continue
		}
		rd, err := q.decodeRun(ref)
		if err != nil {
			return err
		}
		q.runs[ref] = rd
	}
	return nil
}

// RowSel selects rows of a table: the given columns (nil = every stored column) over the
// file-row intervals.
type RowSel struct {
	T    int
	Cols []int
	Ivs  []Interval
}

// Interval is a half-open file-row interval.
type Interval struct{ Lo, Hi uint32 }

// LoadRows loads, in one wave, the pages covering the selected rows.
func (q *Request) LoadRows(ctx context.Context, sels []RowSel) error {
	var ps []PageSel
	for _, s := range sels {
		cols := s.Cols
		if cols == nil {
			cols = StoredCols(s.T)
		}
		for _, c := range cols {
			col, err := q.Column(s.T, c)
			if err != nil {
				return err
			}
			if pg := col.PagesForIntervals(s.Ivs, nil); len(pg) > 0 {
				ps = append(ps, PageSel{T: s.T, C: c, Pages: pg})
			}
		}
	}
	return q.Load(ctx, ps, nil)
}

func (q *Request) bytes(off int64, n uint32, reg Region) ([]byte, error) {
	return q.fetch.Bytes(Range{Off: off, Len: int64(n), Region: reg})
}

func (q *Request) loadDict(c *Column, chunk int) error {
	ch := &c.Group.Chunks[chunk]
	d := ch.Dicts[c.K]
	b, err := q.bytes(d.Off, d.Len, ch.Region())
	if err != nil {
		return err
	}
	page, err := q.dec.dictPage(b)
	if err != nil {
		return err
	}
	cd := &c.chunks[chunk]
	if c.Spec.Phys != PhysBytes {
		cd.ints = plainInt32s(page)
		if uint32(len(cd.ints)) != d.Entries {
			return fmt.Errorf("cgfile: %s.%s int dictionary has %d entries, footer says %d", TableName(c.T), c.Spec.Name, len(cd.ints), d.Entries)
		}
	} else {
		entries, err := plainByteArrays(page, d.Entries)
		if err != nil {
			return err
		}
		cd.remap = c.dict.merge(entries)
	}
	cd.loaded = true
	return nil
}

func (q *Request) decodePage(c *Column, pi int) error {
	p := &c.Group.Pages[pi]
	ch := &c.Group.Chunks[p.Chunk]
	b := &p.Bufs[c.K]
	data, err := q.bytes(b.Off, b.Len, ch.Region())
	if err != nil {
		return err
	}
	cd := &c.chunks[p.Chunk]
	dictLen := int(ch.Dicts[c.K].Entries)
	if !ch.Dicts[c.K].Present() {
		dictLen = 0
	}
	v, err := q.dec.decodeBuf(data, b, int(p.Rows), c.Spec.Phys, dictLen, cd.ints)
	if err != nil {
		return err
	}
	if v.Dict && cd.remap != nil {
		for i, x := range v.Codes {
			if x != NullCode {
				v.Codes[i] = cd.remap[x]
			}
		}
	}
	v.First = p.First
	c.pages[pi] = v
	return nil
}

// DropPagesBefore forgets the decoded pages of table t that end at or before file row r, so a
// request walking the table in row order keeps only the pages it still needs. Fetched bytes and
// merged dictionaries are kept.
func (q *Request) DropPagesBefore(t int, r uint32) {
	for k, c := range q.cols {
		if k.t != t || c.Group == nil {
			continue
		}
		for i, v := range c.pages {
			if v == nil {
				continue
			}
			p := &c.Group.Pages[i]
			if p.First+p.Rows > r {
				break
			}
			c.pages[i] = nil
		}
	}
}

// DropRun forgets a decoded run.
func (q *Request) DropRun(ref RunRef) { delete(q.runs, ref) }

// SortedBounds returns the rows of table t whose column c, a sorted zoned int column without
// nulls (owner rows, entry rows), holds a value in want: contiguous, since the column is
// sorted. It reads, in one wave, at most the two pages where the bounds fall; their zones settle
// the other pages.
func (q *Request) SortedBounds(ctx context.Context, t, c int, want Interval) (Interval, error) {
	col, err := q.Column(t, c)
	if err != nil || col.Group == nil {
		return Interval{}, err
	}
	ps := col.Group.Pages
	k := col.K
	i := sort.Search(len(ps), func(i int) bool { return ps[i].Bufs[k].Zone.Max >= int64(want.Lo) })
	j := sort.Search(len(ps), func(i int) bool { return ps[i].Bufs[k].Zone.Min >= int64(want.Hi) })
	if want.Lo >= want.Hi || i >= j {
		r := q.Rows(t)
		if i < len(ps) {
			r = ps[i].First
		}
		return Interval{r, r}, nil
	}
	var need []int
	if ps[i].Bufs[k].Zone.Min < int64(want.Lo) {
		need = append(need, i)
	}
	if ps[j-1].Bufs[k].Zone.Max >= int64(want.Hi) && (len(need) == 0 || need[0] != j-1) {
		need = append(need, j-1)
	}
	if err := q.Load(ctx, []PageSel{{T: t, C: c, Pages: need}}, nil); err != nil {
		return Interval{}, err
	}
	// first row of page pi whose value is >= v
	bound := func(pi int, v uint32) uint32 {
		vals := col.Values(pi)
		n := sort.Search(vals.N, func(x int) bool { return vals.Ints[x] >= int64(v) })
		return ps[pi].First + uint32(n)
	}
	out := Interval{ps[i].First, ps[j-1].First + ps[j-1].Rows}
	if ps[i].Bufs[k].Zone.Min < int64(want.Lo) {
		out.Lo = bound(i, want.Lo)
	}
	if ps[j-1].Bufs[k].Zone.Max >= int64(want.Hi) {
		out.Hi = bound(j-1, want.Hi)
	}
	return out, nil
}

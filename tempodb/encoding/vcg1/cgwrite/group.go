package cgwrite

import (
	"bytes"

	cg "github.com/grafana/tempo/v3/tempodb/encoding/vcg1/cgfile"
)

// colBuf is one column of a group: the pending page's values (non-null values only, plus a
// validity per row for nullable columns) and the pending chunk's dictionary.
type colBuf struct {
	spec cg.ColSpec
	enc  enc

	valid []bool
	i32   []int32
	i64   []int64
	f64   []float64
	bools []bool
	data  []byte
	offs  []uint32 // len(values)+1, offs[0] = 0

	sdict *strDict
	idict *i32Dict
	plain bool // the chunk's dictionary passed ChunkBytes/4: the rest of the chunk is DELTA_LENGTH
}

func (c *colBuf) reset() {
	c.valid, c.i32, c.i64, c.f64, c.bools, c.data = c.valid[:0], c.i32[:0], c.i64[:0], c.f64[:0], c.bools[:0], c.data[:0]
	c.offs = append(c.offs[:0], 0)
}

func (c *colBuf) newChunk() {
	c.plain = false
	c.sdict, c.idict = nil, nil
	switch c.enc {
	case encDictStr:
		c.sdict = newStrDict()
	case encDictInt:
		c.idict = newI32Dict()
	}
}

func (c *colBuf) memBytes() int {
	n := cap(c.valid) + 4*cap(c.i32) + 8*cap(c.i64) + 8*cap(c.f64) + cap(c.bools) + cap(c.data) + 4*cap(c.offs)
	if c.sdict != nil {
		n += c.sdict.memBytes()
	}
	if c.idict != nil {
		n += 12 * len(c.idict.vals)
	}
	return n
}

type pageMeta struct {
	rows uint32
	bufs []buf
}

type dictMeta struct {
	present      bool
	len, entries uint32
}

type chunkMeta struct {
	off, len uint64
	dicts    []dictMeta
	pages    []pageMeta
}

// groupW writes one column group: rows are cut into pages of about pageBytes uncompressed
// bytes; pages are encoded as they fill and held compressed until the pending chunk would pass
// ChunkBytes, then the chunk (its dictionaries, then its pages) goes to the sink.
type groupW struct {
	t     int
	g     group
	cols  []*colBuf
	rows  int // rows of the pending page
	acc   int // uncompressed bytes of the pending page
	pages []pageMeta
	out   []byte // the pending chunk's encoded pages
	last  int    // encoded bytes of the last page
	// chunks are the written chunks, for the footer.
	chunks []chunkMeta
	npages int
}

func newGroupW(t int, g group) *groupW {
	gw := &groupW{t: t, g: g}
	for _, c := range g.cols {
		cb := &colBuf{spec: cg.Cols(t)[c], enc: colEnc(t, c)}
		cb.reset()
		cb.newChunk()
		gw.cols = append(gw.cols, cb)
	}
	return gw
}

func (g *groupW) dictBytes() int {
	n := 0
	for _, c := range g.cols {
		if c.sdict != nil {
			n += c.sdict.size
		}
		if c.idict != nil {
			n += 4 * len(c.idict.vals)
		}
	}
	return n
}

func (g *groupW) memBytes() int {
	n := cap(g.out)
	for _, c := range g.cols {
		n += c.memBytes()
	}
	return n
}

// endRow closes a row and cuts the page once it holds pageBytes.
func (g *groupW) endRow(w *Writer) error {
	g.rows++
	if g.acc >= g.g.pageBytes {
		return g.cutPage(w)
	}
	return nil
}

// cutPage encodes the pending page (first flushing the pending chunk if the page would take
// it past ChunkBytes).
func (g *groupW) cutPage(w *Writer) error {
	if g.rows == 0 {
		return nil
	}
	if len(g.pages) > 0 && len(g.out)+g.dictBytes()+g.last > w.chunkBytes {
		if err := g.flush(w); err != nil {
			return err
		}
	}
	before := len(g.out)
	bufs := make([]buf, len(g.cols))
	for k, c := range g.cols {
		g.out, bufs[k] = g.encodeCol(w.c, c, g.out)
		if c.sdict != nil && !c.plain && c.sdict.size > w.chunkBytes/4 {
			c.plain = true
		}
		c.reset()
	}
	g.last = len(g.out) - before
	g.pages = append(g.pages, pageMeta{rows: uint32(g.rows), bufs: bufs})
	g.rows, g.acc = 0, 0
	return nil
}

func (g *groupW) encodeCol(cd *coder, c *colBuf, out []byte) ([]byte, buf) {
	var valid []bool
	if c.spec.Opt {
		valid = c.valid
	}
	var z zone
	switch c.spec.Phys {
	case cg.PhysInt32:
		if c.spec.Zone {
			z = intZone32(c.i32)
		}
		switch c.enc {
		case encDictInt:
			idx := cd.i32[:0]
			for _, v := range c.i32 {
				idx = append(idx, c.idict.id(v))
			}
			cd.i32 = idx
			return cd.finish(out, valid, cd.rleDict(idx), idRLEDict, z)
		case encDelta:
			return cd.finish(out, valid, cd.deltaI32(c.i32), idDelta, z)
		}
		cd.data = plainI32(cd.data[:0], c.i32)
		return cd.finish(out, valid, cd.data, idPlain, z)
	case cg.PhysInt64:
		if c.spec.Zone {
			z = intZone(c.i64)
		}
		if c.enc == encDelta {
			return cd.finish(out, valid, cd.deltaI64(c.i64), idDelta, z)
		}
		cd.data = plainI64(cd.data[:0], c.i64)
		return cd.finish(out, valid, cd.data, idPlain, z)
	case cg.PhysDouble:
		cd.data = plainF64(cd.data[:0], c.f64)
		return cd.finish(out, valid, cd.data, idPlain, z)
	case cg.PhysBool:
		cd.data = plainBool(cd.data[:0], c.bools)
		return cd.finish(out, valid, cd.data, idPlain, z)
	}
	n := len(c.offs) - 1
	if c.spec.Zone && n > 0 {
		lo, hi := c.data[c.offs[0]:c.offs[1]], c.data[c.offs[0]:c.offs[1]]
		for i := 1; i < n; i++ {
			v := c.data[c.offs[i]:c.offs[i+1]]
			if bytes.Compare(v, lo) < 0 {
				lo = v
			}
			if bytes.Compare(v, hi) > 0 {
				hi = v
			}
		}
		z = zone{kind: cg.ZoneBytes, bmin: bytes.Clone(lo), bmax: bytes.Clone(hi)}
	}
	if c.sdict != nil && !c.plain {
		idx := cd.i32[:0]
		for i := 0; i < n; i++ {
			idx = append(idx, c.sdict.id(c.data[c.offs[i]:c.offs[i+1]]))
		}
		cd.i32 = idx
		return cd.finish(out, valid, cd.rleDict(idx), idRLEDict, z)
	}
	return cd.finish(out, valid, cd.deltaLen(c.data, c.offs), idDeltaLen, z)
}

// flush writes the pending chunk: its dictionaries (zstd PLAIN pages), then its pages.
func (g *groupW) flush(w *Writer) error {
	if len(g.pages) == 0 {
		return nil
	}
	cd := w.c
	off := w.sink.pos
	dicts := make([]dictMeta, len(g.cols))
	for k, c := range g.cols {
		var page []byte
		var n int
		switch {
		case c.sdict != nil && c.sdict.n() > 0:
			page, n = c.sdict.page(cd.data[:0]), c.sdict.n()
		case c.idict != nil && len(c.idict.vals) > 0:
			page, n = plainI32(cd.data[:0], c.idict.vals), len(c.idict.vals)
		default:
			continue
		}
		cd.data = page
		z := cd.compress(page)
		if _, err := w.sink.write(z); err != nil {
			return err
		}
		dicts[k] = dictMeta{present: true, len: uint32(len(z)), entries: uint32(n)}
	}
	if _, err := w.sink.write(g.out); err != nil {
		return err
	}
	g.chunks = append(g.chunks, chunkMeta{off: off, len: w.sink.pos - off, dicts: dicts, pages: g.pages})
	g.npages += len(g.pages)
	g.pages = nil
	g.out = g.out[:0]
	for _, c := range g.cols {
		c.newChunk()
	}
	return nil
}

// tableW routes a table's rows to its groups.
type tableW struct {
	t      int
	groups []*groupW
	col    []*colBuf // by column id; nil when not stored
	gof    []*groupW // by column id
}

func newTableW(t, pageBytes, attrPageBytes int) *tableW {
	tw := &tableW{t: t, col: make([]*colBuf, len(cg.Cols(t))), gof: make([]*groupW, len(cg.Cols(t)))}
	for _, g := range defaultGroups(t) {
		if t >= 7 { // point-copy attributes and elements
			g.pageBytes = attrPageBytes
		} else {
			g.pageBytes = pageBytes
		}
		gw := newGroupW(t, g)
		tw.groups = append(tw.groups, gw)
		for k, c := range g.cols {
			tw.col[c], tw.gof[c] = gw.cols[k], gw
		}
	}
	return tw
}

func (tw *tableW) i32(c int, v int32) {
	cb := tw.col[c]
	cb.i32 = append(cb.i32, v)
	tw.gof[c].acc += 4
}

func (tw *tableW) i64(c int, v int64) {
	cb := tw.col[c]
	if cb.spec.Opt {
		cb.valid = append(cb.valid, true)
	}
	cb.i64 = append(cb.i64, v)
	tw.gof[c].acc += 8
}

func (tw *tableW) f64(c int, v float64) {
	cb := tw.col[c]
	cb.valid = append(cb.valid, true)
	cb.f64 = append(cb.f64, v)
	tw.gof[c].acc += 8
}

func (tw *tableW) bool(c int, v bool) {
	cb := tw.col[c]
	cb.valid = append(cb.valid, true)
	cb.bools = append(cb.bools, v)
	tw.gof[c].acc++
}

func (tw *tableW) bytes(c int, v []byte) {
	cb := tw.col[c]
	if cb.spec.Opt {
		cb.valid = append(cb.valid, true)
	}
	cb.data = append(cb.data, v...)
	cb.offs = append(cb.offs, uint32(len(cb.data)))
	tw.gof[c].acc += 4 + len(v)
}

func (tw *tableW) str(c int, v string) {
	cb := tw.col[c]
	if cb.spec.Opt {
		cb.valid = append(cb.valid, true)
	}
	cb.data = append(cb.data, v...)
	cb.offs = append(cb.offs, uint32(len(cb.data)))
	tw.gof[c].acc += 4 + len(v)
}

// null adds a null to a nullable column.
func (tw *tableW) null(c int) {
	cb := tw.col[c]
	cb.valid = append(cb.valid, false)
	tw.gof[c].acc += width(cb.spec.Phys)
}

func (tw *tableW) endRow(w *Writer) error {
	for _, g := range tw.groups {
		if err := g.endRow(w); err != nil {
			return err
		}
	}
	w.rows[tw.t]++
	return nil
}

func (tw *tableW) finish(w *Writer) error {
	for _, g := range tw.groups {
		if err := g.cutPage(w); err != nil {
			return err
		}
		if err := g.flush(w); err != nil {
			return err
		}
	}
	return nil
}

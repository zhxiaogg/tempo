package vcg1

import (
	"context"
	"fmt"
	"sort"
	"unsafe"

	"github.com/parquet-go/parquet-go"

	cg "github.com/grafana/tempo/v3/tempodb/encoding/vcg1/cgfile"
)

// windowSpans is the target span count of a processing window (as the Rust CG server).
const windowSpans = 1 << 19

// window is a processing window: the rows of every table between two anchors.
type window struct {
	q      *cg.Request
	lo, hi [cg.NumTables]uint32
}

func newWindow(q *cg.Request, a, b int) *window {
	w := &window{q: q}
	for t := 0; t < cg.NumTables; t++ {
		w.lo[t], w.hi[t] = q.F.Anchor(a, t), q.F.Anchor(b, t)
	}
	return w
}

func (w *window) n(t int) uint32 { return w.hi[t] - w.lo[t] }

func (w *window) all(t int) []cg.Interval { return []cg.Interval{{Lo: w.lo[t], Hi: w.hi[t]}} }

// windows cuts anchors [a, b) into processing windows of about windowSpans spans. The first
// windows grow 4x from one anchor, so a limited search reads little before it is satisfied.
func windows(f *cg.Footer, a, b int) [][2]int {
	var out [][2]int
	for n := 1; a < b; n *= 4 {
		e := a + 1
		for e < b && e-a < n && f.Anchor(e, cg.TSpans)-f.Anchor(a, cg.TSpans) < windowSpans {
			e++
		}
		out = append(out, [2]int{a, e})
		a = e
	}
	return out
}

// loader collects the column rows and runs a stage needs, then reads them in ONE wave.
type loader struct {
	w    *window
	cols map[[2]int][]cg.Interval
	runs []cg.RunRef
}

func newLoader(w *window) *loader { return &loader{w: w, cols: map[[2]int][]cg.Interval{}} }

func (l *loader) col(t, c int, ivs []cg.Interval) {
	k := [2]int{t, c}
	l.cols[k] = append(l.cols[k], ivs...)
}

func (l *loader) allRows(t, c int) { l.col(t, c, l.w.all(t)) }

func (l *loader) run(refs ...cg.RunRef) { l.runs = append(l.runs, refs...) }

func (l *loader) exec(ctx context.Context) error {
	var ps []cg.PageSel
	for k, ivs := range l.cols {
		col, err := l.w.q.Column(k[0], k[1])
		if err != nil {
			return err
		}
		if pg := col.PagesForIntervals(mergeIvs(ivs), nil); len(pg) > 0 {
			ps = append(ps, cg.PageSel{T: k[0], C: k[1], Pages: pg})
		}
	}
	err := l.w.q.Load(ctx, ps, l.runs)
	l.cols, l.runs = map[[2]int][]cg.Interval{}, nil
	return err
}

func mergeIvs(ivs []cg.Interval) []cg.Interval {
	sort.Slice(ivs, func(i, j int) bool { return ivs[i].Lo < ivs[j].Lo })
	out := ivs[:0]
	for _, iv := range ivs {
		if iv.Lo >= iv.Hi {
			continue
		}
		if n := len(out); n > 0 && iv.Lo <= out[n-1].Hi {
			out[n-1].Hi = max(out[n-1].Hi, iv.Hi)
			continue
		}
		out = append(out, iv)
	}
	return out
}

// rowIvs turns ascending file rows into intervals.
func rowIvs(rows []uint32) []cg.Interval {
	var out []cg.Interval
	for _, r := range rows {
		if n := len(out); n > 0 && out[n-1].Hi >= r {
			out[n-1].Hi = max(out[n-1].Hi, r+1)
			continue
		}
		out = append(out, cg.Interval{Lo: r, Hi: r + 1})
	}
	return out
}

// cursor reads a loaded column by file row, caching the current page (ascending access is
// O(1) amortized).
type cursor struct {
	c          *cg.Column
	v          *cg.Values
	first, end uint32
}

func (w *window) cursor(t, c int) (*cursor, error) {
	col, err := w.q.Column(t, c)
	if err != nil {
		return nil, err
	}
	return &cursor{c: col}, nil
}

func (k *cursor) at(r uint32) (*cg.Values, int) {
	if k.v == nil || r < k.first || r >= k.end {
		v, i, err := k.c.At(r)
		if err != nil {
			panic(fmt.Sprintf("vCG1: %v", err))
		}
		k.v, k.first, k.end = v, v.First, v.First+uint32(v.N)
		return v, i
	}
	return k.v, int(r - k.first)
}

func (k *cursor) int(r uint32) int64 {
	v, i := k.at(r)
	return v.Ints[i]
}

// bytes returns the value of a byte-array row (nil, false for null).
func (k *cursor) bytes(r uint32) ([]byte, bool) {
	v, i := k.at(r)
	if v.IsNull(i) {
		return nil, false
	}
	return v.Bytes(i, k.c.Dict()), true
}

func (k *cursor) str(r uint32) string {
	b, _ := k.bytes(r)
	return bstr(b)
}

// value returns row r as a parquet value of the column's physical type.
func (k *cursor) value(r uint32) parquet.Value {
	v, i := k.at(r)
	return valueOf(v, i, k.c.Dict())
}

func valueOf(v *cg.Values, i int, d *cg.Dict) parquet.Value {
	if v.IsNull(i) {
		return parquet.Value{}
	}
	switch v.Phys {
	case cg.PhysBytes:
		return parquet.ByteArrayValue(v.Bytes(i, d))
	case cg.PhysBool:
		return parquet.BooleanValue(v.Bools[i])
	case cg.PhysDouble:
		return parquet.DoubleValue(v.Floats[i])
	case cg.PhysInt32:
		return parquet.Int32Value(int32(v.Ints[i]))
	default:
		return parquet.Int64Value(v.Ints[i])
	}
}

// bstr is a zero-copy string over immutable decoded bytes (they live as long as the request
// holds them or a string points into them).
func bstr(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(unsafe.SliceData(b), len(b))
}

// firsts returns the parent offset column (t, c) over the window's parent rows as window-local
// child offsets, with the window end of the child table appended.
func (w *window) firsts(t, c, child int) ([]uint32, error) {
	k, err := w.cursor(t, c)
	if err != nil {
		return nil, err
	}
	out := make([]uint32, 0, w.n(t)+1)
	for r := w.lo[t]; r < w.hi[t]; r++ {
		out = append(out, uint32(k.int(r))-w.lo[child])
	}
	return append(out, w.n(child)), nil
}

// parentOf maps ascending window-local child rows to their parents through a firsts array.
type parentOf struct {
	first []uint32
	p     int
}

func (p *parentOf) of(child uint32) uint32 {
	if p.p > 0 && child < p.first[p.p] {
		p.p = sort.Search(len(p.first), func(i int) bool { return p.first[i] > child }) - 1
	}
	for p.p+1 < len(p.first)-1 && p.first[p.p+1] <= child {
		p.p++
	}
	return uint32(p.p)
}

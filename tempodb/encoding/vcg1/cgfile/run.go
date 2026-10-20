package cgfile

import (
	"fmt"
)

// RunRef names one run of a key-major flush.
type RunRef struct {
	Level, Flush, Run int
}

// RunData is one decoded key-major run. Owners and Links[0] are file rows; for resource and
// scope levels Links[1] is a span count.
type RunData struct {
	Ref    RunRef
	Meta   Run
	Owners []uint32 // owner file row per entry, non-decreasing
	Vals   *Values  // nil when the run has no value buffer (VCol == VColNone)
	Dict   *Dict    // the run's string dictionary (Vals.Dict codes index it)
	Links  [][]uint32
}

// RunBody is a run's buffers, from its header (absolute offsets).
type RunBody struct {
	Owner Buf
	Dict  DictDesc
	Val   *Buf
	Links []Buf
}

// ParseRunHeader parses the header at the start of a run's bytes b (run at file offset off).
func ParseRunHeader(b []byte, off int64) (*RunBody, error) {
	c := &cursor{b: b}
	hl := c.uvarint()
	if c.err != nil || hl > uint64(len(b)-c.p) {
		return nil, fmt.Errorf("cgfile: run header truncated")
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
		return nil, fmt.Errorf("cgfile: run header: %w", h.err)
	}
	if !h.done() {
		return nil, fmt.Errorf("cgfile: run header has %d trailing bytes", len(h.b)-h.p)
	}
	if o != off+int64(len(b)) {
		return nil, fmt.Errorf("cgfile: run buffers end at %d, run ends at %d", o, off+int64(len(b)))
	}
	return rb, nil
}

func runBuf(h *cursor, b *Buf, o *int64) {
	b.Flags = h.u8()
	b.Len = h.u32()
	b.Off = *o
	*o += int64(b.Len)
	if b.HasNulls() {
		b.DefLen = h.u32()
		b.Nulls = h.u32()
	}
}

// KeyRuns returns the runs of key id at level l in flushes overlapping owner rows [o0, o1).
func (q *Request) KeyRuns(l int, key uint32, o0, o1 uint32) ([]RunRef, error) {
	km, err := q.Km(l)
	if err != nil {
		return nil, err
	}
	var out []RunRef
	for fi := range km {
		k := &km[fi]
		if k.Owner0 >= o1 || k.Owner0+k.Owners <= o0 {
			continue
		}
		a, b := k.KeyRuns(key)
		for i := a; i < b; i++ {
			out = append(out, RunRef{Level: l, Flush: fi, Run: i})
		}
	}
	return out, nil
}

// Run returns a run loaded by Load (nil if not loaded).
func (q *Request) Run(ref RunRef) *RunData { return q.runs[ref] }

func (q *Request) runMeta(ref RunRef) (*Run, Region, error) {
	km, err := q.Km(ref.Level)
	if err != nil {
		return nil, Region{}, err
	}
	if ref.Flush < 0 || ref.Flush >= len(km) || ref.Run < 0 || ref.Run >= len(km[ref.Flush].Runs) {
		return nil, Region{}, fmt.Errorf("cgfile: no run %+v", ref)
	}
	k := &km[ref.Flush]
	r := &k.Runs[ref.Run]
	return r, k.KeyRegion(r.Key), nil
}

func (q *Request) decodeRun(ref RunRef) (*RunData, error) {
	r, reg, err := q.runMeta(ref)
	if err != nil {
		return nil, err
	}
	b, err := q.bytes(r.Off, r.Len, reg)
	if err != nil {
		return nil, err
	}
	rb, err := ParseRunHeader(b, r.Off)
	if err != nil {
		return nil, err
	}
	sub := func(x *Buf) []byte { return b[x.Off-r.Off : x.Off-r.Off+int64(x.Len)] }
	rows := int(r.Rows)
	out := &RunData{Ref: ref, Meta: *r}
	if out.Owners, err = q.dec.u32s(sub(&rb.Owner), &rb.Owner, rows); err != nil {
		return nil, fmt.Errorf("cgfile: run %+v owners: %w", ref, err)
	}
	out.Dict = &Dict{}
	if rb.Dict.Present() {
		page, err := q.dec.dictPage(b[rb.Dict.Off-r.Off : rb.Dict.Off-r.Off+int64(rb.Dict.Len)])
		if err != nil {
			return nil, err
		}
		if out.Dict.Values, err = plainByteArrays(page, rb.Dict.Entries); err != nil {
			return nil, err
		}
	}
	if rb.Val != nil {
		if r.VCol >= uint8(len(VColPhys)) {
			return nil, fmt.Errorf("cgfile: run %+v has values but vcol %d", ref, r.VCol)
		}
		if out.Vals, err = q.dec.decodeBuf(sub(rb.Val), rb.Val, rows, VColPhys[r.VCol], out.Dict.Len(), nil); err != nil {
			return nil, fmt.Errorf("cgfile: run %+v values: %w", ref, err)
		}
	}
	out.Links = make([][]uint32, len(rb.Links))
	for i := range rb.Links {
		if out.Links[i], err = q.dec.u32s(sub(&rb.Links[i]), &rb.Links[i], rows); err != nil {
			return nil, fmt.Errorf("cgfile: run %+v links: %w", ref, err)
		}
	}
	return out, nil
}

// u32s decodes a required INT32 buffer as uint32s.
func (d *decoder) u32s(data []byte, b *Buf, rows int) ([]uint32, error) {
	v, err := d.decodeBuf(data, b, rows, PhysInt32, 0, nil)
	if err != nil {
		return nil, err
	}
	out := make([]uint32, rows)
	for i, x := range v.Ints {
		out[i] = uint32(x)
	}
	return out, nil
}

package cgwrite

import (
	"slices"
	"strings"

	cg "github.com/grafana/tempo/v3/tempodb/encoding/vcg1/cgfile"
)

// kmBucket holds the buffered scan-copy entries of one (key, value type), in insertion order
// (= owner, attribute, element order), column-wise: it becomes one run.
type kmBucket struct {
	key   uint32 // level key id (insertion order; remapped to the sorted id in the footer)
	vtype uint8
	owner []int32
	links [2][]int32
	data  []byte
	offs  []uint32 // strings: len+1 offsets
	ints  []int64
	dbls  []float64
	bools []bool
}

type kmRun struct {
	key   uint32
	vtype uint8
	vcol  uint8
	rows  uint32
	len   uint32
	zone  zone
}

type kmFlush struct {
	owner0, owners uint32
	off, len       uint64
	runs           []kmRun
}

// kmLevel buffers one level's scan-copy entries and writes them as flushes: one run per
// (key, value type), runs sorted by (key bytes, value type).
type kmLevel struct {
	l       int
	nlinks  int
	keyID   map[string]uint32
	keys    []string
	buckets map[uint64]*kmBucket
	bytes   int    // exact bytes of the buffered entries
	owner0  uint32 // first owner row of the pending flush
	entries uint32 // entries written (rows of the km table)
	flushes []kmFlush
	nruns   int
}

func newKmLevel(l int) *kmLevel {
	n := 0
	switch l {
	case cg.LResource, cg.LScope:
		n = 2
	case cg.LEvent, cg.LLink:
		n = 1
	}
	return &kmLevel{l: l, nlinks: n, keyID: map[string]uint32{}, buckets: map[uint64]*kmBucket{}}
}

func (k *kmLevel) key(s string) uint32 {
	if id, ok := k.keyID[s]; ok {
		return id
	}
	id := uint32(len(k.keys))
	s = strings.Clone(s) // kept for the whole file: do not pin the caller's buffer
	k.keys = append(k.keys, s)
	k.keyID[s] = id
	return id
}

const bucketOverhead = 256

// bucket returns the bucket of (key, vtype) with the entry's owner and links appended.
func (k *kmLevel) bucket(key uint32, vt uint8, owner int32, links [2]int32) *kmBucket {
	id := uint64(key)<<8 | uint64(vt)
	b := k.buckets[id]
	if b == nil {
		b = &kmBucket{key: key, vtype: vt}
		if vt == cg.VTStr {
			b.offs = []uint32{0}
		}
		k.buckets[id] = b
		k.bytes += bucketOverhead
	}
	b.owner = append(b.owner, owner)
	for i := 0; i < k.nlinks; i++ {
		b.links[i] = append(b.links[i], links[i])
	}
	k.bytes += 4 + 4*k.nlinks
	k.entries++
	return b
}

func (k *kmLevel) addStr(key uint32, owner int32, links [2]int32, v string) {
	b := k.bucket(key, cg.VTStr, owner, links)
	b.data = append(b.data, v...)
	b.offs = append(b.offs, uint32(len(b.data)))
	k.bytes += 4 + len(v)
}

func (k *kmLevel) addInt(key uint32, owner int32, links [2]int32, v int64) {
	b := k.bucket(key, cg.VTInt, owner, links)
	b.ints = append(b.ints, v)
	k.bytes += 8
}

func (k *kmLevel) addDbl(key uint32, owner int32, links [2]int32, v float64) {
	b := k.bucket(key, cg.VTDouble, owner, links)
	b.dbls = append(b.dbls, v)
	k.bytes += 8
}

func (k *kmLevel) addBool(key uint32, owner int32, links [2]int32, v bool) {
	b := k.bucket(key, cg.VTBool, owner, links)
	b.bools = append(b.bools, v)
	k.bytes++
}

// addNone adds a value-less entry (EmptyArr, Unsupported, Empty).
func (k *kmLevel) addNone(key uint32, vt uint8, owner int32, links [2]int32) {
	k.bucket(key, vt, owner, links)
}

// flush writes the buffered entries as one flush covering owner rows [owner0, ownerEnd).
func (k *kmLevel) flush(w *Writer, ownerEnd uint32) error {
	if len(k.buckets) == 0 {
		return nil
	}
	bs := make([]*kmBucket, 0, len(k.buckets))
	for _, b := range k.buckets {
		bs = append(bs, b)
	}
	slices.SortFunc(bs, func(a, b *kmBucket) int {
		if c := strings.Compare(k.keys[a.key], k.keys[b.key]); c != 0 {
			return c
		}
		return int(a.vtype) - int(b.vtype)
	})
	f := kmFlush{owner0: k.owner0, owners: ownerEnd - k.owner0, off: w.sink.pos, runs: make([]kmRun, 0, len(bs))}
	for i, b := range bs {
		r, err := w.writeRun(b, k.nlinks)
		if err != nil {
			return err
		}
		f.runs = append(f.runs, r)
		bs[i] = nil // release the entries as soon as they are written
		delete(k.buckets, uint64(b.key)<<8|uint64(b.vtype))
	}
	f.len = w.sink.pos - f.off
	k.flushes = append(k.flushes, f)
	k.nruns += len(f.runs)
	k.buckets = map[uint64]*kmBucket{}
	k.bytes = 0
	k.owner0 = ownerEnd
	return nil
}

// writeRun encodes one run (header, owner rows, [dictionary], [values], links) to the sink.
func (w *Writer) writeRun(b *kmBucket, nlinks int) (kmRun, error) {
	cd := w.c
	region := w.region[:0]
	r := kmRun{key: b.key, vtype: b.vtype, vcol: cg.VColNone, rows: uint32(len(b.owner))}
	var owner, val buf
	var links [2]buf
	hasVal := false
	var dict dictMeta

	region, owner = cd.finish(region, nil, cd.deltaI32(b.owner), idDelta, zone{})
	switch b.vtype {
	case cg.VTStr:
		r.vcol, hasVal = cg.VColStr, true
		n := len(b.offs) - 1
		d := newStrDict()
		idx := cd.i32[:0]
		for i := 0; i < n; i++ {
			idx = append(idx, d.id(b.data[b.offs[i]:b.offs[i+1]]))
		}
		cd.i32 = idx
		if n >= runRawMin && float64(d.n()) > runRawDistinct*float64(n) {
			region, val = cd.finish(region, nil, cd.deltaLen(b.data, b.offs), idDeltaLen, zone{})
		} else {
			page := d.page(cd.data[:0])
			cd.data = page
			z := cd.compress(page)
			region = append(region, z...)
			dict = dictMeta{present: true, len: uint32(len(z)), entries: uint32(d.n())}
			region, val = cd.finish(region, nil, cd.rleDict(idx), idRLEDict, zone{})
		}
	case cg.VTInt:
		r.vcol, hasVal = cg.VColInt, true
		cd.data = plainI64(cd.data[:0], b.ints)
		r.zone = intZone(b.ints)
		region, val = cd.finish(region, nil, cd.data, idPlain, zone{})
	case cg.VTDouble:
		r.vcol, hasVal = cg.VColDbl, true
		cd.data = plainF64(cd.data[:0], b.dbls)
		region, val = cd.finish(region, nil, cd.data, idPlain, zone{})
	case cg.VTBool:
		r.vcol, hasVal = cg.VColBool, true
		cd.data = plainBool(cd.data[:0], b.bools)
		region, val = cd.finish(region, nil, cd.data, idPlain, zone{})
	}
	for i := 0; i < nlinks; i++ {
		region, links[i] = cd.finish(region, nil, cd.deltaI32(b.links[i]), idDelta, zone{})
	}
	w.region = region

	h := w.hdr[:0]
	h = hdrBuf(h, &owner)
	if dict.present {
		h = append(h, 1)
		h = putUvarint(h, uint64(dict.len))
		h = putUvarint(h, uint64(dict.entries))
	} else {
		h = append(h, 0)
	}
	misc := uint8(nlinks) << 1
	if hasVal {
		misc |= 1
	}
	h = append(h, misc)
	if hasVal {
		h = hdrBuf(h, &val)
	}
	for i := 0; i < nlinks; i++ {
		h = hdrBuf(h, &links[i])
	}
	w.hdr = h
	pre := putUvarint(w.pre[:0], uint64(len(h)))
	w.pre = pre
	r.len = uint32(len(pre) + len(h) + len(region))
	for _, p := range [][]byte{pre, h, region} {
		if _, err := w.sink.write(p); err != nil {
			return r, err
		}
	}
	return r, nil
}

// hdrBuf is a run-header buffer descriptor: flags (encoding, raw, nulls; no zone), length,
// [def length, nulls].
func hdrBuf(h []byte, b *buf) []byte {
	f := b.enc
	if b.raw {
		f |= 0x08
	}
	if b.nulls > 0 {
		f |= 0x10
	}
	h = append(h, f)
	h = putUvarint(h, uint64(b.len))
	if b.nulls > 0 {
		h = putUvarint(h, uint64(b.defLen))
		h = putUvarint(h, uint64(b.nulls))
	}
	return h
}

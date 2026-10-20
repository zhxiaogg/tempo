package cgfile

import (
	"fmt"
	"sort"
)

// Encoding ids of a buffer's value section.
const (
	EncPlain       = 0
	EncRLEDict     = 1
	EncDeltaBinary = 2
	EncDeltaLength = 3
)

// Buffer flag bits.
const (
	flagEncMask = 0x07
	flagRaw     = 0x08
	flagNulls   = 0x10
	zoneShift   = 5
)

// ZoneKind is the kind of a page zone.
type ZoneKind uint8

const (
	ZoneNone ZoneKind = iota
	ZoneInt
	ZoneBytes
)

// Zone is the min/max of a buffer's non-null values.
type Zone struct {
	Kind     ZoneKind
	Min, Max int64  // ZoneInt
	BMin     []byte // ZoneBytes
	BMax     []byte
}

// Buf locates one buffer: def levels (DefLen bytes, iff Nulls > 0) then the value section.
type Buf struct {
	Off    int64 // absolute file offset
	Len    uint32
	DefLen uint32
	Nulls  uint32
	Flags  uint8
	Zone   Zone
}

func (b *Buf) Encoding() uint8 { return b.Flags & flagEncMask }
func (b *Buf) Raw() bool       { return b.Flags&flagRaw != 0 }
func (b *Buf) HasNulls() bool  { return b.Flags&flagNulls != 0 }

// DictDesc locates a zstd-compressed PLAIN dictionary page.
type DictDesc struct {
	Off     int64
	Len     uint32
	Entries uint32
}

// Present reports whether the column has a dictionary in this chunk.
func (d DictDesc) Present() bool { return d.Len > 0 }

// Page is one page of a group: one buffer per group column, all covering the same rows.
type Page struct {
	First uint32 // file row of the first row
	Rows  uint32
	Chunk int32 // index of the page's chunk in its group
	Bufs  []Buf // group column order
}

// Chunk is one group chunk (a flush of a group): a contiguous region holding its
// dictionaries then its pages.
type Chunk struct {
	Off   int64
	Len   int64
	Dicts []DictDesc // per group column; zero = no dictionary
	Pages []Page     // sub-slice of Group.Pages
}

// Region is the chunk's byte region.
func (c *Chunk) Region() Region { return Region{Off: c.Off, End: c.Off + c.Len} }

// Group is a decoded group section: every chunk of a group of a table.
type Group struct {
	Table, Index int
	Cols         []int
	Chunks       []Chunk
	Pages        []Page // every page of the group in row order (all chunks)
}

// PageOf returns the page index holding file row r, or -1.
func (g *Group) PageOf(r uint32) int {
	i := sort.Search(len(g.Pages), func(i int) bool { return g.Pages[i].First+g.Pages[i].Rows > r })
	if i < len(g.Pages) && g.Pages[i].First <= r {
		return i
	}
	return -1
}

// DecodeGroup parses the section of group gi of table t.
func (f *Footer) DecodeGroup(t, gi int) (*Group, error) {
	if t < 0 || t >= NumTables || gi < 0 || gi >= len(f.Groups[t]) {
		return nil, fmt.Errorf("cgfile: no group %d in table %d", gi, t)
	}
	g, err := parseGroup(f.gsec[t][gi], len(f.Groups[t][gi].Cols))
	if err != nil {
		return nil, fmt.Errorf("cgfile: %s group %q: %w", TableName(t), f.Groups[t][gi].Name, err)
	}
	g.Table, g.Index, g.Cols = t, gi, f.Groups[t][gi].Cols
	return g, nil
}

func parseGroup(sec []byte, ncol int) (*Group, error) {
	S, err := sectionStreams(sec)
	if err != nil {
		return nil, err
	}
	nflush := S[sHead].uvarint()
	if nflush > uint64(len(sec)) {
		return nil, errShort
	}
	// Page and buffer counts are bounded by their stream sizes: one flag byte per buffer.
	nbufs := len(S[sBFlag].b)
	g := &Group{Chunks: make([]Chunk, nflush)}
	if ncol > 0 {
		g.Pages = make([]Page, 0, nbufs/ncol)
	}
	bufs := make([]Buf, nbufs)
	dicts := make([]DictDesc, int(nflush)*ncol)
	var first uint32
	for fi := range g.Chunks {
		off := int64(S[sRegion].uvarint())
		ch := &g.Chunks[fi]
		ch.Off = off
		ch.Len = int64(S[sRegion].uvarint())
		ch.Dicts = dicts[fi*ncol : (fi+1)*ncol : (fi+1)*ncol]
		for k := 0; k < ncol; k++ {
			if S[sDict].u8() != 0 {
				d := DictDesc{Off: off, Len: S[sDict].u32(), Entries: S[sDict].u32()}
				ch.Dicts[k] = d
				off += int64(d.Len)
			}
		}
		p0 := len(g.Pages)
		npages := S[sPRows].uvarint()
		for p := uint64(0); p < npages; p++ {
			rows := S[sPRows].u32()
			if len(bufs) < ncol {
				return nil, errShort
			}
			pb := bufs[:ncol:ncol]
			bufs = bufs[ncol:]
			for k := range pb {
				readBuf(&S, &pb[k], &off)
			}
			g.Pages = append(g.Pages, Page{First: first, Rows: rows, Chunk: int32(fi), Bufs: pb})
			first += rows
		}
		ch.Pages = g.Pages[p0:len(g.Pages):len(g.Pages)]
		if err := streamsFirstErr(&S); err != nil {
			return nil, err
		}
		if off != ch.Off+ch.Len {
			return nil, fmt.Errorf("chunk %d: buffers end at %d, chunk ends at %d", fi, off, ch.Off+ch.Len)
		}
	}
	if err := streamsErr(&S); err != nil {
		return nil, err
	}
	return g, nil
}

func readBuf(S *[numStreams]cursor, b *Buf, off *int64) {
	b.Flags = S[sBFlag].u8()
	b.Len = S[sBLen].u32()
	b.Off = *off
	*off += int64(b.Len)
	if b.HasNulls() {
		b.DefLen = S[sBNull].u32()
		b.Nulls = S[sBNull].u32()
	}
	switch ZoneKind((b.Flags >> zoneShift) & 3) {
	case ZoneNone:
	case ZoneInt:
		mn := S[sZone].svarint()
		b.Zone = Zone{Kind: ZoneInt, Min: mn, Max: mn + S[sZone].svarint()}
	case ZoneBytes:
		b.Zone = Zone{Kind: ZoneBytes, BMin: S[sZone].bytes(), BMax: S[sZone].bytes()}
	default:
		S[sZone].fail(fmt.Errorf("bad zone kind in flags %#x", b.Flags))
	}
}

func streamsFirstErr(s *[numStreams]cursor) error {
	for i := range s {
		if s[i].err != nil {
			return s[i].err
		}
	}
	return nil
}

// Run is one (key, value type) run of a key-major flush.
type Run struct {
	Key   uint32 // index into Footer.Keys[level]
	VType uint8
	VCol  uint8  // VColNone = no value buffer
	First uint32 // first entry of the run within its flush
	Rows  uint32
	Off   int64
	Len   uint32
	Zone  Zone // ZoneInt for int runs, else ZoneNone
}

// KmFlush is one key-major flush: the entries of owner rows [Owner0, Owner0+Owners).
type KmFlush struct {
	Owner0, Owners uint32
	Off, Len       int64
	Runs           []Run // sorted by (key, vtype)
}

// KeyRuns returns the index range [a, b) of the flush's runs of key id.
func (k *KmFlush) KeyRuns(key uint32) (int, int) {
	a := sort.Search(len(k.Runs), func(i int) bool { return k.Runs[i].Key >= key })
	b := a
	for b < len(k.Runs) && k.Runs[b].Key == key {
		b++
	}
	return a, b
}

// KeyRegion is the byte region of every run of key id in the flush (they are adjacent).
func (k *KmFlush) KeyRegion(key uint32) Region {
	a, b := k.KeyRuns(key)
	if a == b {
		return Region{}
	}
	return Region{Off: k.Runs[a].Off, End: k.Runs[b-1].Off + int64(k.Runs[b-1].Len)}
}

// DecodeKm parses the key-major section of level l (nil if the level has none).
func (f *Footer) DecodeKm(l int) ([]KmFlush, error) {
	if l < 0 || l >= len(f.ksec) {
		return nil, nil
	}
	out, err := parseKm(f.ksec[l])
	if err != nil {
		return nil, fmt.Errorf("cgfile: key-major level %d: %w", l, err)
	}
	return out, nil
}

func parseKm(sec []byte) ([]KmFlush, error) {
	S, err := sectionStreams(sec)
	if err != nil {
		return nil, err
	}
	nflush := S[sHead].uvarint()
	if nflush > uint64(len(sec)) {
		return nil, errShort
	}
	out := make([]KmFlush, nflush)
	runs := make([]Run, 0, len(S[sRMisc].b)) // one misc byte per run
	for fi := range out {
		k := &out[fi]
		k.Owner0 = S[sHead].u32()
		k.Owners = S[sHead].u32()
		off := int64(S[sRegion].uvarint())
		k.Off = off
		k.Len = int64(S[sRegion].uvarint())
		nruns := S[sRRows].uvarint()
		var key, first uint32
		r0 := len(runs)
		for r := uint64(0); r < nruns; r++ {
			key += S[sRKey].u32()
			run := Run{Key: key, VType: S[sRVT].u8(), VCol: S[sRVT].u8(), First: first, Rows: S[sRRows].u32(), Off: off}
			run.Len = S[sBLen].u32()
			if S[sRMisc].u8() != 0 {
				mn := S[sZone].svarint()
				run.Zone = Zone{Kind: ZoneInt, Min: mn, Max: mn + S[sZone].svarint()}
			}
			if err := streamsFirstErr(&S); err != nil {
				return nil, err
			}
			runs = append(runs, run)
			off += int64(run.Len)
			first += run.Rows
		}
		k.Runs = runs[r0:len(runs):len(runs)]
		if off != k.Off+k.Len {
			return nil, fmt.Errorf("flush %d: runs end at %d, flush ends at %d", fi, off, k.Off+k.Len)
		}
	}
	if err := streamsErr(&S); err != nil {
		return nil, err
	}
	return out, nil
}

package cgfile

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/klauspost/compress/zstd"
)

const (
	// Magic starts the file and ends the trailer.
	Magic = "TCG1"
	// TrailerLen is the size of the trailer at the end of the file.
	TrailerLen = 10

	fileVersion   = 4
	footerVariant = 1
	numStreams    = 12
)

// Trailer is the last TrailerLen bytes of the file.
type Trailer struct {
	FooterLen uint32 // compressed footer bytes, right before the trailer
	Variant   uint8
	Version   uint8
}

// ParseTrailer validates and decodes the trailer.
func ParseTrailer(b []byte) (Trailer, error) {
	if len(b) != TrailerLen {
		return Trailer{}, fmt.Errorf("cgfile: trailer is %d bytes, want %d", len(b), TrailerLen)
	}
	if string(b[6:10]) != Magic {
		return Trailer{}, errors.New("cgfile: bad trailer magic")
	}
	t := Trailer{FooterLen: binary.LittleEndian.Uint32(b[0:4]), Variant: b[4], Version: b[5]}
	if t.Version != fileVersion {
		return t, fmt.Errorf("cgfile: unsupported version %d (want %d: rewrite the block)", t.Version, fileVersion)
	}
	if t.Variant != footerVariant {
		return t, fmt.Errorf("cgfile: unsupported footer variant %d", t.Variant)
	}
	return t, nil
}

// GroupDef is one column group of a table: its column ids in page order.
type GroupDef struct {
	Name string
	Cols []int
}

type colLoc struct{ gi, k int32 }

// Footer is the decoded top level of the footer. Group and key-major sections are kept as
// bytes and decoded on demand (DecodeGroup, DecodeKm). A Footer is immutable.
//
// The point-copy tables (AttrsTable, ElemsTable) have one group each: a row-major attrs
// group's entry and element streams.
type Footer struct {
	Schema *FileSchema
	Groups [NumTables][]GroupDef
	Keys   [][]string // per level, sorted bytewise
	Rows   [NumTables]uint32
	// Anchors are the window cuts: per page of the traces group holding first_span, the first
	// row of every tracing table (t < 7) at its first trace, from the traces.first_* zones.
	Anchors [][NumTables]uint32

	gsec [NumTables][][]byte
	ksec [][]byte
	loc  [NumTables][]colLoc // column id -> group, position; gi = -1 if not in any group

	zonesOnce sync.Once
	zones     [NumLevels][2][]pageSpan // per level: entry pages by owner, element pages by entry
	zonesErr  error
}

// DecodeFooter decompresses and parses the footer bytes (without the trailer).
func DecodeFooter(dec *zstd.Decoder, compressed []byte) (*Footer, error) {
	raw, err := dec.DecodeAll(compressed, nil)
	if err != nil {
		return nil, fmt.Errorf("cgfile: footer zstd: %w", err)
	}
	return parseFooter(raw)
}

func parseFooter(raw []byte) (*Footer, error) {
	c := &cursor{b: raw}
	s, err := decodeFileSchema(c, len(raw))
	if err != nil {
		return nil, err
	}
	m, err := mapSchema(s)
	if err != nil {
		return nil, err
	}
	f := &Footer{Schema: s}
	nk := c.uvarint()
	if c.err == nil && nk != NumLevels {
		return nil, fmt.Errorf("cgfile: footer has %d key dictionaries, want %d", nk, NumLevels)
	}
	f.Keys = make([][]string, NumLevels)
	for l := range f.Keys {
		n := c.uvarint()
		if n > uint64(len(raw)) {
			return nil, errShort
		}
		keys := make([]string, n)
		for i := range keys {
			keys[i] = string(c.bytes())
		}
		f.Keys[l] = keys
	}
	for t := 0; t < 7; t++ {
		f.Rows[t] = c.u32()
	}
	var sec [7][][]byte
	for t := 0; t < 7; t++ {
		sec[t] = make([][]byte, len(s.Tables[t].Groups))
		for gi := range sec[t] {
			sec[t][gi] = c.bytes()
		}
	}
	if c.err != nil {
		return nil, fmt.Errorf("cgfile: footer: %w", c.err)
	}
	if !c.done() {
		return nil, fmt.Errorf("cgfile: footer has %d trailing bytes", len(raw)-c.p)
	}
	f.ksec = make([][]byte, NumLevels)
	for t := 0; t < 7; t++ {
		f.Groups[t] = m.groups[t]
		for _, gi := range m.sec[t] {
			f.gsec[t] = append(f.gsec[t], sec[t][gi])
		}
	}
	for l := 0; l < NumLevels; l++ {
		t := OwnerTable[l]
		rc := &cursor{b: sec[t][m.rm[l]]}
		ent, el := rc.bytes(), rc.bytes()
		if rc.err != nil || !rc.done() {
			return nil, fmt.Errorf("cgfile: %s attrs section: entry and element streams expected", TableName(t))
		}
		for _, x := range []struct {
			t   int
			sec []byte
		}{{AttrsTable(l), ent}, {ElemsTable(l), el}} {
			cols := make([]int, len(Cols(x.t)))
			for i := range cols {
				cols[i] = i
			}
			f.Groups[x.t] = []GroupDef{{Name: s.Tables[t].Groups[m.rm[l]].Name, Cols: cols}}
			f.gsec[x.t] = [][]byte{x.sec}
		}
		f.ksec[l] = sec[t][m.km[l]]
	}
	for t := 0; t < NumTables; t++ {
		loc := make([]colLoc, len(Cols(t)))
		for i := range loc {
			loc[i] = colLoc{-1, -1}
		}
		for gi, g := range f.Groups[t] {
			for k, cid := range g.Cols {
				loc[cid] = colLoc{int32(gi), int32(k)}
			}
		}
		f.loc[t] = loc
	}
	if err := f.countRows(); err != nil {
		return nil, err
	}
	if err := f.cutAnchors(); err != nil {
		return nil, err
	}
	return f, nil
}

// countRows sets the rows of the point-copy and key-major tables from their sections (the
// footer stores the rows of the tracing tables only).
func (f *Footer) countRows() error {
	for l := 0; l < NumLevels; l++ {
		for _, t := range []int{AttrsTable(l), ElemsTable(l)} {
			g, err := f.DecodeGroup(t, 0)
			if err != nil {
				return err
			}
			if n := len(g.Pages); n > 0 {
				f.Rows[t] = g.Pages[n-1].First + g.Pages[n-1].Rows
			}
		}
		km, err := f.DecodeKm(l)
		if err != nil {
			return err
		}
		for _, fl := range km {
			for _, r := range fl.Runs {
				f.Rows[KmTable(l)] += r.Rows
			}
		}
	}
	return nil
}

// traceFirst is the traces column holding each trace's first row of tracing table t.
var traceFirst = [7]int{TTraces: -1, TServiceStats: TrFirstStat, TResources: TrFirstResource, TScopes: TrFirstScope, TSpans: TrFirstSpan, TEvents: TrFirstEvent, TLinks: TrFirstLink}

// zoneMins returns (first row, zone minimum) of every page of traces column c's group.
func (f *Footer) zoneMins(c int) ([][2]uint32, error) {
	gi, k, ok := f.ColumnGroup(TTraces, c)
	if !ok {
		return nil, fmt.Errorf("cgfile: traces.%s is in no group", Cols(TTraces)[c].Name)
	}
	g, err := f.DecodeGroup(TTraces, gi)
	if err != nil {
		return nil, err
	}
	out := make([][2]uint32, len(g.Pages))
	for i := range g.Pages {
		p := &g.Pages[i]
		z := p.Bufs[k].Zone
		if z.Kind != ZoneInt || z.Min < 0 || z.Min > 1<<32-1 {
			return nil, fmt.Errorf("cgfile: traces.%s: the page at row %d has no row zone", Cols(TTraces)[c].Name, p.First)
		}
		out[i] = [2]uint32{p.First, uint32(z.Min)}
	}
	return out, nil
}

// cutAnchors derives the window cuts (tempo-rs cg_trace::windows::cuts): the pages of
// first_span's group start at cut traces, and every traces.first_* column's pages start there
// too, so at a cut the zone minimum of each is the first row of its table.
func (f *Footer) cutAnchors() error {
	cut, err := f.zoneMins(TrFirstSpan)
	if err != nil {
		return err
	}
	f.Anchors = make([][NumTables]uint32, len(cut))
	for i, p := range cut {
		f.Anchors[i][TTraces] = p[0]
	}
	for t := 1; t < 7; t++ {
		zs := cut
		if traceFirst[t] != TrFirstSpan {
			if zs, err = f.zoneMins(traceFirst[t]); err != nil {
				return err
			}
		}
		if len(zs) != len(cut) {
			return fmt.Errorf("cgfile: traces.%s: %d pages, first_span has %d", Cols(TTraces)[traceFirst[t]].Name, len(zs), len(cut))
		}
		for i, z := range zs {
			if z[0] != cut[i][0] {
				return fmt.Errorf("cgfile: traces.%s: pages do not start with first_span's", Cols(TTraces)[traceFirst[t]].Name)
			}
			f.Anchors[i][t] = z[1]
		}
	}
	return nil
}

// pageSpan is one page of a point-copy table: its rows and the zone of its sorted
// row-reference column (owner rows, or entry rows for elements).
type pageSpan struct{ first, end, min, max uint32 }

func (f *Footer) attrZones() error {
	f.zonesOnce.Do(func() {
		for l := 0; l < NumLevels; l++ {
			for i, x := range [2][2]int{{AttrsTable(l), AtOwner}, {ElemsTable(l), ElAttrRow}} {
				g, err := f.DecodeGroup(x[0], 0)
				if err != nil {
					f.zonesErr = err
					return
				}
				_, k, _ := f.ColumnGroup(x[0], x[1])
				ps := make([]pageSpan, len(g.Pages))
				for j := range g.Pages {
					p := &g.Pages[j]
					z := p.Bufs[k].Zone
					if z.Kind != ZoneInt {
						f.zonesErr = fmt.Errorf("cgfile: %s page %d has no row zone", TableName(x[0]), j)
						return
					}
					ps[j] = pageSpan{p.First, p.First + p.Rows, uint32(z.Min), uint32(z.Max)}
				}
				f.zones[l][i] = ps
			}
		}
	})
	return f.zonesErr
}

// hull is the rows of the pages whose zone meets [lo, hi): a superset of the rows referencing
// it (the column is sorted, so the pages are consecutive), and the first row of the last such
// page (rows before it reference only values below hi).
func hull(ps []pageSpan, lo, hi uint32) (a, b, keep uint32) {
	i := sort.Search(len(ps), func(i int) bool { return ps[i].max >= lo })
	j := sort.Search(len(ps), func(i int) bool { return ps[i].min >= hi })
	if lo >= hi || i >= j {
		if i < len(ps) {
			return ps[i].first, ps[i].first, ps[i].first
		}
		if n := len(ps); n > 0 {
			return ps[n-1].end, ps[n-1].end, ps[n-1].end
		}
		return 0, 0, 0
	}
	return ps[i].first, ps[j-1].end, ps[j-1].first
}

// WindowRows returns the rows [lo, hi) of table t in the window between anchors a and b, and
// keep: rows before it are of no later window. For the tracing tables they are exact; for the
// point-copy tables they are a superset (the pages whose owner zones meet the window's owners),
// so a reader filters entries by owner. Key-major tables have none (their runs are located by
// owner).
func (f *Footer) WindowRows(a, b, t int) (lo, hi, keep uint32, err error) {
	if t < 7 {
		lo, hi = f.Anchor(a, t), f.Anchor(b, t)
		return lo, hi, hi, nil
	}
	l, kind := (t-7)/3, (t-7)%3
	if kind == 2 {
		return 0, 0, 0, nil
	}
	if err := f.attrZones(); err != nil {
		return 0, 0, 0, err
	}
	o := OwnerTable[l]
	lo, hi, keep = hull(f.zones[l][0], f.Anchor(a, o), f.Anchor(b, o))
	if kind == 1 {
		lo, hi, keep = hull(f.zones[l][1], lo, hi)
	}
	return lo, hi, keep, nil
}

// ColumnGroup returns the group of column c of table t and the column's position in it.
func (f *Footer) ColumnGroup(t, c int) (gi, k int, ok bool) {
	if t < 0 || t >= NumTables || c < 0 || c >= len(f.loc[t]) {
		return 0, 0, false
	}
	l := f.loc[t][c]
	return int(l.gi), int(l.k), l.gi >= 0
}

// Anchor returns anchor i of tracing table t (t < 7); anchor len(Anchors) is the table's row
// count. The point-copy tables have no anchors: see WindowRows.
func (f *Footer) Anchor(i, t int) uint32 {
	if i >= len(f.Anchors) {
		return f.Rows[t]
	}
	return f.Anchors[i][t]
}

// KeyID returns the id of key in level l's key dictionary.
func (f *Footer) KeyID(l int, key string) (uint32, bool) {
	if l < 0 || l >= len(f.Keys) {
		return 0, false
	}
	keys := f.Keys[l]
	i := sort.SearchStrings(keys, key)
	if i < len(keys) && keys[i] == key {
		return uint32(i), true
	}
	return 0, false
}

// sectionStreams splits a section into its 12 column-wise streams.
func sectionStreams(b []byte) ([numStreams]cursor, error) {
	var s [numStreams]cursor
	c := &cursor{b: b}
	if n := c.uvarint(); c.err == nil && n != numStreams {
		return s, fmt.Errorf("cgfile: section has %d streams, want %d", n, numStreams)
	}
	var lens [numStreams]uint64
	for i := range lens {
		lens[i] = c.uvarint()
	}
	if c.err != nil {
		return s, c.err
	}
	p := c.p
	for i, l := range lens {
		if l > uint64(len(b)-p) {
			return s, errShort
		}
		s[i] = cursor{b: b[p : p+int(l)]}
		p += int(l)
	}
	if p != len(b) {
		return s, fmt.Errorf("cgfile: section has %d trailing bytes", len(b)-p)
	}
	return s, nil
}

// Stream ids.
const (
	sHead = iota
	sRegion
	sDict
	sPRows
	sBFlag
	sBLen
	sBNull
	sZone
	sRKey
	sRVT
	sRRows
	sRMisc
)

func streamsErr(s *[numStreams]cursor) error {
	for i := range s {
		if s[i].err != nil {
			return s[i].err
		}
		if !s[i].done() {
			return fmt.Errorf("cgfile: section stream %d has %d trailing bytes", i, len(s[i].b)-s[i].p)
		}
	}
	return nil
}

package cgfile

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/klauspost/compress/zstd"
)

const (
	// Magic starts the file and ends the trailer.
	Magic = "TCG1"
	// TrailerLen is the size of the trailer at the end of the file.
	TrailerLen = 10

	fileVersion   = 2
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
		return t, fmt.Errorf("cgfile: unsupported version %d", t.Version)
	}
	if t.Variant != footerVariant {
		return t, fmt.Errorf("cgfile: unsupported footer variant %d", t.Variant)
	}
	return t, nil
}

// BlockMeta is the footer's JSON block metadata.
type BlockMeta struct {
	StartNs     uint64            `json:"start_ns"`
	EndNs       uint64            `json:"end_ns"`
	TotalTraces uint64            `json:"total_traces"`
	Dedicated   []DedicatedColumn `json:"dedicated"`
}

// DedicatedColumn is Tempo's dedicated-column config, carried as information only.
type DedicatedColumn struct {
	Scope string `json:"scope"`
	Name  string `json:"name"`
	IsInt bool   `json:"is_int"`
}

// GroupDef is one column group of a table: its column ids in page order.
type GroupDef struct {
	Name string
	Cols []int
}

type colLoc struct{ gi, k int32 }

// Footer is the decoded top level of the footer. Group and key-major sections are kept as
// bytes and decoded on demand (DecodeGroup, DecodeKm). A Footer is immutable.
type Footer struct {
	Meta    BlockMeta
	Groups  [NumTables][]GroupDef
	Keys    [][]string // per level, sorted bytewise
	Rows    [NumTables]uint32
	Anchors [][NumTables]uint32

	gsec [NumTables][][]byte
	ksec [][]byte
	loc  [NumTables][]colLoc // column id -> group, position; gi = -1 if not in any group
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
	f := &Footer{}
	meta := c.bytes()
	if c.err == nil {
		if err := json.Unmarshal(meta, &f.Meta); err != nil {
			return nil, fmt.Errorf("cgfile: footer meta: %w", err)
		}
	}
	if nt := c.uvarint(); c.err == nil && nt != NumTables {
		return nil, fmt.Errorf("cgfile: footer has %d tables, want %d", nt, NumTables)
	}
	for t := 0; t < NumTables && c.err == nil; t++ {
		ng := c.uvarint()
		for g := uint64(0); g < ng && c.err == nil; g++ {
			name := string(c.bytes())
			nc := c.uvarint()
			if nc > uint64(len(raw)) {
				return nil, errShort
			}
			cols := make([]int, nc)
			for i := range cols {
				cols[i] = int(c.uvarint())
				if cols[i] >= len(Cols(t)) {
					return nil, fmt.Errorf("cgfile: %s group %q: column %d out of range", TableName(t), name, cols[i])
				}
			}
			f.Groups[t] = append(f.Groups[t], GroupDef{Name: name, Cols: cols})
		}
	}
	nl := c.uvarint()
	if nl > uint64(len(raw)) {
		return nil, errShort
	}
	f.Keys = make([][]string, nl)
	for l := range f.Keys {
		nk := c.uvarint()
		if nk > uint64(len(raw)) {
			return nil, errShort
		}
		keys := make([]string, nk)
		for i := range keys {
			keys[i] = string(c.bytes())
		}
		f.Keys[l] = keys
	}
	for t := range f.Rows {
		f.Rows[t] = c.u32()
	}
	na := c.uvarint()
	if na > uint64(len(raw)) {
		return nil, errShort
	}
	f.Anchors = make([][NumTables]uint32, na)
	for t := 0; t < NumTables; t++ {
		var last uint32
		for i := range f.Anchors {
			last += c.u32()
			f.Anchors[i][t] = last
		}
	}
	for t := 0; t < NumTables; t++ {
		f.gsec[t] = make([][]byte, len(f.Groups[t]))
		for gi := range f.gsec[t] {
			f.gsec[t][gi] = c.bytes()
		}
	}
	nks := c.uvarint()
	if nks > uint64(len(raw)) {
		return nil, errShort
	}
	f.ksec = make([][]byte, nks)
	for l := range f.ksec {
		f.ksec[l] = c.bytes()
	}
	if c.err != nil {
		return nil, fmt.Errorf("cgfile: footer: %w", c.err)
	}
	if !c.done() {
		return nil, fmt.Errorf("cgfile: footer has %d trailing bytes", len(raw)-c.p)
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
	return f, nil
}

// ColumnGroup returns the group of column c of table t and the column's position in it.
func (f *Footer) ColumnGroup(t, c int) (gi, k int, ok bool) {
	if t < 0 || t >= NumTables || c < 0 || c >= len(f.loc[t]) {
		return 0, 0, false
	}
	l := f.loc[t][c]
	return int(l.gi), int(l.k), l.gi >= 0
}

// Anchor returns anchor i of table t; anchor len(Anchors) is the table's row count.
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

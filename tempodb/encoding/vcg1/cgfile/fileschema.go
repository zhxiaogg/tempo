package cgfile

import (
	"errors"
	"fmt"
)

// The footer's schema (version 4): tables, their columns and the column groups that store them,
// as tempo-rs fmt-cg encodes them. The reader maps it onto the fixed tracing tables of this
// package by name (mapSchema).

// Column encodings recorded in the schema (fmt-cg Enc).
const (
	ColEncDictStr     = 0
	ColEncRawBytes    = 1
	ColEncDeltaInt    = 2
	ColEncPlainInt    = 3
	ColEncPlainDouble = 4
	ColEncPlainBool   = 5
	ColEncDictInt     = 6
)

// Group encodings.
const (
	GroupStructured   = 0
	GroupSemiRowMajor = 1
	GroupSemiKeyMajor = 2
)

// FileCol is one column of a schema table: scalar, or semi-structured (attrs).
type FileCol struct {
	Name     string
	Semi     bool
	Phys     Phys
	Enc      uint8
	Nullable bool
	Zone     bool
}

// FileGroup is one column group of a schema table. Optional sizes are -1 when unset.
type FileGroup struct {
	Name                         string
	Cols                         []int
	Enc                          uint8
	Carry                        []int // key-major: the carried int columns
	PageBytes, ChunkBytes, Flush int
	CutWith                      int
}

// FileTable is one table of a schema.
type FileTable struct {
	Name   string
	Cols   []FileCol
	Groups []FileGroup
}

// FileSchema is the footer's schema.
type FileSchema struct {
	Tables []FileTable
}

func putOpt(out []byte, v int) []byte {
	if v < 0 {
		return putUvarint(out, 0)
	}
	return putUvarint(out, uint64(v)+1)
}

func (c *cursor) opt() int { return int(c.uvarint()) - 1 }

// Append encodes the schema.
func (s *FileSchema) Append(out []byte) []byte {
	out = putUvarint(out, uint64(len(s.Tables)))
	for _, t := range s.Tables {
		out = putBytes(out, []byte(t.Name))
		out = putUvarint(out, uint64(len(t.Cols)))
		for _, c := range t.Cols {
			out = putBytes(out, []byte(c.Name))
			if c.Semi {
				out = append(out, 1)
				continue
			}
			f := byte(0)
			if c.Nullable {
				f |= 1
			}
			if c.Zone {
				f |= 2
			}
			out = append(out, 0, byte(c.Phys), c.Enc, f)
		}
		out = putUvarint(out, uint64(len(t.Groups)))
		for _, g := range t.Groups {
			out = putBytes(out, []byte(g.Name))
			out = putUvarint(out, uint64(len(g.Cols)))
			for _, c := range g.Cols {
				out = putUvarint(out, uint64(c))
			}
			out = append(out, g.Enc)
			if g.Enc == GroupSemiKeyMajor {
				out = putUvarint(out, uint64(len(g.Carry)))
				for _, c := range g.Carry {
					out = putUvarint(out, uint64(c))
				}
			}
			out = putOpt(out, g.PageBytes)
			out = putOpt(out, g.ChunkBytes)
			out = putOpt(out, g.Flush)
			out = putOpt(out, g.CutWith)
		}
	}
	return out
}

func decodeFileSchema(c *cursor, limit int) (*FileSchema, error) {
	n := func() int {
		v := c.uvarint()
		if v > uint64(limit) {
			c.fail(errShort)
			return 0
		}
		return int(v)
	}
	s := &FileSchema{Tables: make([]FileTable, n())}
	for ti := range s.Tables {
		t := &s.Tables[ti]
		t.Name = string(c.bytes())
		t.Cols = make([]FileCol, n())
		for ci := range t.Cols {
			col := &t.Cols[ci]
			col.Name = string(c.bytes())
			switch k := c.u8(); k {
			case 0:
				col.Phys, col.Enc = Phys(c.u8()), c.u8()
				f := c.u8()
				col.Nullable, col.Zone = f&1 != 0, f&2 != 0
				if col.Phys > PhysBytes || col.Enc > ColEncDictInt {
					return nil, fmt.Errorf("cgfile: schema column %s.%s: bad type %d / encoding %d", t.Name, col.Name, col.Phys, col.Enc)
				}
			case 1:
				col.Semi = true
			default:
				if c.err == nil {
					return nil, fmt.Errorf("cgfile: schema column %s.%s: bad kind %d", t.Name, col.Name, k)
				}
			}
		}
		t.Groups = make([]FileGroup, n())
		for gi := range t.Groups {
			g := &t.Groups[gi]
			g.Name = string(c.bytes())
			g.Cols = make([]int, n())
			for i := range g.Cols {
				g.Cols[i] = n()
			}
			g.Enc = c.u8()
			switch g.Enc {
			case GroupStructured, GroupSemiRowMajor:
			case GroupSemiKeyMajor:
				for k := n(); k > 0; k-- {
					g.Carry = append(g.Carry, n())
				}
			default:
				if c.err == nil {
					return nil, fmt.Errorf("cgfile: schema group %s.%s: bad encoding %d", t.Name, g.Name, g.Enc)
				}
			}
			g.PageBytes, g.ChunkBytes, g.Flush, g.CutWith = c.opt(), c.opt(), c.opt(), c.opt()
			for _, x := range append(append([]int(nil), g.Cols...), g.Carry...) {
				if x >= len(t.Cols) && c.err == nil {
					return nil, fmt.Errorf("cgfile: schema group %s.%s: no column %d", t.Name, g.Name, x)
				}
			}
		}
	}
	if c.err != nil {
		return nil, c.err
	}
	return s, nil
}

// The tracing schema's carry-only columns and semi-structured column, which this package's
// tables do not hold as stored columns.
const (
	colSpanCount = "span_count"
	colSpan      = "span"
	colAttrs     = "attrs"
)

// carryCols are the columns the key-major attrs group of each level carries (run links).
var carryCols = [NumLevels][]string{{"first_span", colSpanCount}, {"first_span", colSpanCount}, {}, {colSpan}, {colSpan}}

// rmPageBytes is the page size the tracing schema records for the row-major attrs groups.
const rmPageBytes = 4 << 20

// fileColEnc maps a stored column's encoding class to the schema's encoding id.
func fileColEnc(t, c int) uint8 {
	switch t {
	case TTraces:
		switch c {
		case TrTraceID:
			return ColEncRawBytes
		case TrRootService, TrRootSpan:
			return ColEncDictStr
		}
	case TServiceStats:
		if c == SsServiceName {
			return ColEncDictStr
		}
	case TResources:
		if c == ResServiceName {
			return ColEncDictStr
		}
	case TScopes:
		if c == ScName || c == ScVersion {
			return ColEncDictStr
		}
	case TSpans:
		switch c {
		case SpSpanID, SpParentSpanID, SpTraceState, SpStatusMessage:
			return ColEncRawBytes
		case SpName:
			return ColEncDictStr
		}
	case TEvents:
		if c == EvName {
			return ColEncDictStr
		}
	case TLinks:
		switch c {
		case LnTraceID, LnSpanID, LnTraceState:
			return ColEncRawBytes
		}
	}
	return ColEncDeltaInt
}

// SchemaGroup is a structured group of one of the 7 tracing tables: a name, its column ids
// (this package's) and the group it cuts its pages with (-1: by size).
type SchemaGroup struct {
	Name    string
	Cols    []int
	CutWith int
}

// TracingSchema returns the footer schema of a file whose tracing tables are stored by groups
// (per table t < 7; this package's column ids), plus the row-major and key-major attrs groups
// of every owner table: the schema tempo-rs cg-trace writes.
func TracingSchema(groups [7][]SchemaGroup) *FileSchema {
	s := &FileSchema{}
	for t := 0; t < 7; t++ {
		ft := FileTable{Name: TableName(t)}
		id := map[int]int{} // package column id -> schema column id
		for c, spec := range Cols(t) {
			if !spec.Stored {
				continue
			}
			id[c] = len(ft.Cols)
			ft.Cols = append(ft.Cols, FileCol{Name: spec.Name, Phys: spec.Phys, Enc: fileColEnc(t, c), Nullable: spec.Opt, Zone: spec.Zone})
		}
		byName := func(name string) int {
			for i, c := range ft.Cols {
				if c.Name == name {
					return i
				}
			}
			return -1
		}
		l := levelOf(t)
		if l >= 0 {
			for _, name := range carryCols[l] {
				if byName(name) < 0 {
					ft.Cols = append(ft.Cols, FileCol{Name: name, Phys: PhysInt32, Enc: ColEncDeltaInt})
				}
			}
		}
		for _, g := range groups[t] {
			fg := FileGroup{Name: g.Name, Enc: GroupStructured, PageBytes: -1, ChunkBytes: -1, Flush: -1, CutWith: g.CutWith}
			for _, c := range g.Cols {
				fg.Cols = append(fg.Cols, id[c])
			}
			ft.Groups = append(ft.Groups, fg)
		}
		if l >= 0 {
			a := len(ft.Cols)
			ft.Cols = append(ft.Cols, FileCol{Name: colAttrs, Semi: true})
			ft.Groups = append(ft.Groups, FileGroup{Name: "attrs", Cols: []int{a}, Enc: GroupSemiRowMajor, PageBytes: rmPageBytes, ChunkBytes: -1, Flush: -1, CutWith: -1})
			km := FileGroup{Name: "attrs_km", Cols: []int{a}, Enc: GroupSemiKeyMajor, PageBytes: -1, ChunkBytes: -1, Flush: -1, CutWith: -1}
			for _, name := range carryCols[l] {
				km.Carry = append(km.Carry, byName(name))
			}
			ft.Groups = append(ft.Groups, km)
		}
		s.Tables = append(s.Tables, ft)
	}
	return s
}

// levelOf is the attribute level whose owner table is t, or -1.
func levelOf(t int) int {
	for l, o := range OwnerTable {
		if o == t {
			return l
		}
	}
	return -1
}

// fileMap is how a footer schema maps onto this package's tables.
type fileMap struct {
	// groups[t]: the structured groups of tracing table t, with package column ids.
	groups [7][]GroupDef
	// sec[t][gi]: the schema group index of groups[t][gi].
	sec [7][]int
	// rm[l], km[l]: the schema group indices of level l's attrs groups (-1: none).
	rm, km [NumLevels]int
}

var errSchema = errors.New("cgfile: footer schema")

// mapSchema resolves the footer schema against the tracing tables by name: every table, and
// every column of a structured group, must be one of this package's with the same physical
// type; carry-only columns (in no structured group) are skipped; each owner table has a
// row-major and a key-major attrs group carrying its span links.
func mapSchema(s *FileSchema) (*fileMap, error) {
	if len(s.Tables) != 7 {
		return nil, fmt.Errorf("%w: %d tables, want 7", errSchema, len(s.Tables))
	}
	m := &fileMap{}
	for t := 0; t < 7; t++ {
		ft := &s.Tables[t]
		if ft.Name != TableName(t) {
			return nil, fmt.Errorf("%w: table %d is %q, want %q", errSchema, t, ft.Name, TableName(t))
		}
		pkg := map[string]int{}
		for c, spec := range Cols(t) {
			if spec.Stored {
				pkg[spec.Name] = c
			}
		}
		l := levelOf(t)
		if l >= 0 {
			m.rm[l], m.km[l] = -1, -1
		}
		for gi, g := range ft.Groups {
			switch g.Enc {
			case GroupStructured:
				gd := GroupDef{Name: g.Name}
				for _, sc := range g.Cols {
					fc := &ft.Cols[sc]
					c, ok := pkg[fc.Name]
					if !ok || fc.Semi || Cols(t)[c].Phys != fc.Phys {
						return nil, fmt.Errorf("%w: %s.%s (group %s) is not a %s column of this reader", errSchema, ft.Name, fc.Name, g.Name, ft.Name)
					}
					gd.Cols = append(gd.Cols, c)
				}
				m.groups[t] = append(m.groups[t], gd)
				m.sec[t] = append(m.sec[t], gi)
			case GroupSemiRowMajor, GroupSemiKeyMajor:
				if l < 0 || len(g.Cols) != 1 || ft.Cols[g.Cols[0]].Name != colAttrs {
					return nil, fmt.Errorf("%w: semi-structured group %s.%s", errSchema, ft.Name, g.Name)
				}
				if g.Enc == GroupSemiRowMajor {
					m.rm[l] = gi
					continue
				}
				if len(g.Carry) != len(carryCols[l]) {
					return nil, fmt.Errorf("%w: %s.%s carries %d columns, want %v", errSchema, ft.Name, g.Name, len(g.Carry), carryCols[l])
				}
				for i, c := range g.Carry {
					if ft.Cols[c].Name != carryCols[l][i] {
						return nil, fmt.Errorf("%w: %s.%s carries %s, want %v", errSchema, ft.Name, g.Name, ft.Cols[c].Name, carryCols[l])
					}
				}
				m.km[l] = gi
			}
		}
		if l >= 0 && (m.rm[l] < 0 || m.km[l] < 0) {
			return nil, fmt.Errorf("%w: %s has no row-major and key-major attrs groups", errSchema, ft.Name)
		}
	}
	return m, nil
}

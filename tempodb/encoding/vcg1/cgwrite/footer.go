package cgwrite

import (
	"encoding/binary"
	"sort"

	cg "github.com/grafana/tempo/v3/tempodb/encoding/vcg1/cgfile"
)

// Section stream ids (cgfile footer layout).
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

// streams is a section's 12 column-wise streams.
type streams [numStreams][]byte

func (s *streams) uv(i int, v uint64) { s[i] = putUvarint(s[i], v) }
func (s *streams) u8(i int, v uint8)  { s[i] = append(s[i], v) }

func (s *streams) zone(z *zone) {
	switch z.kind {
	case cg.ZoneInt:
		s[sZone] = putVarint(s[sZone], z.min)
		s[sZone] = putVarint(s[sZone], z.max-z.min)
	case cg.ZoneBytes:
		s[sZone] = putBytes(s[sZone], z.bmin)
		s[sZone] = putBytes(s[sZone], z.bmax)
	}
}

func (s *streams) buf(b *buf) {
	s.u8(sBFlag, b.flags())
	s.uv(sBLen, uint64(b.len))
	if b.nulls > 0 {
		s.uv(sBNull, uint64(b.defLen))
		s.uv(sBNull, uint64(b.nulls))
	}
	s.zone(&b.zone)
}

func (s *streams) finish(out []byte) []byte {
	out = putUvarint(out, numStreams)
	for i := range s {
		out = putUvarint(out, uint64(len(s[i])))
	}
	for i := range s {
		out = append(out, s[i]...)
	}
	return out
}

func groupSection(chunks []chunkMeta) []byte {
	var s streams
	s.uv(sHead, uint64(len(chunks)))
	for _, c := range chunks {
		s.uv(sRegion, c.off)
		s.uv(sRegion, c.len)
		for _, d := range c.dicts {
			if !d.present {
				s.u8(sDict, 0)
				continue
			}
			s.u8(sDict, 1)
			s.uv(sDict, uint64(d.len))
			s.uv(sDict, uint64(d.entries))
		}
		s.uv(sPRows, uint64(len(c.pages)))
		for i := range c.pages {
			p := &c.pages[i]
			s.uv(sPRows, uint64(p.rows))
			for k := range p.bufs {
				s.buf(&p.bufs[k])
			}
		}
	}
	return s.finish(nil)
}

func kmSection(flushes []kmFlush, rank []uint32) []byte {
	var s streams
	s.uv(sHead, uint64(len(flushes)))
	for _, f := range flushes {
		s.uv(sHead, uint64(f.owner0))
		s.uv(sHead, uint64(f.owners))
		s.uv(sRegion, f.off)
		s.uv(sRegion, f.len)
		s.uv(sRRows, uint64(len(f.runs)))
		last := uint32(0)
		for i := range f.runs {
			r := &f.runs[i]
			key := rank[r.key]
			s.uv(sRKey, uint64(key-last))
			last = key
			s.u8(sRVT, r.vtype)
			s.u8(sRVT, r.vcol)
			s.uv(sRRows, uint64(r.rows))
			s.uv(sBLen, uint64(r.len))
			if r.zone.kind == cg.ZoneInt {
				s.u8(sRMisc, 1)
				s.zone(&r.zone)
			} else {
				s.u8(sRMisc, 0)
			}
		}
	}
	return s.finish(nil)
}

// footer returns the uncompressed footer (version 4): the schema, the key dictionaries of the
// key-major groups, the rows of the tracing tables, then one section per schema group (a
// row-major attrs group: its entry and element sections).
func (w *Writer) footer() ([]byte, error) {
	var groups [7][]cg.SchemaGroup
	for t := 0; t < 7; t++ {
		for _, g := range w.tables[t].groups {
			groups[t] = append(groups[t], cg.SchemaGroup{Name: g.g.name, Cols: g.g.cols, CutWith: g.g.cutWith})
		}
	}
	out := cg.TracingSchema(groups).Append(nil)
	// key dictionaries: every key of the level, sorted; ranks map level key ids to them
	ranks := make([][]uint32, cg.NumLevels)
	out = putUvarint(out, cg.NumLevels)
	for l, k := range w.km {
		ids := make([]uint32, len(k.keys))
		for i := range ids {
			ids[i] = uint32(i)
		}
		sort.Slice(ids, func(a, b int) bool { return k.keys[ids[a]] < k.keys[ids[b]] })
		ranks[l] = make([]uint32, len(ids))
		out = putUvarint(out, uint64(len(ids)))
		for pos, id := range ids {
			ranks[l][id] = uint32(pos)
			out = putBytes(out, []byte(k.keys[id]))
		}
	}
	for t := 0; t < 7; t++ {
		out = putUvarint(out, uint64(w.rows[t]))
	}
	for t := 0; t < 7; t++ {
		for _, g := range w.tables[t].groups {
			out = putBytes(out, groupSection(g.chunks))
		}
		for l, o := range cg.OwnerTable {
			if o != t {
				continue
			}
			rm := putBytes(nil, groupSection(w.tables[cg.AttrsTable(l)].groups[0].chunks))
			rm = putBytes(rm, groupSection(w.tables[cg.ElemsTable(l)].groups[0].chunks))
			out = putBytes(out, rm)
			out = putBytes(out, kmSection(w.km[l].flushes, ranks[l]))
		}
	}
	return out, nil
}

// trailer is footer length u32 LE, footer variant, version, magic.
func trailer(footerLen int) []byte {
	b := binary.LittleEndian.AppendUint32(nil, uint32(footerLen))
	b = append(b, footerVariant, fileVersion)
	return append(b, cg.Magic...)
}

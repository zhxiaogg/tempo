package cgwrite

import (
	"encoding/binary"
	"encoding/json"
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

// footerMeta is the footer's JSON block metadata (cgfile.BlockMeta).
type footerMeta struct {
	StartNs     uint64         `json:"start_ns"`
	EndNs       uint64         `json:"end_ns"`
	TotalTraces uint64         `json:"total_traces"`
	Dedicated   []footerDedCol `json:"dedicated"`
}

type footerDedCol struct {
	Scope string `json:"scope"`
	Name  string `json:"name"`
	IsInt bool   `json:"is_int"`
}

// footer returns the uncompressed footer.
func (w *Writer) footer() ([]byte, error) {
	meta, err := json.Marshal(footerMeta{StartNs: w.startNs, EndNs: w.endNs, TotalTraces: uint64(w.rows[cg.TTraces]), Dedicated: w.footerDed})
	if err != nil {
		return nil, err
	}
	out := putBytes(nil, meta)
	out = putUvarint(out, cg.NumTables)
	for t := 0; t < cg.NumTables; t++ {
		tw := w.tables[t]
		if tw == nil {
			out = putUvarint(out, 0)
			continue
		}
		out = putUvarint(out, uint64(len(tw.groups)))
		for _, g := range tw.groups {
			out = putBytes(out, []byte(g.g.name))
			out = putUvarint(out, uint64(len(g.g.cols)))
			for _, c := range g.g.cols {
				out = putUvarint(out, uint64(c))
			}
		}
	}
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
	for t := 0; t < cg.NumTables; t++ {
		out = putUvarint(out, uint64(w.rows[t]))
	}
	out = putUvarint(out, uint64(len(w.anchors)))
	for t := 0; t < cg.NumTables; t++ {
		last := uint32(0)
		for i := range w.anchors {
			out = putUvarint(out, uint64(w.anchors[i][t]-last))
			last = w.anchors[i][t]
		}
	}
	for t := 0; t < cg.NumTables; t++ {
		if tw := w.tables[t]; tw != nil {
			for _, g := range tw.groups {
				out = putBytes(out, groupSection(g.chunks))
			}
		}
	}
	out = putUvarint(out, cg.NumLevels)
	for l, k := range w.km {
		out = putBytes(out, kmSection(k.flushes, ranks[l]))
	}
	return out, nil
}

// trailer is footer length u32 LE, footer variant, version, magic.
func trailer(footerLen int) []byte {
	b := binary.LittleEndian.AppendUint32(nil, uint32(footerLen))
	b = append(b, footerVariant, fileVersion)
	return append(b, cg.Magic...)
}

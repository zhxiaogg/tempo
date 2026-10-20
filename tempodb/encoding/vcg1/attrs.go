package vcg1

import (
	"context"
	"sort"

	"github.com/parquet-go/parquet-go"

	cg "github.com/grafana/tempo/v3/tempodb/encoding/vcg1/cgfile"
)

// kmKey is one attribute key of one level in the current window, read from the key's
// scan-copy runs: every entry (attribute value or array element) with its owner.
type kmKey struct {
	level int
	key   string
	id    uint32
	ok    bool                 // the key is in the level's key dictionary
	want  [cg.VTEmpty + 1]bool // run value types some iterator reads

	refs []cg.RunRef
	rds  []*cg.RunData
	ents []kmEnt // sorted by owner, stable
}

type kmEnt struct {
	o   uint32 // owner file row
	run int32
	i   int32
}

func (k *kmKey) need(w *window, ld *loader) error {
	k.refs, k.rds, k.ents = k.refs[:0], k.rds[:0], k.ents[:0]
	if !k.ok {
		return nil
	}
	t := cg.OwnerTable[k.level]
	refs, err := w.q.KeyRuns(k.level, k.id, w.lo[t], w.hi[t])
	if err != nil {
		return err
	}
	km, err := w.q.Km(k.level)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		if vt := km[ref.Flush].Runs[ref.Run].VType; int(vt) < len(k.want) && k.want[vt] {
			k.refs = append(k.refs, ref)
		}
	}
	ld.run(k.refs...)
	return nil
}

func (k *kmKey) wantAll() {
	for i := range k.want {
		k.want[i] = true
	}
}

func (k *kmKey) prepare(w *window) {
	t := cg.OwnerTable[k.level]
	lo, hi := w.lo[t], w.hi[t]
	for ri, ref := range k.refs {
		rd := w.q.Run(ref)
		k.rds = append(k.rds, rd)
		j := sort.Search(len(rd.Owners), func(i int) bool { return rd.Owners[i] >= lo })
		for ; j < len(rd.Owners) && rd.Owners[j] < hi; j++ {
			k.ents = append(k.ents, kmEnt{o: rd.Owners[j], run: int32(ri), i: int32(j)})
		}
	}
	if len(k.refs) > 1 {
		sort.SliceStable(k.ents, func(i, j int) bool { return k.ents[i].o < k.ents[j].o })
	}
}

func (k *kmKey) vtype(e kmEnt) uint8 { return k.rds[e.run].Meta.VType }

// value is the entry's value (null for an entry without one: empty, unsupported, empty array).
func (k *kmKey) value(e kmEnt) parquet.Value {
	rd := k.rds[e.run]
	if rd.Vals == nil {
		return parquet.Value{}
	}
	return valueOf(rd.Vals, int(e.i), rd.Dict)
}

// owner returns the entries of owner o; c is a cursor into ents for ascending lookups.
func (k *kmKey) owner(o uint32, c *int) []kmEnt {
	i := *c
	if i > len(k.ents) || (i > 0 && k.ents[i-1].o >= o) {
		i = sort.Search(len(k.ents), func(j int) bool { return k.ents[j].o >= o })
	}
	for i < len(k.ents) && k.ents[i].o < o {
		i++
	}
	j := i
	for j < len(k.ents) && k.ents[j].o == o {
		j++
	}
	*c = j
	return k.ents[i:j]
}

// strMemo caches a string predicate per dictionary code.
type strMemo []int8

func (m *strMemo) keep(code uint32, eval func() bool) bool {
	if int(code) >= len(*m) {
		n := make(strMemo, int(code)+1+len(*m))
		copy(n, *m)
		*m = n
	}
	switch (*m)[code] {
	case 1:
		return true
	case 2:
		return false
	}
	ok := eval()
	if ok {
		(*m)[code] = 1
	} else {
		(*m)[code] = 2
	}
	return ok
}

// pcAttr is one stored attribute from the point copy, with its elements (one for a scalar,
// the array's elements for an array, none for empty, unsupported or empty-array values).
type pcAttr struct {
	key  string
	vt   uint8
	vals []parquet.Value
}

// pointCopy reads the stored attributes of the owners (ascending file rows) of level l: the
// attribute pages whose owner zones meet them, then the element pages of their arrays.
func pointCopy(ctx context.Context, w *window, l int, owners []uint32, values bool) (map[uint32][]pcAttr, error) {
	out := make(map[uint32][]pcAttr, len(owners))
	if len(owners) == 0 {
		return out, nil
	}
	ta, te := cg.AttrsTable(l), cg.ElemsTable(l)
	ivs, err := zoneIvs(w, ta, cg.AtOwner, owners)
	if err != nil || len(ivs) == 0 {
		return out, err
	}
	ld := newLoader(w)
	cols := []int{cg.AtOwner, cg.AtKey, cg.AtVType, cg.AtStr, cg.AtStrRaw, cg.AtInt, cg.AtDbl, cg.AtBool}
	if !values {
		cols = cols[:3]
	}
	for _, c := range cols {
		ld.col(ta, c, ivs)
	}
	if err := ld.exec(ctx); err != nil {
		return nil, err
	}
	var cur [cg.AtBool + 1]*cursor
	for _, c := range cols {
		if cur[c], err = w.cursor(ta, c); err != nil {
			return nil, err
		}
	}
	type arr struct {
		o   uint32
		idx int
	}
	arrays := map[uint32]arr{}
	var arrRows []uint32
	for _, iv := range ivs {
		for r := iv.Lo; r < iv.Hi; r++ {
			o := uint32(cur[cg.AtOwner].int(r))
			if i := sort.Search(len(owners), func(i int) bool { return owners[i] >= o }); i == len(owners) || owners[i] != o {
				continue
			}
			a := pcAttr{key: cur[cg.AtKey].str(r), vt: uint8(cur[cg.AtVType].int(r))}
			if !values {
				out[o] = append(out[o], a)
				continue
			}
			switch a.vt {
			case cg.VTStr:
				v := cur[cg.AtStr].value(r)
				if v.IsNull() {
					v = cur[cg.AtStrRaw].value(r)
				}
				a.vals = []parquet.Value{v}
			case cg.VTInt:
				a.vals = []parquet.Value{cur[cg.AtInt].value(r)}
			case cg.VTDouble:
				a.vals = []parquet.Value{cur[cg.AtDbl].value(r)}
			case cg.VTBool:
				a.vals = []parquet.Value{cur[cg.AtBool].value(r)}
			case cg.VTStrArr, cg.VTIntArr, cg.VTDoubleArr, cg.VTBoolArr:
				arrays[r] = arr{o, len(out[o])}
				arrRows = append(arrRows, r)
			}
			out[o] = append(out[o], a)
		}
	}
	if len(arrRows) == 0 || w.q.Rows(te) == 0 {
		return out, nil
	}
	eivs, err := zoneIvs(w, te, cg.ElAttrRow, arrRows)
	if err != nil {
		return nil, err
	}
	for _, c := range []int{cg.ElAttrRow, cg.ElStr, cg.ElStrRaw, cg.ElInt, cg.ElDbl, cg.ElBool} {
		ld.col(te, c, eivs)
	}
	if err := ld.exec(ctx); err != nil {
		return nil, err
	}
	var ec [cg.ElBool + 1]*cursor
	for c := range ec {
		if ec[c], err = w.cursor(te, c); err != nil {
			return nil, err
		}
	}
	for _, iv := range eivs {
		for r := iv.Lo; r < iv.Hi; r++ {
			ar, ok := arrays[uint32(ec[cg.ElAttrRow].int(r))]
			if !ok {
				continue
			}
			a := &out[ar.o][ar.idx]
			for _, c := range []int{cg.ElStr, cg.ElStrRaw, cg.ElInt, cg.ElDbl, cg.ElBool} {
				if v := ec[c].value(r); !v.IsNull() {
					a.vals = append(a.vals, v)
					break
				}
			}
		}
	}
	return out, nil
}

// zoneIvs returns the row intervals of the pages of table t whose sorted row-reference column
// c may hold one of the (ascending) rows.
func zoneIvs(w *window, t, c int, rows []uint32) ([]cg.Interval, error) {
	col, err := w.q.Column(t, c)
	if err != nil {
		return nil, err
	}
	return col.PageIntervals(func(i int) bool {
		if col.AllNull(i) {
			return false
		}
		z := col.Zone(i)
		if z.Kind != cg.ZoneInt {
			return true
		}
		k := sort.Search(len(rows), func(j int) bool { return int64(rows[j]) >= z.Min })
		return k < len(rows) && int64(rows[k]) <= z.Max
	}), nil
}

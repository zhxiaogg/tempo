package cgfile

import (
	"github.com/grafana/tempo/v3/tempodb/backend"
	"github.com/grafana/tempo/v3/tempodb/encoding/vparquet5"
)

// DedSlot is one dedicated column of a scope: its attribute name, type and vParquet5 spare
// column index (String01 = 0, Int01 = 0, ...).
type DedSlot struct {
	Name  string
	IsInt bool
	Slot  int
}

// DedicatedSlots assigns a scope's dedicated columns to spare columns as vParquet5 does (per
// type in config order, up to the scope's spare columns) and returns them in the order the
// writer appends them to an owner's attributes: the string columns, then the int columns.
func DedicatedSlots(dc backend.DedicatedColumns, scope backend.DedicatedColumnScope) []DedSlot {
	paths := vparquet5.DedicatedResourceColumnPaths[scope]
	used := map[backend.DedicatedColumnType]int{}
	var strs, ints []DedSlot
	for _, c := range dc {
		if c.Scope != scope {
			continue
		}
		p, ok := paths[c.Type]
		if !ok || used[c.Type] >= len(p) {
			continue
		}
		s := DedSlot{Name: c.Name, IsInt: c.Type == backend.DedicatedColumnTypeInt, Slot: used[c.Type]}
		used[c.Type]++
		if s.IsInt {
			ints = append(ints, s)
		} else {
			strs = append(strs, s)
		}
	}
	return append(strs, ints...)
}

// DedStr returns spare string column i of d.
func DedStr(d *vparquet5.DedicatedAttributes, i int) *[]string {
	switch i {
	case 0:
		return &d.String01
	case 1:
		return &d.String02
	case 2:
		return &d.String03
	case 3:
		return &d.String04
	case 4:
		return &d.String05
	case 5:
		return &d.String06
	case 6:
		return &d.String07
	case 7:
		return &d.String08
	case 8:
		return &d.String09
	case 9:
		return &d.String10
	case 10:
		return &d.String11
	case 11:
		return &d.String12
	case 12:
		return &d.String13
	case 13:
		return &d.String14
	case 14:
		return &d.String15
	case 15:
		return &d.String16
	case 16:
		return &d.String17
	case 17:
		return &d.String18
	case 18:
		return &d.String19
	case 19:
		return &d.String20
	}
	return nil
}

// DedInt returns spare int column i of d.
func DedInt(d *vparquet5.DedicatedAttributes, i int) *[]int64 {
	switch i {
	case 0:
		return &d.Int01
	case 1:
		return &d.Int02
	case 2:
		return &d.Int03
	case 3:
		return &d.Int04
	case 4:
		return &d.Int05
	}
	return nil
}

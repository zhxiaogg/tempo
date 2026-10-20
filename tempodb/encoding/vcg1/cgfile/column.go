package cgfile

import "fmt"

// Column is one column's state in a request: its pages (from the footer), the pages decoded
// so far, and its merged dictionary.
type Column struct {
	T, C  int
	Spec  ColSpec
	Group *Group // nil when the column is in no group (not stored)
	K     int    // position in the group

	pages  []*Values
	chunks []chunkDict
	dict   Dict
}

type chunkDict struct {
	loaded bool
	wanted bool
	remap  []uint32 // chunk code -> merged code; nil = identity
	ints   []int64  // INT32 dictionary values
}

// Column returns the state of column c of table t, created from the footer on first use.
func (q *Request) Column(t, c int) (*Column, error) {
	k := colKey{t, c}
	if col := q.cols[k]; col != nil {
		return col, nil
	}
	cs := Cols(t)
	if c < 0 || c >= len(cs) {
		return nil, fmt.Errorf("cgfile: no column %d in %s", c, TableName(t))
	}
	col := &Column{T: t, C: c, Spec: cs[c]}
	if gi, pos, ok := q.F.ColumnGroup(t, c); ok {
		g, err := q.Group(t, gi)
		if err != nil {
			return nil, err
		}
		col.Group, col.K = g, pos
		col.pages = make([]*Values, len(g.Pages))
		col.chunks = make([]chunkDict, len(g.Chunks))
	}
	q.cols[k] = col
	return col, nil
}

// NumPages is the number of pages of the column.
func (c *Column) NumPages() int { return len(c.pages) }

// Page returns the footer metadata of page i.
func (c *Column) Page(i int) *Page { return &c.Group.Pages[i] }

// Buf returns the column's buffer of page i.
func (c *Column) Buf(i int) *Buf { return &c.Group.Pages[i].Bufs[c.K] }

// Zone returns the zone of page i (ZoneNone for unzoned columns).
func (c *Column) Zone(i int) Zone { return c.Buf(i).Zone }

// AllNull reports whether every row of page i is null for this column.
func (c *Column) AllNull(i int) bool { return c.Buf(i).Nulls == c.Group.Pages[i].Rows }

// Values returns decoded page i, or nil if it is not loaded.
func (c *Column) Values(i int) *Values { return c.pages[i] }

// Dict is the request's merged dictionary of the column: every value of every chunk
// dictionary loaded so far, each once. Codes of decoded dictionary pages index it.
func (c *Column) Dict() *Dict { return &c.dict }

// PagesForIntervals appends the pages meeting any of the (sorted) intervals to dst.
func (c *Column) PagesForIntervals(ivs []Interval, dst []int) []int {
	if c.Group == nil {
		return dst
	}
	for _, iv := range ivs {
		if iv.Lo >= iv.Hi {
			continue
		}
		i := c.Group.PageOf(iv.Lo)
		if i < 0 {
			continue
		}
		for ; i < len(c.Group.Pages) && c.Group.Pages[i].First < iv.Hi; i++ {
			if n := len(dst); n == 0 || dst[n-1] < i {
				dst = append(dst, i)
			}
		}
	}
	return dst
}

// PageIntervals returns the merged row intervals of the pages for which keep is true.
func (c *Column) PageIntervals(keep func(i int) bool) []Interval {
	var out []Interval
	if c.Group == nil {
		return out
	}
	for i := range c.Group.Pages {
		if !keep(i) {
			continue
		}
		p := &c.Group.Pages[i]
		if n := len(out); n > 0 && out[n-1].Hi == p.First {
			out[n-1].Hi = p.First + p.Rows
		} else {
			out = append(out, Interval{p.First, p.First + p.Rows})
		}
	}
	return out
}

// At returns the decoded page holding file row r and r's index in it.
func (c *Column) At(r uint32) (*Values, int, error) {
	if c.Group == nil {
		return nil, 0, fmt.Errorf("cgfile: %s.%s is not stored", TableName(c.T), c.Spec.Name)
	}
	i := c.Group.PageOf(r)
	if i < 0 {
		return nil, 0, fmt.Errorf("cgfile: %s.%s: row %d out of range", TableName(c.T), c.Spec.Name, r)
	}
	v := c.pages[i]
	if v == nil {
		return nil, 0, fmt.Errorf("cgfile: %s.%s: row %d not loaded", TableName(c.T), c.Spec.Name, r)
	}
	return v, int(r - v.First), nil
}

// Int returns the int value of row r (ok = false when null).
func (c *Column) Int(r uint32) (v int64, ok bool, err error) {
	p, i, err := c.At(r)
	if err != nil {
		return 0, false, err
	}
	if p.IsNull(i) {
		return 0, false, nil
	}
	return p.Ints[i], true, nil
}

// Bytes returns the byte-array value of row r (nil and ok = false when null).
func (c *Column) Bytes(r uint32) ([]byte, bool, error) {
	p, i, err := c.At(r)
	if err != nil {
		return nil, false, err
	}
	if p.IsNull(i) {
		return nil, false, nil
	}
	return p.Bytes(i, &c.dict), true, nil
}

// Dict is a dictionary of byte-array values.
type Dict struct {
	Values [][]byte
	index  map[string]uint32
}

// Len is the number of entries.
func (d *Dict) Len() int { return len(d.Values) }

// Get returns entry code (nil for NullCode or out of range).
func (d *Dict) Get(code uint32) []byte {
	if int64(code) >= int64(len(d.Values)) {
		return nil
	}
	return d.Values[code]
}

// Lookup returns the code of value v.
func (d *Dict) Lookup(v []byte) (uint32, bool) {
	d.buildIndex()
	c, ok := d.index[string(v)]
	return c, ok
}

func (d *Dict) buildIndex() {
	if d.index != nil {
		return
	}
	d.index = make(map[string]uint32, len(d.Values))
	for i, v := range d.Values {
		if _, ok := d.index[string(v)]; !ok {
			d.index[string(v)] = uint32(i)
		}
	}
}

// merge adds the entries of a chunk dictionary, each value once, and returns the chunk's
// code -> merged code map (nil: identity, for the first dictionary merged).
func (d *Dict) merge(entries [][]byte) []uint32 {
	if len(d.Values) == 0 && d.index == nil {
		d.Values = entries
		return nil
	}
	d.buildIndex()
	remap := make([]uint32, len(entries))
	for i, v := range entries {
		c, ok := d.index[string(v)]
		if !ok {
			c = uint32(len(d.Values))
			d.Values = append(d.Values, v)
			d.index[string(v)] = c
		}
		remap[i] = c
	}
	return remap
}

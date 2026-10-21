// Package cgwrite writes CG blocks (block.cg, read by package cgfile) from a stream of
// vParquet5 trace rows in trace-ID order.
//
// Traces are flattened one at a time straight into the column groups' pending pages and the
// scan copy's buffers, so memory is bounded by the writer limits, not by the input:
//
//   - Structured groups: pages of about PageBytes (AttrPageBytes for the point-copy attribute
//     tables) of uncompressed column data are encoded as they fill and held compressed until the
//     pending chunk would pass ChunkBytes; then the chunk (its dictionaries, then its pages) is
//     written. A byte-array column whose chunk dictionary passes ChunkBytes/4 writes the rest of
//     the chunk's pages as DELTA_LENGTH_BYTE_ARRAY.
//   - Scan copy (key-major attributes): entries are buffered per (key, value type) in
//     insertion order, which is already the run order; once the buffers of all levels hold
//     KmBytes, the level holding the most is written as one flush of runs, run by run.
//   - Anchors: one per page of traces.tmeta, the first row of every table at the page's first
//     trace. CutAnchor forces the next trace to start a page (and so an anchor).
//
// The trace is the vParquet5 row Tempo would write (vparquet5.TraceConverter), so the
// precomputed fields (nested set, rounded start times, service stats, root names) are
// vParquet5's own. Dedicated columns are unfolded into ordinary attributes the way vParquet5
// returns them (one value: a scalar, several: an array), after the generic attributes.
package cgwrite

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/grafana/tempo/v3/tempodb/backend"
	cg "github.com/grafana/tempo/v3/tempodb/encoding/vcg1/cgfile"
	"github.com/grafana/tempo/v3/tempodb/encoding/vparquet5"
)

// Options configures a Writer. Zero values are the package defaults.
type Options struct {
	// Dedicated is the vParquet5 dedicated-column config the rows were built with.
	Dedicated backend.DedicatedColumns
	// KmBytes bounds the buffered scan-copy entries (all levels together).
	KmBytes int
	// PartBytes is the minimum size of a write to the output (except the last).
	PartBytes int
	// ChunkBytes bounds a group chunk.
	ChunkBytes int
	// PageBytes and AttrPageBytes are the page sizes of the groups and of the point-copy
	// attribute tables.
	PageBytes, AttrPageBytes int
	// Zstd is the compression level (default zstd.SpeedDefault: on the reference block it
	// writes 4.8% fewer bytes than SpeedFastest at the same wall time).
	Zstd zstd.EncoderLevel
}

// Result describes a written file.
type Result struct {
	Size      uint64 // bytes of the file
	FooterLen uint32 // compressed footer bytes (the block meta's FooterSize)
	Anchors   int    // anchors (the block meta's TotalRecords)
	Traces    uint32
	Rows      [cg.NumTables]uint32
	Chunks    int
	Pages     int
	KmFlushes [cg.NumLevels]int
	Runs      int
	FooterRaw int // uncompressed footer bytes
}

// Writer writes one CG file. It is not safe for concurrent use.
type Writer struct {
	c    *coder
	sink *sink

	tables [cg.NumTables]*tableW // nil for the key-major tables
	km     [cg.NumLevels]*kmLevel
	rows   [cg.NumTables]uint32

	anchors [][cg.NumTables]uint32
	cut     bool

	chunkBytes, kmBytes int

	startNs, endNs uint64
	footerDed      []footerDedCol
	resDed         []cg.DedSlot
	spanDed        []cg.DedSlot
	eventDed       []cg.DedSlot

	byLeft map[int32][]byte
	lastID []byte
	closed bool

	// scratch
	region, hdr, pre []byte
	val              aval
}

// New starts a file on w.
func New(w io.Writer, opts Options) (*Writer, error) {
	level := opts.Zstd
	if level == 0 {
		level = zstd.SpeedDefault
	}
	c, err := newCoder(level)
	if err != nil {
		return nil, err
	}
	wr := &Writer{
		c:          c,
		sink:       &sink{w: w, part: valueOr(opts.PartBytes, PartBytes)},
		chunkBytes: valueOr(opts.ChunkBytes, ChunkBytes),
		kmBytes:    valueOr(opts.KmBytes, KmBytes),
		byLeft:     map[int32][]byte{},
	}
	for t := 0; t < cg.NumTables; t++ {
		if t < 7 || (t-7)%3 != 2 {
			wr.tables[t] = newTableW(t, valueOr(opts.PageBytes, PageBytes), valueOr(opts.AttrPageBytes, AttrPageBytes))
		}
	}
	for l := range wr.km {
		wr.km[l] = newKmLevel(l)
	}
	wr.resDed = cg.DedicatedSlots(opts.Dedicated, backend.DedicatedColumnScopeResource)
	wr.spanDed = cg.DedicatedSlots(opts.Dedicated, backend.DedicatedColumnScopeSpan)
	wr.eventDed = cg.DedicatedSlots(opts.Dedicated, backend.DedicatedColumnScopeEvent)
	wr.footerDed = make([]footerDedCol, 0, len(opts.Dedicated))
	for _, d := range opts.Dedicated {
		s := string(d.Scope)
		if s == "" {
			s = string(backend.DedicatedColumnScopeSpan)
		}
		wr.footerDed = append(wr.footerDed, footerDedCol{Scope: strings.ToUpper(s[:1]) + s[1:], Name: d.Name, IsInt: d.Type == backend.DedicatedColumnTypeInt})
	}
	if _, err := wr.sink.write([]byte(cg.Magic)); err != nil {
		return nil, err
	}
	return wr, nil
}

func valueOr(v, d int) int {
	if v > 0 {
		return v
	}
	return d
}

// CutAnchor makes the next trace start a new anchor (e.g. at a source row-group boundary).
func (w *Writer) CutAnchor() { w.cut = true }

// Anchors is the number of anchors so far.
func (w *Writer) Anchors() int { return len(w.anchors) }

// BufferedBytes is the memory held by the writer's buffers (pending pages and chunks,
// dictionaries and scan-copy entries).
func (w *Writer) BufferedBytes() int {
	n := cap(w.sink.buf) + cap(w.region)
	for _, tw := range w.tables {
		if tw == nil {
			continue
		}
		for _, g := range tw.groups {
			n += g.memBytes()
		}
	}
	for _, k := range w.km {
		n += k.bytes
	}
	return n
}

var errClosed = errors.New("cgwrite: writer is closed")

// Add appends one trace. Traces must come in non-decreasing trace-ID order.
func (w *Writer) Add(tr *vparquet5.Trace) error {
	if w.closed {
		return errClosed
	}
	if w.rows[cg.TTraces] > 0 && bytes.Compare(tr.TraceID, w.lastID) < 0 {
		return fmt.Errorf("cgwrite: trace %x after %x: traces must be in trace-ID order", tr.TraceID, w.lastID)
	}
	w.lastID = append(w.lastID[:0], tr.TraceID...)

	tt := w.tables[cg.TTraces]
	tm := tt.groups[anchorGroup]
	if w.cut && tm.rows > 0 {
		if err := tm.cutPage(w); err != nil {
			return err
		}
	}
	w.cut = false
	if tm.rows == 0 {
		var a [cg.NumTables]uint32
		for t := range a {
			if w.tables[t] != nil {
				a[t] = w.rows[t]
			}
		}
		w.anchors = append(w.anchors, a)
	}
	if tr.StartTimeUnixNano > 0 && (w.startNs == 0 || tr.StartTimeUnixNano < w.startNs) {
		w.startNs = tr.StartTimeUnixNano
	}
	w.endNs = max(w.endNs, tr.EndTimeUnixNano)

	tt.bytes(cg.TrTraceID, tr.TraceID)
	tt.i64(cg.TrStart, int64(tr.StartTimeUnixNano))
	tt.i64(cg.TrEnd, int64(tr.EndTimeUnixNano))
	tt.i64(cg.TrDuration, int64(tr.DurationNano))
	tt.str(cg.TrRootService, tr.RootServiceName)
	tt.str(cg.TrRootSpan, tr.RootSpanName)
	tt.i32(cg.TrFirstStat, int32(w.rows[cg.TServiceStats]))
	tt.i32(cg.TrFirstResource, int32(w.rows[cg.TResources]))
	tt.i32(cg.TrFirstSpan, int32(w.rows[cg.TSpans]))
	if err := tt.endRow(w); err != nil {
		return err
	}

	st := w.tables[cg.TServiceStats]
	for i := range tr.ServiceStats {
		s := &tr.ServiceStats[i]
		st.str(cg.SsServiceName, s.ServiceName)
		st.i32(cg.SsSpanCount, int32(s.SpanCount))
		st.i32(cg.SsErrorCount, int32(s.ErrorCount))
		if err := st.endRow(w); err != nil {
			return err
		}
	}

	// derived parent span ids: the first span of the trace (trace order) per nested_left
	if len(w.byLeft) > 1024 {
		// clear costs the map's capacity, which a big trace leaves large for every later trace
		w.byLeft = make(map[int32][]byte)
	} else {
		clear(w.byLeft)
	}
	for ri := range tr.ResourceSpans {
		for si := range tr.ResourceSpans[ri].ScopeSpans {
			sps := tr.ResourceSpans[ri].ScopeSpans[si].Spans
			for k := range sps {
				if _, ok := w.byLeft[sps[k].NestedSetLeft]; !ok {
					w.byLeft[sps[k].NestedSetLeft] = sps[k].SpanID
				}
			}
		}
	}

	rt, sct := w.tables[cg.TResources], w.tables[cg.TScopes]
	for ri := range tr.ResourceSpans {
		rs := &tr.ResourceSpans[ri]
		nsp := 0
		for si := range rs.ScopeSpans {
			nsp += len(rs.ScopeSpans[si].Spans)
		}
		row, span0 := w.rows[cg.TResources], int32(w.rows[cg.TSpans])
		rt.str(cg.ResServiceName, rs.Resource.ServiceName)
		rt.i32(cg.ResDropped, rs.Resource.DroppedAttributesCount)
		rt.i32(cg.ResFirstScope, int32(w.rows[cg.TScopes]))
		rt.i32(cg.ResFirstSpan, span0)
		if err := rt.endRow(w); err != nil {
			return err
		}
		if err := w.attrs(cg.LResource, row, [2]int32{span0, int32(nsp)}, rs.Resource.Attrs, &rs.Resource.DedicatedAttributes, w.resDed); err != nil {
			return err
		}
		for si := range rs.ScopeSpans {
			ss := &rs.ScopeSpans[si]
			row, span0 := w.rows[cg.TScopes], int32(w.rows[cg.TSpans])
			sct.str(cg.ScName, ss.Scope.Name)
			sct.str(cg.ScVersion, ss.Scope.Version)
			sct.i32(cg.ScDropped, ss.Scope.DroppedAttributesCount)
			sct.i32(cg.ScFirstSpan, span0)
			if err := sct.endRow(w); err != nil {
				return err
			}
			if err := w.attrs(cg.LScope, row, [2]int32{span0, int32(len(ss.Spans))}, ss.Scope.Attrs, nil, nil); err != nil {
				return err
			}
			for k := range ss.Spans {
				if err := w.span(&ss.Spans[k]); err != nil {
					return err
				}
			}
		}
	}
	return w.maybeFlushKm()
}

func (w *Writer) span(s *vparquet5.Span) error {
	p := w.tables[cg.TSpans]
	row := w.rows[cg.TSpans]
	p.bytes(cg.SpSpanID, s.SpanID)
	var derived []byte
	if s.ParentID > 0 {
		derived = w.byLeft[s.ParentID]
	}
	if bytes.Equal(s.ParentSpanID, derived) {
		p.null(cg.SpParentSpanID)
	} else {
		p.bytes(cg.SpParentSpanID, s.ParentSpanID)
	}
	p.i32(cg.SpParentID, s.ParentID)
	p.i32(cg.SpNestedLeft, s.NestedSetLeft)
	p.i32(cg.SpNestedRight, s.NestedSetRight)
	p.i32(cg.SpChildCount, s.ChildCount)
	p.str(cg.SpName, s.Name)
	p.i64(cg.SpKind, int64(s.Kind))
	p.str(cg.SpTraceState, s.TraceState)
	p.i64(cg.SpStart, int64(s.StartTimeUnixNano))
	p.i64(cg.SpDuration, int64(s.DurationNano))
	p.i64(cg.SpStatusCode, int64(s.StatusCode))
	p.str(cg.SpStatusMessage, s.StatusMessage)
	p.i32(cg.SpDroppedAttrs, s.DroppedAttributesCount)
	p.i32(cg.SpDroppedEvents, s.DroppedEventsCount)
	p.i32(cg.SpDroppedLinks, s.DroppedLinksCount)
	p.i32(cg.SpRounded15, int32(s.StartTimeRounded15))
	p.i32(cg.SpRounded60, int32(s.StartTimeRounded60))
	p.i32(cg.SpRounded300, int32(s.StartTimeRounded300))
	p.i32(cg.SpRounded3600, int32(s.StartTimeRounded3600))
	p.i32(cg.SpFirstEvent, int32(w.rows[cg.TEvents]))
	p.i32(cg.SpFirstLink, int32(w.rows[cg.TLinks]))
	if err := p.endRow(w); err != nil {
		return err
	}
	if err := w.attrs(cg.LSpan, row, [2]int32{}, s.Attrs, &s.DedicatedAttributes, w.spanDed); err != nil {
		return err
	}
	et := w.tables[cg.TEvents]
	for i := range s.Events {
		e := &s.Events[i]
		erow := w.rows[cg.TEvents]
		et.i64(cg.EvTime, int64(e.TimeSinceStartNano))
		et.str(cg.EvName, e.Name)
		et.i32(cg.EvDropped, e.DroppedAttributesCount)
		if err := et.endRow(w); err != nil {
			return err
		}
		if err := w.attrs(cg.LEvent, erow, [2]int32{int32(row)}, e.Attrs, &e.DedicatedAttributes, w.eventDed); err != nil {
			return err
		}
	}
	lt := w.tables[cg.TLinks]
	for i := range s.Links {
		l := &s.Links[i]
		lrow := w.rows[cg.TLinks]
		lt.bytes(cg.LnTraceID, l.TraceID)
		lt.bytes(cg.LnSpanID, l.SpanID)
		lt.str(cg.LnTraceState, l.TraceState)
		lt.i32(cg.LnDropped, l.DroppedAttributesCount)
		if err := lt.endRow(w); err != nil {
			return err
		}
		if err := w.attrs(cg.LLink, lrow, [2]int32{int32(row)}, l.Attrs, nil, nil); err != nil {
			return err
		}
	}
	return nil
}

// aval is one attribute value in model form.
type aval struct {
	vt    uint8
	s     string
	i     int64
	d     float64
	b     bool
	ss    []string
	is    []int64
	ds    []float64
	bs    []bool
	unsup string
}

// fromParquet maps a vParquet5 generic attribute to its model value (vParquet5's own reading:
// the first non-empty typed list; arrays keep every element).
func (v *aval) fromParquet(a *vparquet5.Attribute) {
	*v = aval{}
	switch {
	case a.ValueUnsupported != nil:
		v.vt, v.unsup = cg.VTUnsupported, *a.ValueUnsupported
	case a.IsArray:
		switch {
		case len(a.Value) > 0:
			v.vt, v.ss = cg.VTStrArr, a.Value
		case len(a.ValueInt) > 0:
			v.vt, v.is = cg.VTIntArr, a.ValueInt
		case len(a.ValueDouble) > 0:
			v.vt, v.ds = cg.VTDoubleArr, a.ValueDouble
		case len(a.ValueBool) > 0:
			v.vt, v.bs = cg.VTBoolArr, a.ValueBool
		default:
			v.vt = cg.VTEmptyArr
		}
	case len(a.Value) > 0:
		v.vt, v.s = cg.VTStr, a.Value[0]
	case len(a.ValueInt) > 0:
		v.vt, v.i = cg.VTInt, a.ValueInt[0]
	case len(a.ValueDouble) > 0:
		v.vt, v.d = cg.VTDouble, a.ValueDouble[0]
	case len(a.ValueBool) > 0:
		v.vt, v.b = cg.VTBool, a.ValueBool[0]
	default:
		v.vt = cg.VTEmpty
	}
}

// attrs writes the attributes of one owner row: generic ones, then the dedicated columns.
func (w *Writer) attrs(l int, owner uint32, links [2]int32, gen []vparquet5.Attribute, ded *vparquet5.DedicatedAttributes, slots []cg.DedSlot) error {
	v := &w.val
	for i := range gen {
		v.fromParquet(&gen[i])
		if err := w.attr(l, owner, links, gen[i].Key, v); err != nil {
			return err
		}
	}
	for _, s := range slots {
		*v = aval{}
		if s.IsInt {
			vals := *cg.DedInt(ded, s.Slot)
			switch len(vals) {
			case 0:
				continue
			case 1:
				v.vt, v.i = cg.VTInt, vals[0]
			default:
				v.vt, v.is = cg.VTIntArr, vals
			}
		} else {
			vals := *cg.DedStr(ded, s.Slot)
			switch len(vals) {
			case 0:
				continue
			case 1:
				v.vt, v.s = cg.VTStr, vals[0]
			default:
				v.vt, v.ss = cg.VTStrArr, vals
			}
		}
		if err := w.attr(l, owner, links, s.Name, v); err != nil {
			return err
		}
	}
	return nil
}

// attr writes one attribute: its point-copy row, its array elements and its scan-copy entries.
func (w *Writer) attr(l int, owner uint32, links [2]int32, key string, v *aval) error {
	ta, te := cg.AttrsTable(l), cg.ElemsTable(l)
	at := w.tables[ta]
	arow := w.rows[ta]
	at.i32(cg.AtOwner, int32(owner))
	at.str(cg.AtKey, key)
	at.i32(cg.AtVType, int32(v.vt))
	if v.vt == cg.VTStr {
		at.str(cg.AtStr, v.s)
	} else {
		at.null(cg.AtStr)
	}
	at.null(cg.AtStrRaw)
	if v.vt == cg.VTInt {
		at.i64(cg.AtInt, v.i)
	} else {
		at.null(cg.AtInt)
	}
	if v.vt == cg.VTDouble {
		at.f64(cg.AtDbl, v.d)
	} else {
		at.null(cg.AtDbl)
	}
	if v.vt == cg.VTBool {
		at.bool(cg.AtBool, v.b)
	} else {
		at.null(cg.AtBool)
	}
	if v.vt == cg.VTUnsupported {
		at.str(cg.AtUnsupported, v.unsup)
	} else {
		at.null(cg.AtUnsupported)
	}
	if err := at.endRow(w); err != nil {
		return err
	}

	km := w.km[l]
	kid := km.key(key)
	o := int32(owner)
	et := w.tables[te]
	elem := func(c int) {
		et.i32(cg.ElAttrRow, int32(arow))
		for _, k := range [...]int{cg.ElStr, cg.ElStrRaw, cg.ElInt, cg.ElDbl, cg.ElBool} {
			if k != c {
				et.null(k)
			}
		}
	}
	switch v.vt {
	case cg.VTStr:
		km.addStr(kid, o, links, v.s)
	case cg.VTInt:
		km.addInt(kid, o, links, v.i)
	case cg.VTDouble:
		km.addDbl(kid, o, links, v.d)
	case cg.VTBool:
		km.addBool(kid, o, links, v.b)
	case cg.VTStrArr:
		for _, x := range v.ss {
			elem(cg.ElStr)
			et.str(cg.ElStr, x)
			if err := et.endRow(w); err != nil {
				return err
			}
			km.addStr(kid, o, links, x)
		}
	case cg.VTIntArr:
		for _, x := range v.is {
			elem(cg.ElInt)
			et.i64(cg.ElInt, x)
			if err := et.endRow(w); err != nil {
				return err
			}
			km.addInt(kid, o, links, x)
		}
	case cg.VTDoubleArr:
		for _, x := range v.ds {
			elem(cg.ElDbl)
			et.f64(cg.ElDbl, x)
			if err := et.endRow(w); err != nil {
				return err
			}
			km.addDbl(kid, o, links, x)
		}
	case cg.VTBoolArr:
		for _, x := range v.bs {
			elem(cg.ElBool)
			et.bool(cg.ElBool, x)
			if err := et.endRow(w); err != nil {
				return err
			}
			km.addBool(kid, o, links, x)
		}
	default:
		km.addNone(kid, v.vt, o, links)
	}
	return nil
}

// maybeFlushKm writes the largest level's scan copy while the buffers hold KmBytes or more.
func (w *Writer) maybeFlushKm() error {
	for {
		total, big := 0, 0
		for l, k := range w.km {
			total += k.bytes
			if k.bytes > w.km[big].bytes {
				big = l
			}
		}
		if total < w.kmBytes {
			return nil
		}
		if err := w.km[big].flush(w, w.rows[cg.OwnerTable[big]]); err != nil {
			return err
		}
	}
}

// Close writes the remaining pages and runs, the footer and the trailer.
func (w *Writer) Close() (Result, error) {
	if w.closed {
		return Result{}, errClosed
	}
	w.closed = true
	defer w.c.close()
	var res Result
	for _, tw := range w.tables {
		if tw == nil {
			continue
		}
		if err := tw.finish(w); err != nil {
			return res, err
		}
		for _, g := range tw.groups {
			res.Chunks += len(g.chunks)
			res.Pages += g.npages
		}
	}
	for l, k := range w.km {
		if err := k.flush(w, w.rows[cg.OwnerTable[l]]); err != nil {
			return res, err
		}
		w.rows[cg.KmTable(l)] = k.entries
		res.KmFlushes[l] = len(k.flushes)
		res.Runs += k.nruns
	}
	raw, err := w.footer()
	if err != nil {
		return res, err
	}
	z := w.c.zw.EncodeAll(raw, nil)
	if _, err := w.sink.write(z); err != nil {
		return res, err
	}
	if _, err := w.sink.write(trailer(len(z))); err != nil {
		return res, err
	}
	if err := w.sink.flush(); err != nil {
		return res, err
	}
	res.Size = w.sink.pos
	res.FooterLen = uint32(len(z))
	res.FooterRaw = len(raw)
	res.Anchors = len(w.anchors)
	res.Traces = w.rows[cg.TTraces]
	res.Rows = w.rows
	return res, nil
}

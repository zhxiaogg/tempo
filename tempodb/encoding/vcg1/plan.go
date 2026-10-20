package vcg1

import (
	"fmt"
	"math"
	"time"

	"github.com/parquet-go/parquet-go"

	"github.com/grafana/tempo/v3/pkg/parquetquery"
	"github.com/grafana/tempo/v3/pkg/sampling"
	"github.com/grafana/tempo/v3/pkg/traceql"
	"github.com/grafana/tempo/v3/tempodb/backend"
	cg "github.com/grafana/tempo/v3/tempodb/encoding/vcg1/cgfile"
	"github.com/grafana/tempo/v3/tempodb/encoding/vparquet5"
)

// Iterator levels, in vparquet5's nesting order (outermost first).
const (
	lvTrace = iota
	lvRes
	lvScope
	lvSpan
	lvEvent
	lvLink
	nLv
)

// cgLevel is the attribute level of each iterator level (-1: none).
var cgLevel = [nLv]int{-1, cg.LResource, cg.LScope, cg.LSpan, cg.LEvent, cg.LLink}

var attrOf = [nLv]func(string) traceql.Attribute{
	nil,
	func(n string) traceql.Attribute {
		return traceql.NewScopedAttribute(traceql.AttributeScopeResource, false, n)
	},
	func(n string) traceql.Attribute {
		return traceql.NewScopedAttribute(traceql.AttributeScopeInstrumentation, false, n)
	},
	func(n string) traceql.Attribute {
		return traceql.NewScopedAttribute(traceql.AttributeScopeSpan, false, n)
	},
	func(n string) traceql.Attribute {
		return traceql.NewScopedAttribute(traceql.AttributeScopeEvent, false, n)
	},
	func(n string) traceql.Attribute {
		return traceql.NewScopedAttribute(traceql.AttributeScopeLink, false, n)
	},
}

// level is one iterator level of a pass: its generic attribute iterator, its column
// iterators (emission order: generic first, as vparquet5's OtherEntries precede Entries) and
// the minimum attribute count of its collector under AllConditions.
type level struct {
	n     int // conditions at this level
	gen   *genItem
	items []item
	opt   []bool // optional even under AllConditions (exemplar callbacks)
	min   int
}

func (l *level) add(it item) {
	l.items = append(l.items, it)
	l.opt = append(l.opt, false)
}

// plan is one pass (createAllIterator) compiled from its conditions.
type plan struct {
	all        bool
	requireOne bool
	selectAll  bool
	start, end uint64
	lv         [nLv]*level
	ded        [cg.NumLevels]map[string]uint8 // dedicated key -> scalar vtype of its column
	keys       map[string]*kmKey              // "<level>/<key>"
	footer     *cg.Footer
	spanStart  bool // the pass sets span start times (a span-level start-time column)

	timeStart, timeEnd item // trace time-range filter (both or neither)

	spanOnly bool // FetchSpans: one span-level join (no per-level minimums, events joined per span)
	spanMin  int  // FetchSpans: spanCollector2's minimum attribute count
}

var intrinsicScope = map[traceql.Intrinsic]int{
	traceql.IntrinsicName: lvSpan, traceql.IntrinsicStatus: lvSpan, traceql.IntrinsicStatusMessage: lvSpan,
	traceql.IntrinsicDuration: lvSpan, traceql.IntrinsicKind: lvSpan, traceql.IntrinsicSpanID: lvSpan,
	traceql.IntrinsicParentID: lvSpan, traceql.IntrinsicSpanStartTime: lvSpan,
	traceql.IntrinsicStructuralDescendant: lvSpan, traceql.IntrinsicStructuralChild: lvSpan,
	traceql.IntrinsicStructuralSibling: lvSpan, traceql.IntrinsicNestedSetLeft: lvSpan,
	traceql.IntrinsicNestedSetRight: lvSpan, traceql.IntrinsicNestedSetParent: lvSpan,
	traceql.IntrinsicChildCount: lvSpan, traceql.IntrinsicSpanMultiplier: lvSpan,
	traceql.IntrinsicTraceRootService: lvTrace, traceql.IntrinsicTraceRootSpan: lvTrace,
	traceql.IntrinsicTraceDuration: lvTrace, traceql.IntrinsicTraceID: lvTrace,
	traceql.IntrinsicTraceStartTime: lvTrace, traceql.IntrinsicServiceStats: lvTrace,
	traceql.IntrinsicEventName: lvEvent, traceql.IntrinsicEventTimeSinceStart: lvEvent,
	traceql.IntrinsicLinkTraceID: lvLink, traceql.IntrinsicLinkSpanID: lvLink,
	traceql.IntrinsicInstrumentationName: lvScope, traceql.IntrinsicInstrumentationVersion: lvScope,
}

// dedicatedMapping is vparquet5's dedicatedColumnsToColumnMapping: columns of each scope and
// type in meta order, up to the spare columns available; key -> column type.
func dedicatedMapping(dc backend.DedicatedColumns, scope backend.DedicatedColumnScope) map[string]uint8 {
	out := map[string]uint8{}
	used := map[backend.DedicatedColumnType]int{}
	for _, c := range dc {
		if c.Scope != scope {
			continue
		}
		paths, ok := vparquet5.DedicatedResourceColumnPaths[scope][c.Type]
		if !ok || used[c.Type] >= len(paths) {
			continue
		}
		used[c.Type]++
		if c.Type == backend.DedicatedColumnTypeInt {
			out[c.Name] = cg.VTInt
		} else {
			out[c.Name] = cg.VTStr
		}
	}
	return out
}

func staticTypeOf(vt uint8) traceql.StaticType {
	if vt == cg.VTInt {
		return traceql.TypeInt
	}
	return traceql.TypeString
}

func compile(f *cg.Footer, conds []traceql.Condition, all bool, start, end uint64, dc backend.DedicatedColumns, selectAll, spanOnly bool) (*plan, error) {
	p := &plan{selectAll: selectAll, start: start, end: end, keys: map[string]*kmKey{}, footer: f, spanOnly: spanOnly}
	p.ded[cg.LResource] = dedicatedMapping(dc, backend.DedicatedColumnScopeResource)
	p.ded[cg.LSpan] = dedicatedMapping(dc, backend.DedicatedColumnScopeSpan)
	p.ded[cg.LEvent] = dedicatedMapping(dc, backend.DedicatedColumnScopeEvent)
	var cats [nLv][]traceql.Condition
	mingled := false
	for _, c := range conds {
		lv := -1
		switch c.Attribute.Scope {
		case traceql.AttributeScopeNone:
			if s, ok := intrinsicScope[c.Attribute.Intrinsic]; ok {
				lv = s
			} else {
				mingled = true
				cats[lvSpan] = append(cats[lvSpan], c)
				cats[lvRes] = append(cats[lvRes], c)
				continue
			}
		case traceql.AttributeScopeSpan:
			lv = lvSpan
		case traceql.AttributeScopeResource:
			lv = lvRes
		case traceql.AttributeScopeEvent:
			lv = lvEvent
		case traceql.AttributeScopeLink:
			lv = lvLink
		case traceql.AttributeScopeInstrumentation:
			lv = lvScope
		default:
			return nil, fmt.Errorf("unsupported traceql scope: %s", c.Attribute)
		}
		cats[lv] = append(cats[lv], c)
	}
	p.requireOne = len(conds) > 0 && len(cats[lvTrace]) == 0
	p.all = all && !mingled
	switch {
	case selectAll:
	case p.all:
		names := map[string]struct{}{}
		for _, c := range conds {
			if c.CallBack == nil {
				names[c.Attribute.Name] = struct{}{}
			}
		}
		p.spanMin = len(names)
	case p.requireOne:
		p.spanMin = 1
	}
	for l := range p.lv {
		p.lv[l] = &level{n: len(cats[l])}
		if p.all {
			names := map[string]struct{}{}
			for _, c := range cats[l] {
				names[c.Attribute.Name] = struct{}{}
			}
			p.lv[l].min = len(names)
		}
	}
	var err error
	for _, l := range []int{lvEvent, lvLink, lvSpan, lvScope, lvRes} {
		if err = p.compileLevel(l, cats[l]); err != nil {
			return nil, err
		}
	}
	return p, p.compileTrace(cats[lvTrace])
}

func (p *plan) kmKey(l int, key string) *kmKey {
	k := fmt.Sprintf("%d/%s", l, key)
	if x := p.keys[k]; x != nil {
		return x
	}
	x := &kmKey{level: l, key: key}
	x.id, x.ok = p.footer.KeyID(l, key)
	p.keys[k] = x
	return x
}

// colGroup collects the predicates of one column (vparquet5's columnPredicates/addPredicate):
// one iterator with the OR of them.
type colGroup struct {
	preds []parquetquery.Predicate
	name  string
	mk    func(pred parquetquery.Predicate) item
}

type groups struct {
	order []string
	m     map[string]*colGroup
}

func (g *groups) add(path string, pred parquetquery.Predicate, name string, mk func(parquetquery.Predicate) item) {
	if g.m == nil {
		g.m = map[string]*colGroup{}
	}
	c := g.m[path]
	if c == nil {
		c = &colGroup{mk: mk}
		g.m[path] = c
		g.order = append(g.order, path)
	}
	c.preds = append(c.preds, pred)
	c.name = name
}

func (g *groups) hasNil(path string) bool {
	if c := g.m[path]; c != nil {
		for _, p := range c.preds {
			if p == nil {
				return true
			}
		}
	}
	return false
}

func (p *plan) compileLevel(l int, conds []traceql.Condition) error {
	lv := p.lv[l]
	cl := cgLevel[l]
	attr := attrOf[l]
	var (
		g       groups
		generic []traceql.Condition
		extra   []item // nil iterators, per condition
		explL   bool
		explR   bool
		explP   bool
	)
	ded := func(name string, pr parquetquery.Predicate, vt uint8) item {
		k := p.kmKey(cl, name)
		k.want[vt] = true
		return &dedItem{k: k, vt: vt, pred: pr, attr: attr(name)}
	}
	// specialCase / matching-type routing of a dedicated (or well-known) column
	routeColumn := func(c traceql.Condition, path string, colType traceql.StaticType, mk func(string, parquetquery.Predicate) item, groupMatching bool) (bool, error) {
		switch c.Op {
		case traceql.OpNone:
			g.add(path, nil, c.Attribute.Name, func(pr parquetquery.Predicate) item { return mk(c.Attribute.Name, pr) })
			return true, nil
		case traceql.OpExists:
			g.add(path, &parquetquery.SkipNilsPredicate{}, c.Attribute.Name, func(pr parquetquery.Predicate) item { return mk(c.Attribute.Name, pr) })
			return true, nil
		case traceql.OpNotExists:
			it := mk(c.Attribute.Name, parquetquery.NewNilValuePredicate())
			if d, ok := it.(*dedItem); ok {
				d.nilOp, d.pred = true, nil
			}
			extra = append(extra, it)
			return true, nil
		}
		if !isMatchingColumnType(colType, operandType(c.Operands)) {
			return false, nil
		}
		pred, err := createPredicate(c.Op, c.Operands)
		if err != nil {
			return false, fmt.Errorf("creating predicate: %w", err)
		}
		if groupMatching {
			g.add(path, pred, c.Attribute.Name, func(pr parquetquery.Predicate) item { return mk(c.Attribute.Name, pr) })
		} else {
			extra = append(extra, mk(c.Attribute.Name, pred))
		}
		return true, nil
	}
	for _, c := range conds {
		if l == lvSpan || l == lvEvent || l == lvLink || l == lvScope {
			handled, err := p.intrinsic(l, c, &g, &extra, &explL, &explR, &explP)
			if err != nil {
				return err
			}
			if handled {
				continue
			}
		}
		if l == lvRes && c.Attribute.Name == vparquet5.LabelServiceName {
			mk := func(name string, pr parquetquery.Predicate) item {
				return &colItem{t: cg.TResources, c: cg.ResServiceName, pred: pr, put: func(e *emitter, v parquet.Value) {
					e.add(attr(name), traceql.NewStaticString(bstr(v.ByteArray())))
				}}
			}
			ok, err := routeColumn(c, "service.name", traceql.TypeString, mk, false)
			if err != nil {
				return err
			}
			if ok {
				continue
			}
		}
		if cl >= 0 && p.ded[cl] != nil {
			if vt, ok := p.ded[cl][c.Attribute.Name]; ok {
				mk := func(name string, pr parquetquery.Predicate) item { return ded(name, pr, vt) }
				ok, err := routeColumn(c, "ded/"+c.Attribute.Name, staticTypeOf(vt), mk, true)
				if err != nil {
					return err
				}
				if ok {
					continue
				}
			}
		}
		if c.Op == traceql.OpNotExists {
			extra = append(extra, &genNilItem{level: cl, key: c.Attribute.Name, attr: attr(c.Attribute.Name)})
			continue
		}
		generic = append(generic, c)
	}
	if p.selectAll {
		p.selectAllColumns(l, &g)
	}
	gen, err := p.genericItem(l, generic)
	if err != nil {
		return err
	}
	lv.gen = gen
	for _, it := range extra {
		lv.add(it)
	}
	for _, path := range g.order {
		c := g.m[path]
		lv.add(c.mk(orIfNeeded(c.preds)))
	}
	return nil
}

// selectAllColumns adds SecondPassSelectAll's columns: intrinsics and every dedicated column.
func (p *plan) selectAllColumns(l int, g *groups) {
	switch l {
	case lvSpan:
		for _, in := range []traceql.Intrinsic{traceql.IntrinsicName, traceql.IntrinsicStatus, traceql.IntrinsicStatusMessage,
			traceql.IntrinsicDuration, traceql.IntrinsicKind, traceql.IntrinsicChildCount} {
			c := traceql.Condition{Attribute: traceql.NewIntrinsic(in), Op: traceql.OpNone}
			var ex []item
			var a, b, d bool
			_, _ = p.intrinsic(l, c, g, &ex, &a, &b, &d)
		}
	case lvScope:
		for _, in := range []traceql.Intrinsic{traceql.IntrinsicInstrumentationName, traceql.IntrinsicInstrumentationVersion} {
			c := traceql.Condition{Attribute: traceql.NewIntrinsic(in), Op: traceql.OpNone}
			var ex []item
			var a, b, d bool
			_, _ = p.intrinsic(l, c, g, &ex, &a, &b, &d)
			for _, it := range ex {
				p.lv[l].add(it)
			}
		}
	case lvRes:
		g.add("service.name", nil, vparquet5.LabelServiceName, func(pr parquetquery.Predicate) item {
			return &colItem{t: cg.TResources, c: cg.ResServiceName, pred: pr, put: func(e *emitter, v parquet.Value) {
				e.add(attrOf[lvRes](vparquet5.LabelServiceName), traceql.NewStaticString(bstr(v.ByteArray())))
			}}
		})
	}
	if cl := cgLevel[l]; cl >= 0 && (l == lvSpan || l == lvRes) {
		for name, vt := range p.ded[cl] {
			k, vt := p.kmKey(cl, name), vt
			k.want[vt] = true
			a := attrOf[l](name)
			g.add("ded/"+name, nil, name, func(pr parquetquery.Predicate) item {
				return &dedItem{k: k, vt: vt, pred: pr, attr: a}
			})
		}
	}
}

// genericItem is createAttributeIterator over the level's generic conditions.
func (p *plan) genericItem(l int, conds []traceql.Condition) (*genItem, error) {
	cl := cgLevel[l]
	if cl < 0 || (len(conds) == 0 && !(p.selectAll && l != lvEvent && l != lvLink)) {
		return nil, nil
	}
	g := &genItem{level: cl, attr: attrOf[l]}
	var preds [4][]parquetquery.Predicate
	seen := map[string]bool{}
	addKey := func(k string) {
		if seen[k] {
			return
		}
		seen[k] = true
		g.keys = append(g.keys, p.kmKey(cl, k))
		d := -1
		if vt, ok := p.ded[cl][k]; ok {
			d = int(vt)
		}
		g.ded = append(g.ded, d)
	}
	if p.selectAll {
		// select all: every key of the level, every value
		conds = nil
		for _, k := range p.footer.Keys[cl] {
			addKey(k)
		}
		for i := range preds {
			preds[i] = []parquetquery.Predicate{&parquetquery.SkipNilsPredicate{}}
		}
	}
	skipNils := &parquetquery.SkipNilsPredicate{}
	for _, c := range conds {
		addKey(c.Attribute.Name)
		switch c.Op {
		case traceql.OpNone:
			for i := range preds {
				preds[i] = append(preds[i], nil)
			}
			continue
		case traceql.OpExists:
			for i := range preds {
				preds[i] = append(preds[i], skipNils)
			}
			continue
		}
		var (
			pred parquetquery.Predicate
			err  error
			ti   int
		)
		switch c.Operands[0].Type {
		case traceql.TypeString, traceql.TypeStringArray:
			pred, err = createStringPredicate(c.Op, c.Operands)
			ti = tStr
		case traceql.TypeInt, traceql.TypeIntArray:
			pred, err = createIntPredicate(c.Op, c.Operands)
			ti = tInt
		case traceql.TypeFloat, traceql.TypeFloatArray:
			pred, err = createFloatPredicate(c.Op, c.Operands)
			ti = tFlt
		case traceql.TypeBoolean, traceql.TypeBooleanArray:
			pred, err = createBoolPredicate(c.Op, c.Operands)
			ti = tBool
		default:
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("creating attribute predicate: %w", err)
		}
		preds[ti] = append(preds[ti], pred)
	}
	n := 0
	for i := range preds {
		if len(preds[i]) > 0 {
			g.has[i] = true
			g.preds[i] = orIfNeeded(preds[i])
			n++
		}
	}
	if n == 0 {
		return nil, nil
	}
	g.join = p.all && n == 1
	scalar := [4]uint8{cg.VTStr, cg.VTInt, cg.VTDouble, cg.VTBool}
	for ki, k := range g.keys {
		if !g.join {
			k.wantAll()
			continue
		}
		for c, ok := range g.has {
			if ok && int(scalar[c]) != g.ded[ki] {
				k.want[scalar[c]] = true
			}
		}
	}
	return g, nil
}

// intrinsic routes the intrinsic conditions of the span, event, link and scope levels.
func (p *plan) intrinsic(l int, c traceql.Condition, g *groups, extra *[]item, explL, explR, explP *bool) (bool, error) {
	col := func(t, cid int, pred parquetquery.Predicate, put func(*emitter, parquet.Value)) item {
		return &colItem{t: t, c: cid, pred: pred, put: put}
	}
	group := func(path string, pred parquetquery.Predicate, t, cid int, put func(*emitter, parquet.Value)) {
		g.add(path, pred, path, func(pr parquetquery.Predicate) item { return col(t, cid, pr, put) })
	}
	in := c.Attribute.Intrinsic
	switch l {
	case lvEvent:
		switch in {
		case traceql.IntrinsicEventName:
			pred, err := createStringPredicate(c.Op, c.Operands)
			if err != nil {
				return true, err
			}
			*extra = append(*extra, col(cg.TEvents, cg.EvName, pred, func(e *emitter, v parquet.Value) {
				e.add(traceql.IntrinsicEventNameAttribute, traceql.NewStaticString(bstr(v.ByteArray())))
			}))
			return true, nil
		case traceql.IntrinsicEventTimeSinceStart:
			pred, err := createIntPredicate(c.Op, c.Operands)
			if err != nil {
				return true, err
			}
			*extra = append(*extra, col(cg.TEvents, cg.EvTime, pred, func(e *emitter, v parquet.Value) {
				e.add(traceql.IntrinsicEventTimeSinceStartAttribute, traceql.NewStaticDuration(time.Duration(v.Int64())))
			}))
			return true, nil
		}
	case lvLink:
		switch in {
		case traceql.IntrinsicLinkTraceID, traceql.IntrinsicLinkSpanID:
			isSpan := in == traceql.IntrinsicLinkSpanID
			pred, err := createBytesPredicate(c.Op, c.Operands, isSpan)
			if err != nil {
				return true, err
			}
			cid, a := cg.LnTraceID, traceql.NewIntrinsic(traceql.IntrinsicLinkTraceID)
			hex := traceIDHex
			if isSpan {
				cid, a, hex = cg.LnSpanID, traceql.NewIntrinsic(traceql.IntrinsicLinkSpanID), spanIDHex
			}
			*extra = append(*extra, col(cg.TLinks, cid, pred, func(e *emitter, v parquet.Value) {
				e.add(a, traceql.NewStaticString(hex(v.ByteArray())))
			}))
			return true, nil
		}
	case lvScope:
		switch in {
		case traceql.IntrinsicInstrumentationName, traceql.IntrinsicInstrumentationVersion:
			pred, err := createStringPredicate(c.Op, c.Operands)
			if err != nil {
				return true, err
			}
			cid, a := cg.ScName, traceql.IntrinsicInstrumentationNameAttribute
			if in == traceql.IntrinsicInstrumentationVersion {
				cid, a = cg.ScVersion, traceql.IntrinsicInstrumentationVersionAttribute
			}
			*extra = append(*extra, col(cg.TScopes, cid, pred, func(e *emitter, v parquet.Value) {
				e.add(a, traceql.NewStaticString(bstr(v.ByteArray())))
			}))
			return true, nil
		}
	case lvSpan:
		return p.spanIntrinsic(c, group, g, explL, explR, explP)
	}
	return false, nil
}

func (p *plan) spanIntrinsic(c traceql.Condition, group func(string, parquetquery.Predicate, int, int, func(*emitter, parquet.Value)), g *groups, explL, explR, explP *bool) (bool, error) {
	intPred := func() (parquetquery.Predicate, error) { return createIntPredicate(c.Op, c.Operands) }
	strPred := func() (parquetquery.Predicate, error) { return createStringPredicate(c.Op, c.Operands) }
	nilIfNot := func(path string, cid int, put func(*emitter, parquet.Value)) {
		if !g.hasNil(path) {
			group(path, nil, cg.TSpans, cid, put)
		}
	}
	putLeft := func(e *emitter, v parquet.Value) {
		e.sp.nestedSetLeft = v.Int32()
		if *explL {
			e.add(traceql.IntrinsicNestedSetLeftAttribute, traceql.NewStaticInt(int(v.Int32())))
		}
	}
	putRight := func(e *emitter, v parquet.Value) {
		e.sp.nestedSetRight = v.Int32()
		if *explR {
			e.add(traceql.IntrinsicNestedSetRightAttribute, traceql.NewStaticInt(int(v.Int32())))
		}
	}
	putParent := func(e *emitter, v parquet.Value) {
		e.sp.nestedSetParent = v.Int32()
		if *explP {
			e.add(traceql.IntrinsicNestedSetParentAttribute, traceql.NewStaticInt(int(v.Int32())))
		}
	}
	var (
		pred parquetquery.Predicate
		err  error
	)
	switch c.Attribute.Intrinsic {
	case traceql.IntrinsicSpanID:
		if pred, err = createBytesPredicate(c.Op, c.Operands, true); err == nil {
			group("span:id", pred, cg.TSpans, cg.SpSpanID, func(e *emitter, v parquet.Value) {
				e.sp.id = v.ByteArray()
				e.add(traceql.IntrinsicSpanIDAttribute, traceql.NewStaticString(spanIDHex(v.ByteArray())))
			})
		}
	case traceql.IntrinsicParentID:
		if pred, err = createBytesPredicate(c.Op, c.Operands, true); err == nil {
			group("parent:id", pred, cg.TSpans, cg.SpParentSpanID, func(e *emitter, v parquet.Value) {
				e.add(traceql.IntrinsicParentIDAttribute, traceql.NewStaticString(spanIDHex(v.ByteArray())))
			})
		}
	case traceql.IntrinsicSpanStartTime:
		if pred, err = intPred(); err == nil {
			cid, period := cg.SpStart, uint64(0)
			switch pr := c.Precision; {
			case pr >= 3600*time.Second && pr%(3600*time.Second) == 0:
				cid, period = cg.SpRounded3600, 3600
			case pr >= 300*time.Second && pr%(300*time.Second) == 0:
				cid, period = cg.SpRounded300, 300
			case pr >= 60*time.Second && pr%(60*time.Second) == 0:
				cid, period = cg.SpRounded60, 60
			case pr >= 15*time.Second && pr%(15*time.Second) == 0:
				cid, period = cg.SpRounded15, 15
			}
			p.spanStart = true
			if period == 0 {
				group("start", pred, cg.TSpans, cid, func(e *emitter, v parquet.Value) { e.sp.startTimeUnixNanos = v.Uint64() })
			} else {
				m := traceql.NewIntervalMapper(0, 0xF000000000000000, period*uint64(time.Second), false)
				group(fmt.Sprintf("start%d", period), pred, cg.TSpans, cid, func(e *emitter, v parquet.Value) {
					e.sp.startTimeUnixNanos = m.TimestampOf(int(uint32(v.Int32())))
				})
			}
		}
	case traceql.IntrinsicName:
		if pred, err = strPred(); err == nil {
			group("name", pred, cg.TSpans, cg.SpName, func(e *emitter, v parquet.Value) {
				e.add(traceql.IntrinsicNameAttribute, traceql.NewStaticString(bstr(v.ByteArray())))
			})
		}
	case traceql.IntrinsicKind:
		if pred, err = intPred(); err == nil {
			group("kind", pred, cg.TSpans, cg.SpKind, func(e *emitter, v parquet.Value) {
				e.add(traceql.IntrinsicKindAttribute, traceql.NewStaticKind(otlpKindToTraceqlKind(v.Uint64())))
			})
		}
	case traceql.IntrinsicDuration:
		if pred, err = createDurationPredicate(c.Op, c.Operands); err == nil {
			group("duration", pred, cg.TSpans, cg.SpDuration, func(e *emitter, v parquet.Value) {
				e.sp.durationNanos = v.Uint64()
				e.add(traceql.IntrinsicDurationAttribute, traceql.NewStaticDuration(time.Duration(v.Uint64())))
			})
		}
	case traceql.IntrinsicStatus:
		if pred, err = intPred(); err == nil {
			group("status", pred, cg.TSpans, cg.SpStatusCode, func(e *emitter, v parquet.Value) {
				e.add(traceql.IntrinsicStatusAttribute, traceql.NewStaticStatus(otlpStatusToTraceqlStatus(v.Uint64())))
			})
		}
	case traceql.IntrinsicStatusMessage:
		if pred, err = strPred(); err == nil {
			group("statusMessage", pred, cg.TSpans, cg.SpStatusMessage, func(e *emitter, v parquet.Value) {
				e.add(traceql.IntrinsicStatusMessageAttribute, traceql.NewStaticString(bstr(v.ByteArray())))
			})
		}
	case traceql.IntrinsicStructuralDescendant:
		nilIfNot("nsLeft", cg.SpNestedLeft, putLeft)
		nilIfNot("nsRight", cg.SpNestedRight, putRight)
	case traceql.IntrinsicStructuralChild:
		nilIfNot("nsLeft", cg.SpNestedLeft, putLeft)
		nilIfNot("nsParent", cg.SpParentID, putParent)
	case traceql.IntrinsicStructuralSibling:
		nilIfNot("nsParent", cg.SpParentID, putParent)
	case traceql.IntrinsicNestedSetLeft:
		*explL = true
		if pred, err = intPred(); err == nil {
			group("nsLeft", pred, cg.TSpans, cg.SpNestedLeft, putLeft)
		}
	case traceql.IntrinsicNestedSetRight:
		*explR = true
		if pred, err = intPred(); err == nil {
			group("nsRight", pred, cg.TSpans, cg.SpNestedRight, putRight)
		}
	case traceql.IntrinsicNestedSetParent:
		*explP = true
		if pred, err = intPred(); err == nil {
			group("nsParent", pred, cg.TSpans, cg.SpParentID, putParent)
		}
	case traceql.IntrinsicChildCount:
		if pred, err = intPred(); err == nil {
			group("childCount", pred, cg.TSpans, cg.SpChildCount, func(e *emitter, v parquet.Value) {
				e.add(traceql.IntrinsicChildCountAttribute, traceql.NewStaticInt(int(v.Int32())))
			})
		}
	case traceql.IntrinsicSpanMultiplier:
		group("traceState", nil, cg.TSpans, cg.SpTraceState, func(e *emitter, v parquet.Value) {
			m := sampling.MultiplierFromTraceState(bstr(v.ByteArray()))
			if m <= 0 {
				m = 1.0
			}
			e.add(traceql.IntrinsicSpanMultiplierAttribute, traceql.NewStaticFloat(m))
		})
	default:
		return false, nil
	}
	return true, err
}

// compileTrace is createTraceIterator: trace intrinsics (each its own iterator) and the time
// range filter (always required).
func (p *plan) compileTrace(conds []traceql.Condition) error {
	lv := p.lv[lvTrace]
	col := func(cid int, pred parquetquery.Predicate, put func(*emitter, parquet.Value)) item {
		return &colItem{t: cg.TTraces, c: cid, pred: pred, put: put}
	}
	putID := func(e *emitter, v parquet.Value) {
		e.tr.id = v.ByteArray()
		e.add(traceql.IntrinsicTraceIDAttribute, traceql.NewStaticString(traceIDHex(v.ByteArray())))
	}
	putDur := func(e *emitter, v parquet.Value) {
		e.tr.dur = v.Uint64()
		e.add(traceql.IntrinsicTraceDurationAttribute, traceql.NewStaticDuration(time.Duration(v.Uint64())))
	}
	putRootSpan := func(e *emitter, v parquet.Value) {
		e.tr.rootSpan = bstr(v.ByteArray())
		e.add(traceql.IntrinsicTraceRootSpanAttribute, traceql.NewStaticString(e.tr.rootSpan))
	}
	putRootSvc := func(e *emitter, v parquet.Value) {
		e.tr.rootService = bstr(v.ByteArray())
		e.add(traceql.IntrinsicTraceRootServiceAttribute, traceql.NewStaticString(e.tr.rootService))
	}
	putStart := func(e *emitter, v parquet.Value) { e.tr.start = v.Uint64() }
	if p.selectAll {
		lv.add(col(cg.TrRootService, nil, putRootSvc))
		lv.add(col(cg.TrRootSpan, nil, putRootSpan))
		lv.add(col(cg.TrDuration, nil, putDur))
		lv.add(col(cg.TrTraceID, nil, putID))
	} else {
		for _, c := range conds {
			var (
				pred parquetquery.Predicate
				err  error
			)
			switch c.Attribute.Intrinsic {
			case traceql.IntrinsicTraceID:
				if c.Op == traceql.OpNone && c.CallBack != nil {
					lv.items = append(lv.items, col(cg.TrTraceID, parquetquery.NewCallbackPredicate(c.CallBack), putID))
					lv.opt = append(lv.opt, true)
					continue
				}
				if pred, err = createBytesPredicate(c.Op, c.Operands, false); err == nil {
					lv.add(col(cg.TrTraceID, pred, putID))
				}
			case traceql.IntrinsicTraceDuration:
				if pred, err = createDurationPredicate(c.Op, c.Operands); err == nil {
					lv.add(col(cg.TrDuration, pred, putDur))
				}
			case traceql.IntrinsicTraceStartTime:
				if p.start == 0 && p.end == 0 {
					lv.add(col(cg.TrStart, nil, putStart))
				}
			case traceql.IntrinsicTraceRootSpan:
				if pred, err = createStringPredicate(c.Op, c.Operands); err == nil {
					lv.add(col(cg.TrRootSpan, pred, putRootSpan))
				}
			case traceql.IntrinsicTraceRootService:
				if pred, err = createStringPredicate(c.Op, c.Operands); err == nil {
					lv.add(col(cg.TrRootService, pred, putRootSvc))
				}
			case traceql.IntrinsicServiceStats:
				lv.add(&serviceStatsItem{})
			}
			if err != nil {
				return err
			}
		}
	}
	if p.start > 0 && p.end > 0 {
		p.timeStart = col(cg.TrStart, parquetquery.NewIntBetweenPredicate(0, int64(p.end)), putStart)
		p.timeEnd = col(cg.TrEnd, parquetquery.NewIntBetweenPredicate(int64(p.start), math.MaxInt64), func(*emitter, parquet.Value) {})
	}
	return nil
}

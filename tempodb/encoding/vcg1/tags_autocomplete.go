package vcg1

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/parquet-go/parquet-go"

	"github.com/grafana/tempo/v3/pkg/parquetquery"
	"github.com/grafana/tempo/v3/pkg/traceql"
	"github.com/grafana/tempo/v3/tempodb/backend"
	"github.com/grafana/tempo/v3/tempodb/encoding/common"
	cg "github.com/grafana/tempo/v3/tempodb/encoding/vcg1/cgfile"
	"github.com/grafana/tempo/v3/tempodb/encoding/vparquet5"
)

// FetchTagNames and FetchTagValues follow vparquet5's autocomplete (block_autocomplete.go):
// per condition group, one iterator per level (event, link, span, scope, resource, trace),
// each the AND of that level's iterators and of "some child matches" for the levels nested in
// it. Routing (intrinsic, well-known, dedicated, generic, `= nil`), predicates, the OR of
// predicates sharing a column, the generic conditions vparquet5 ignores, which iterators
// select the tag's values, the value types and the unfiltered dedicated / well-known names
// are vparquet5's. Where vparquet5's iterators lose matches (see the comparison test) this
// returns what its join means:
//   - events and links are each joined to their span (vparquet5 joins the event and link
//     iterators to each other by position under the span, so with both present a span with
//     events but no links, or the reverse, drops out);
//   - a value seen first on an entity whose parents do not match is still reported when a
//     matching entity has it (vparquet5's per-level dedupe runs before the parent join);
//   - a generic array attribute reports all its elements (vparquet5 reports the last one).

func (b *backendBlock) FetchTagNames(ctx context.Context, req traceql.FetchTagsRequest, cb traceql.FetchTagsCallback, mcb common.MetricsCallback, opts common.SearchOptions) error {
	unfiltered := func() error {
		return b.SearchTags(ctx, req.Scope, func(t string, s traceql.AttributeScope) { cb(t, s) }, mcb, opts)
	}
	if len(req.ConditionGroups) == 0 {
		return unfiltered()
	}
	for _, g := range req.ConditionGroups {
		if err := checkConditions(g); err != nil {
			return fmt.Errorf("conditions invalid: %w", err)
		}
		_, mingled, err := acCategorize(g)
		if err != nil {
			return err
		}
		if (len(req.ConditionGroups) == 1 && len(g) < 1) || mingled {
			return unfiltered()
		}
	}
	r, err := b.tagRequest(ctx, opts)
	if err != nil {
		return err
	}
	defer func() { mcb(r.bytes()) }()
	plans, err := acCompileAll(r, b.meta.DedicatedColumns, req.ConditionGroups, traceql.Attribute{}, true, req.Scope)
	if err != nil {
		return err
	}
	if plans == nil {
		return r.searchTags(req.Scope, func(t string, s traceql.AttributeScope) bool { cb(t, s); return false })
	}
	sent := map[tagNameKey]struct{}{}
	run := &acRun{r: r, plans: plans, name: func(n string, s traceql.AttributeScope) bool {
		k := tagNameKey{n, s}
		if _, ok := sent[k]; ok {
			return false
		}
		sent[k] = struct{}{}
		return cb(n, s)
	}}
	stopped, err := run.exec()
	if err != nil || stopped {
		return err
	}
	r.specialTagNames(req.Scope, cb)
	return nil
}

func (b *backendBlock) FetchTagValues(ctx context.Context, req traceql.FetchTagValuesRequest, cb traceql.FetchTagValuesCallback, mcb common.MetricsCallback, opts common.SearchOptions) error {
	unfiltered := func() error {
		return b.SearchTagValuesV2(ctx, req.TagName, common.TagValuesCallbackV2(cb), mcb, opts)
	}
	if len(req.ConditionGroups) == 0 {
		return unfiltered()
	}
	for _, g := range req.ConditionGroups {
		if err := checkConditions(g); err != nil {
			return fmt.Errorf("conditions invalid: %w", err)
		}
		_, mingled, err := acCategorize(g)
		if err != nil {
			return err
		}
		// <= 1: the engine always adds an OpNone condition for the tag
		if (len(req.ConditionGroups) == 1 && len(g) <= 1) || mingled {
			return unfiltered()
		}
	}
	r, err := b.tagRequest(ctx, opts)
	if err != nil {
		return err
	}
	defer func() { mcb(r.bytes()) }()
	plans, err := acCompileAll(r, b.meta.DedicatedColumns, req.ConditionGroups, req.TagName, false, traceql.AttributeScopeNone)
	if err != nil {
		return err
	}
	if plans == nil {
		_, err := r.searchTagValues(req.TagName, cb)
		return err
	}
	sent := map[traceql.StaticMapKey]struct{}{}
	run := &acRun{r: r, plans: plans, value: func(v traceql.Static) bool {
		k := v.MapKey()
		if _, ok := sent[k]; ok {
			return false
		}
		sent[k] = struct{}{}
		return cb(v)
	}}
	_, err = run.exec()
	return err
}

type tagNameKey struct {
	name  string
	scope traceql.AttributeScope
}

// specialTagNames is vparquet5's tagNamesForSpecialColumns: the well-known and dedicated
// columns with any value in the whole block, whatever the conditions and the shard.
func (r *tagReq) specialTagNames(scope traceql.AttributeScope, cb traceql.FetchTagsCallback) {
	in := func(s traceql.AttributeScope) bool { return scope == traceql.AttributeScopeNone || scope == s }
	if in(traceql.AttributeScopeResource) && r.q.F.Rows[cg.TResources] > 0 {
		if cb(vparquet5.LabelServiceName, traceql.AttributeScopeResource) {
			return
		}
	}
	for _, s := range []struct {
		l     int
		scope traceql.AttributeScope
		ds    backend.DedicatedColumnScope
	}{
		{cg.LSpan, traceql.AttributeScopeSpan, backend.DedicatedColumnScopeSpan},
		{cg.LResource, traceql.AttributeScopeResource, backend.DedicatedColumnScopeResource},
		{cg.LEvent, traceql.AttributeScopeEvent, backend.DedicatedColumnScopeEvent},
	} {
		if !in(s.scope) {
			continue
		}
		for _, name := range dedicatedNames(r.ded[s.l]) {
			if r.anyRun(s.l, name, r.ded[s.l][name]) && cb(name, s.scope) {
				return
			}
		}
	}
}

// dedicatedNames are the keys of a dedicated mapping in a stable order.
func dedicatedNames(m map[string]uint8) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// anyRun reports whether the block has a scan-copy entry of key with value type vt.
func (r *tagReq) anyRun(l int, key string, vt uint8) bool {
	id, ok := r.q.F.KeyID(l, key)
	if !ok {
		return false
	}
	km, err := r.q.Km(l)
	if err != nil {
		return false
	}
	for fi := range km {
		a, b := km[fi].KeyRuns(id)
		for i := a; i < b; i++ {
			if km[fi].Runs[i].VType == vt {
				return true
			}
		}
	}
	return false
}

// ---- compilation ----

var acTable = [nLv]int{cg.TTraces, cg.TResources, cg.TScopes, cg.TSpans, cg.TEvents, cg.TLinks}

var acScope = [nLv]traceql.AttributeScope{
	traceql.AttributeScopeUnknown, traceql.AttributeScopeResource, traceql.AttributeScopeInstrumentation,
	traceql.AttributeScopeSpan, traceql.AttributeScopeEvent, traceql.AttributeScopeLink,
}

// acDepth is a level's depth in the span tree (events and links are both children of spans).
var acDepth = [nLv]int{0, 1, 2, 3, 4, 4}

// acCategorize is vparquet5's categorizeConditions.
func acCategorize(conds []traceql.Condition) ([nLv][]traceql.Condition, bool, error) {
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
			return cats, false, fmt.Errorf("unsupported traceql scope: %s", c.Attribute)
		}
		cats[lv] = append(cats[lv], c)
	}
	return cats, mingled, nil
}

// acCol is a column iterator: a fixed column, a dedicated column (its key's entries of the
// column type) or the well-known resource.service.name. pred nil keeps every row, nulls too.
type acCol struct {
	t, c int
	k    *kmKey // dedicated column
	vt   uint8
	pred parquetquery.Predicate
	sel  bool // yields the tag's values
	mapv func(parquet.Value) traceql.Static

	cur    *cursor
	kc     int
	memo   strMemo
	loaded bool
}

// acFilt is one generic condition (a key/value join at the attribute level): an attribute
// with the key whose elements of the operand's type (none: one null) pass pred.
type acFilt struct {
	k     *kmKey
	class int
	pred  parquetquery.Predicate
	ded   int // dedicated column type of the key at this level (not a generic attribute), or -1
	null  bool
	kc    int
}

type acLevel struct {
	cols    []*acCol
	filts   []*acFilt
	tagKeys []*kmKey // generic attributes of the tag: required, their values selected
	tagDed  []int
	nils    []string // generic `= nil`
	names   bool     // tag names requested at this level

	match []bool // per window-local owner row
	null  bool   // the null event/link row of a span without any
	alive []bool
}

func (l *acLevel) own() bool {
	return len(l.cols)+len(l.filts)+len(l.tagKeys)+len(l.nils) > 0 || l.names
}

type acPlan struct {
	lv   [nLv]*acLevel
	keys map[string]*kmKey
	f    *cg.Footer
	ded  [cg.NumLevels]map[string]uint8
	hi   int // depth of the outermost iterator
	lo   int // depth of the innermost iterator
}

func (p *acPlan) kmKey(l int, key string) *kmKey {
	k := fmt.Sprintf("%d/%s", l, key)
	if x := p.keys[k]; x != nil {
		return x
	}
	x := &kmKey{level: l, key: key}
	x.id, x.ok = p.f.KeyID(l, key)
	p.keys[k] = x
	return x
}

// acCompileAll compiles every group; nil when some group has no iterator (vparquet5 then
// answers unfiltered).
func acCompileAll(r *tagReq, dc backend.DedicatedColumns, groups [][]traceql.Condition, tag traceql.Attribute, names bool, scope traceql.AttributeScope) ([]*acPlan, error) {
	out := make([]*acPlan, 0, len(groups))
	for _, g := range groups {
		p, err := acCompile(r, g, tag, names, scope)
		if err != nil {
			return nil, err
		}
		if p == nil {
			return nil, nil
		}
		out = append(out, p)
	}
	return out, nil
}

// colGroups is vparquet5's columnPredicates/columnSelectAs: the conditions on one column share
// an iterator with the OR of their predicates, selected as the last of them decides.
type colGroups struct {
	order []string
	m     map[string]*acCol
	preds map[string][]parquetquery.Predicate
}

func (g *colGroups) add(path string, mk func() *acCol, pred parquetquery.Predicate, sel bool) {
	if g.m == nil {
		g.m, g.preds = map[string]*acCol{}, map[string][]parquetquery.Predicate{}
	}
	c := g.m[path]
	if c == nil {
		c = mk()
		g.m[path] = c
		g.order = append(g.order, path)
	}
	g.preds[path] = append(g.preds[path], pred)
	c.sel = sel
}

func (g *colGroups) cols() []*acCol {
	out := make([]*acCol, 0, len(g.order))
	for _, path := range g.order {
		c := g.m[path]
		c.pred = orIfNeeded(g.preds[path])
		out = append(out, c)
	}
	return out
}

func mapStr(v parquet.Value) traceql.Static { return traceql.NewStaticString(bstr(v.ByteArray())) }

// mapKind is the value of a dedicated / well-known column by physical type.
func mapKind(v parquet.Value) traceql.Static {
	switch v.Kind() {
	case parquet.Boolean:
		return traceql.NewStaticBool(v.Boolean())
	case parquet.Int32, parquet.Int64:
		return traceql.NewStaticInt(int(v.Int64()))
	case parquet.Float, parquet.Double:
		return traceql.NewStaticFloat(v.Double())
	case parquet.ByteArray, parquet.FixedLenByteArray:
		return traceql.NewStaticString(bstr(v.ByteArray()))
	}
	return traceql.NewStaticNil()
}

func mapNil(parquet.Value) traceql.Static { return traceql.NewStaticNil() }

func acCompile(r *tagReq, conds []traceql.Condition, tag traceql.Attribute, names bool, scope traceql.AttributeScope) (*acPlan, error) {
	cats, _, err := acCategorize(conds)
	if err != nil {
		return nil, err
	}
	p := &acPlan{keys: map[string]*kmKey{}, f: r.q.F, ded: r.ded}
	exact := func(a traceql.Attribute) bool { return a == tag }
	byName := func(a traceql.Attribute) bool {
		return a == tag || (a.Name == tag.Name && tag.Scope == traceql.AttributeScopeNone)
	}
	keysReq := func(l int) bool {
		return names && (scope == traceql.AttributeScopeNone || scope == acScope[l])
	}
	fixed := func(t, c int, pred parquetquery.Predicate, sel bool, mapv func(parquet.Value) traceql.Static) *acCol {
		return &acCol{t: t, c: c, pred: pred, sel: sel, mapv: mapv}
	}
	ded := func(l int, name string, vt uint8) func() *acCol {
		return func() *acCol {
			k := p.kmKey(cgLevel[l], name)
			k.want[vt] = true
			return &acCol{t: -1, k: k, vt: vt, mapv: mapKind}
		}
	}
	for l := range p.lv {
		lv := &acLevel{names: keysReq(l) && l != lvTrace}
		p.lv[l] = lv
		var (
			g       colGroups
			generic []traceql.Condition
		)
		// routeDed is the dedicated-column branch shared by span, resource and event levels.
		routeDed := func(c traceql.Condition, sel func(traceql.Attribute) bool) (bool, error) {
			vt, ok := r.ded[cgLevel[l]][c.Attribute.Name]
			if !ok {
				return false, nil
			}
			path := "ded/" + c.Attribute.Name
			mk := ded(l, c.Attribute.Name, vt)
			switch c.Op {
			case traceql.OpNone:
				g.add(path, mk, nil, sel(c.Attribute))
				return true, nil
			case traceql.OpExists:
				g.add(path, mk, &parquetquery.SkipNilsPredicate{}, sel(c.Attribute))
				return true, nil
			case traceql.OpNotExists:
				g.add(path, mk, parquetquery.NewNilValuePredicate(), sel(c.Attribute))
				return true, nil
			}
			if !isMatchingColumnType(staticTypeOf(vt), operandType(c.Operands)) {
				return false, nil
			}
			pred, err := createPredicate(c.Op, c.Operands)
			if err != nil {
				return true, fmt.Errorf("creating predicate: %w", err)
			}
			g.add(path, mk, pred, sel(c.Attribute))
			return true, nil
		}
		for _, c := range cats[l] {
			handled, err := func() (bool, error) {
				switch l {
				case lvEvent:
					if c.Attribute.Intrinsic == traceql.IntrinsicEventName {
						pred, err := createStringPredicate(c.Op, c.Operands)
						if err != nil {
							return true, err
						}
						lv.cols = append(lv.cols, fixed(cg.TEvents, cg.EvName, pred, exact(c.Attribute), mapStr))
						return true, nil
					}
					if ok, err := routeDed(c, exact); ok || err != nil {
						return ok, err
					}
				case lvLink:
					switch c.Attribute.Intrinsic {
					case traceql.IntrinsicLinkTraceID, traceql.IntrinsicLinkSpanID:
						pred, err := createBytesPredicate(c.Op, c.Operands, false)
						if err != nil {
							return true, err
						}
						cid := cg.LnTraceID
						if c.Attribute.Intrinsic == traceql.IntrinsicLinkSpanID {
							cid = cg.LnSpanID
						}
						lv.cols = append(lv.cols, fixed(cg.TLinks, cid, pred, false, mapNil))
						return true, nil
					}
					if len(c.Operands) == 0 && c.Op == traceql.OpNotExists {
						lv.nils = append(lv.nils, c.Attribute.Name)
						return true, nil
					}
					return false, nil
				case lvSpan:
					if ok, err := acSpanIntrinsic(c, &g, byName); ok || err != nil {
						return ok, err
					}
					if ok, err := routeDed(c, byName); ok || err != nil {
						return ok, err
					}
				case lvScope:
					switch c.Attribute.Intrinsic {
					case traceql.IntrinsicInstrumentationName, traceql.IntrinsicInstrumentationVersion:
						pred, err := createStringPredicate(c.Op, c.Operands)
						if err != nil {
							return true, err
						}
						cid := cg.ScName
						if c.Attribute.Intrinsic == traceql.IntrinsicInstrumentationVersion {
							cid = cg.ScVersion
						}
						lv.cols = append(lv.cols, fixed(cg.TScopes, cid, pred, exact(c.Attribute), mapStr))
						return true, nil
					}
				case lvRes:
					if c.Attribute.Name == vparquet5.LabelServiceName {
						mk := func() *acCol { return fixed(cg.TResources, cg.ResServiceName, nil, false, mapKind) }
						switch c.Op {
						case traceql.OpNone:
							g.add("service.name", mk, nil, byName(c.Attribute))
							return true, nil
						case traceql.OpExists:
							g.add("service.name", mk, &parquetquery.SkipNilsPredicate{}, byName(c.Attribute))
							return true, nil
						}
						if isMatchingColumnType(traceql.TypeString, operandType(c.Operands)) {
							pred, err := createPredicate(c.Op, c.Operands)
							if err != nil {
								return true, fmt.Errorf("creating predicate: %w", err)
							}
							lv.cols = append(lv.cols, fixed(cg.TResources, cg.ResServiceName, pred, exact(c.Attribute), mapKind))
							return true, nil
						}
					}
					if ok, err := routeDed(c, byName); ok || err != nil {
						return ok, err
					}
				case lvTrace:
					var (
						pred parquetquery.Predicate
						err  error
						cid  int
						mapv func(parquet.Value) traceql.Static
					)
					switch c.Attribute.Intrinsic {
					case traceql.IntrinsicTraceID:
						pred, err = createBytesPredicate(c.Op, c.Operands, false)
						cid, mapv = cg.TrTraceID, func(v parquet.Value) traceql.Static { return traceql.NewStaticString(traceIDHex(v.ByteArray())) }
					case traceql.IntrinsicTraceDuration:
						pred, err = createIntPredicate(c.Op, c.Operands)
						cid, mapv = cg.TrDuration, func(v parquet.Value) traceql.Static { return traceql.NewStaticDuration(time.Duration(v.Int64())) }
					case traceql.IntrinsicTraceRootSpan:
						pred, err = createStringPredicate(c.Op, c.Operands)
						cid, mapv = cg.TrRootSpan, mapStr
					case traceql.IntrinsicTraceRootService:
						pred, err = createStringPredicate(c.Op, c.Operands)
						cid, mapv = cg.TrRootService, mapStr
					default: // traceStartTime and the rest: nothing to fetch
						return true, nil
					}
					if err != nil {
						return true, err
					}
					lv.cols = append(lv.cols, fixed(cg.TTraces, cid, pred, exact(c.Attribute), mapv))
					return true, nil
				}
				if c.Op == traceql.OpNotExists {
					lv.nils = append(lv.nils, c.Attribute.Name)
					return true, nil
				}
				return false, nil
			}()
			if err != nil {
				return nil, err
			}
			if !handled {
				generic = append(generic, c)
			}
		}
		lv.cols = append(lv.cols, g.cols()...)
		if err := p.genericIter(l, lv, generic, byName); err != nil {
			return nil, err
		}
	}
	p.hi, p.lo = -1, -1
	for l, lv := range p.lv {
		if !lv.own() {
			continue
		}
		d := acDepth[l]
		if p.hi < 0 || d < p.hi {
			p.hi = d
		}
		if d > p.lo {
			p.lo = d
		}
	}
	if p.hi < 0 {
		return nil, nil
	}
	if p.lv[lvEvent].own() && p.lv[lvLink].own() {
		p.hi = min(p.hi, acDepth[lvSpan])
	}
	return p, nil
}

// acSpanIntrinsic routes the span intrinsics vparquet5's autocomplete reads.
func acSpanIntrinsic(c traceql.Condition, g *colGroups, sel func(traceql.Attribute) bool) (bool, error) {
	var (
		pred parquetquery.Predicate
		err  error
		cid  int
		path string
		mapv func(parquet.Value) traceql.Static
	)
	switch c.Attribute.Intrinsic {
	case traceql.IntrinsicSpanID:
		pred, err = createBytesPredicate(c.Op, c.Operands, true)
		cid, path, mapv = cg.SpSpanID, "span:id", func(v parquet.Value) traceql.Static { return traceql.NewStaticString(spanIDHex(v.ByteArray())) }
	case traceql.IntrinsicSpanStartTime, traceql.IntrinsicStructuralDescendant, traceql.IntrinsicStructuralChild, traceql.IntrinsicStructuralSibling:
		return true, nil
	case traceql.IntrinsicName:
		pred, err = createStringPredicate(c.Op, c.Operands)
		cid, path, mapv = cg.SpName, "name", mapStr
	case traceql.IntrinsicKind:
		pred, err = createIntPredicate(c.Op, c.Operands)
		cid, path, mapv = cg.SpKind, "kind", func(v parquet.Value) traceql.Static {
			return traceql.NewStaticKind(otlpKindToTraceqlKind(v.Uint64()))
		}
	case traceql.IntrinsicDuration:
		pred, err = createIntPredicate(c.Op, c.Operands)
		cid, path, mapv = cg.SpDuration, "duration", func(v parquet.Value) traceql.Static {
			return traceql.NewStaticDuration(time.Duration(v.Int64()))
		}
	case traceql.IntrinsicStatus:
		pred, err = createIntPredicate(c.Op, c.Operands)
		cid, path, mapv = cg.SpStatusCode, "status", func(v parquet.Value) traceql.Static {
			return traceql.NewStaticStatus(otlpStatusToTraceqlStatus(v.Uint64()))
		}
	case traceql.IntrinsicStatusMessage:
		pred, err = createStringPredicate(c.Op, c.Operands)
		cid, path, mapv = cg.SpStatusMessage, "statusMessage", mapStr
	case traceql.IntrinsicNestedSetLeft:
		pred, err = createIntPredicate(c.Op, c.Operands)
		cid, path, mapv = cg.SpNestedLeft, "nsLeft", mapNil
	case traceql.IntrinsicNestedSetRight:
		pred, err = createIntPredicate(c.Op, c.Operands)
		cid, path, mapv = cg.SpNestedRight, "nsRight", mapNil
	case traceql.IntrinsicNestedSetParent:
		pred, err = createIntPredicate(c.Op, c.Operands)
		cid, path, mapv = cg.SpParentID, "nsParent", mapNil
	case traceql.IntrinsicChildCount:
		pred, err = createIntPredicate(c.Op, c.Operands)
		cid, path, mapv = cg.SpChildCount, "childCount", mapKind
	default:
		return false, nil
	}
	if err != nil {
		return true, err
	}
	g.add(path, func() *acCol { return &acCol{t: cg.TSpans, c: cid, mapv: mapv} }, pred, sel(c.Attribute))
	return true, nil
}

// genericIter is createDistinctAttributeIterator: generic conditions with a typed operand are
// key/value filters; OpNone/OpExists only count for the tag (the attribute must exist, its
// values are selected); other operand types are ignored.
func (p *acPlan) genericIter(l int, lv *acLevel, conds []traceql.Condition, isTag func(traceql.Attribute) bool) error {
	cl := cgLevel[l]
	if cl < 0 {
		return nil
	}
	dedOf := func(name string) int {
		if vt, ok := p.ded[cl][name]; ok {
			return int(vt)
		}
		return -1
	}
	scalar := [4]uint8{cg.VTStr, cg.VTInt, cg.VTDouble, cg.VTBool}
	seenTag := map[string]bool{}
	for _, c := range conds {
		if c.Op == traceql.OpNone || c.Op == traceql.OpExists {
			if isTag(c.Attribute) && !seenTag[c.Attribute.Name] {
				seenTag[c.Attribute.Name] = true
				k := p.kmKey(cl, c.Attribute.Name)
				k.wantAll()
				lv.tagKeys = append(lv.tagKeys, k)
				lv.tagDed = append(lv.tagDed, dedOf(c.Attribute.Name))
			}
			continue
		}
		var (
			pred  parquetquery.Predicate
			err   error
			class int
		)
		switch c.Operands[0].Type {
		case traceql.TypeString, traceql.TypeStringArray:
			pred, err = createStringPredicate(c.Op, c.Operands)
			class = tStr
		case traceql.TypeInt, traceql.TypeIntArray:
			pred, err = createIntPredicate(c.Op, c.Operands)
			class = tInt
		case traceql.TypeFloat, traceql.TypeFloatArray:
			pred, err = createFloatPredicate(c.Op, c.Operands)
			class = tFlt
		case traceql.TypeBoolean, traceql.TypeBooleanArray:
			pred, err = createBoolPredicate(c.Op, c.Operands)
			class = tBool
		default:
			continue
		}
		if err != nil {
			return fmt.Errorf("creating attribute predicate: %w", err)
		}
		k := p.kmKey(cl, c.Attribute.Name)
		fl := &acFilt{k: k, class: class, pred: pred, ded: dedOf(c.Attribute.Name)}
		fl.null = pred == nil || pred.KeepValue(parquet.Value{})
		if fl.null {
			k.wantAll()
		} else {
			k.want[scalar[class]] = true
		}
		lv.filts = append(lv.filts, fl)
	}
	return nil
}

// ---- evaluation ----

type acRun struct {
	r     *tagReq
	plans []*acPlan
	name  func(string, traceql.AttributeScope) bool
	value func(traceql.Static) bool
	live  map[cg.RunRef]struct{}
}

func (x *acRun) exec() (bool, error) {
	x.live = map[cg.RunRef]struct{}{}
	for _, wr := range windows(x.r.q.F, x.r.a, x.r.e) {
		w := newWindow(x.r.q, wr[0], wr[1])
		for _, p := range x.plans {
			stop, err := x.window(p, w)
			if err != nil || stop {
				return stop, err
			}
		}
		x.release(w)
	}
	return false, nil
}

// release drops what no later window needs (as the fetcher does).
func (x *acRun) release(w *window) {
	q := x.r.q
	for t := 0; t < cg.NumTables; t++ {
		q.DropPagesBefore(t, w.hi[t])
	}
	for _, p := range x.plans {
		for _, k := range p.keys {
			for _, ref := range k.refs {
				x.live[ref] = struct{}{}
			}
		}
	}
	for ref := range x.live {
		km, err := q.Km(ref.Level)
		if err != nil {
			continue
		}
		fl := km[ref.Flush]
		if fl.Owner0+fl.Owners <= w.hi[cg.OwnerTable[ref.Level]] {
			q.DropRun(ref)
			delete(x.live, ref)
		}
	}
}

// chain are the window-local child offsets between the levels a plan spans.
type chain struct {
	trRes, resSc, scSp, spEv, spLn []uint32
}

func (x *acRun) window(p *acPlan, w *window) (bool, error) {
	ctx := x.r.ctx
	ld := newLoader(w)
	for _, k := range p.keys {
		if err := k.need(w, ld); err != nil {
			return false, err
		}
	}
	for _, lv := range p.lv {
		for _, c := range lv.cols {
			c.loaded = c.k == nil && c.pred != nil
			if c.loaded {
				ld.allRows(c.t, c.c)
			}
		}
	}
	need := func(d int) bool { return p.hi <= d-1 && p.lo >= d } // the offsets from depth d-1 to d
	if need(1) {
		ld.allRows(cg.TTraces, cg.TrFirstResource)
	}
	if need(2) {
		ld.allRows(cg.TResources, cg.ResFirstScope)
	}
	if need(3) {
		ld.allRows(cg.TScopes, cg.ScFirstSpan)
	}
	ev, ln := p.lv[lvEvent].own(), p.lv[lvLink].own()
	if p.hi <= 3 && ev {
		ld.allRows(cg.TSpans, cg.SpFirstEvent)
	}
	if p.hi <= 3 && ln {
		ld.allRows(cg.TSpans, cg.SpFirstLink)
	}
	if err := ld.exec(ctx); err != nil {
		return false, err
	}
	for _, k := range p.keys {
		k.prepare(w)
	}
	var ch chain
	var err error
	if need(1) {
		if ch.trRes, err = w.firsts(cg.TTraces, cg.TrFirstResource, cg.TResources); err != nil {
			return false, err
		}
	}
	if need(2) {
		if ch.resSc, err = w.firsts(cg.TResources, cg.ResFirstScope, cg.TScopes); err != nil {
			return false, err
		}
	}
	if need(3) {
		if ch.scSp, err = w.firsts(cg.TScopes, cg.ScFirstSpan, cg.TSpans); err != nil {
			return false, err
		}
	}
	if p.hi <= 3 && ev {
		if ch.spEv, err = w.firsts(cg.TSpans, cg.SpFirstEvent, cg.TEvents); err != nil {
			return false, err
		}
	}
	if p.hi <= 3 && ln {
		if ch.spLn, err = w.firsts(cg.TSpans, cg.SpFirstLink, cg.TLinks); err != nil {
			return false, err
		}
	}
	// own matches
	for l, lv := range p.lv {
		if !lv.own() {
			continue
		}
		if err := x.ownMatch(p, l, lv, w); err != nil {
			return false, err
		}
	}
	p.alive(w, &ch)
	// values and names of the matching entities
	for l, lv := range p.lv {
		if !lv.own() {
			continue
		}
		if x.value != nil {
			if stop, err := x.values(l, lv, w); err != nil || stop {
				return stop, err
			}
		}
		if x.name != nil && lv.names {
			if stop, err := x.names(p, l, lv, w); err != nil || stop {
				return stop, err
			}
		}
	}
	return false, nil
}

// ownMatch evaluates a level's own iterators (their AND) for every owner of the window, and
// for the null row of a span without events / links.
func (x *acRun) ownMatch(p *acPlan, l int, lv *acLevel, w *window) error {
	t := acTable[l]
	n := w.n(t)
	m := make([]bool, n)
	for i := range m {
		m[i] = true
	}
	lv.null = true
	var err error
	for _, c := range lv.cols {
		if c.pred == nil {
			continue // keeps every row, nulls too
		}
		lv.null = lv.null && c.pred.KeepValue(parquet.Value{})
		if c.k == nil {
			if c.cur, err = w.cursor(c.t, c.c); err != nil {
				return err
			}
			c.memo = c.memo[:0]
			for i := uint32(0); i < n; i++ {
				if m[i] {
					m[i] = c.keepRow(w.lo[t] + i)
				}
			}
			continue
		}
		c.kc = 0
		nullOK := c.pred.KeepValue(parquet.Value{})
		for i := uint32(0); i < n; i++ {
			if !m[i] {
				continue
			}
			found, ok := false, false
			for _, en := range c.k.owner(w.lo[t]+i, &c.kc) {
				if c.k.vtype(en) != c.vt {
					continue
				}
				found = true
				if c.pred.KeepValue(c.k.value(en)) {
					ok = true
					break
				}
			}
			m[i] = ok || (!found && nullOK)
		}
	}
	for _, f := range lv.filts {
		lv.null = false
		f.kc = 0
		for i := uint32(0); i < n; i++ {
			if !m[i] {
				continue
			}
			ok := false
			for _, en := range f.k.owner(w.lo[t]+i, &f.kc) {
				vt := f.k.vtype(en)
				if f.ded >= 0 && vtClass(vt) == vtClass(uint8(f.ded)) {
					continue
				}
				if vtClass(vt) == f.class {
					ok = f.pred == nil || f.pred.KeepValue(f.k.value(en))
				} else {
					ok = f.null
				}
				if ok {
					break
				}
			}
			m[i] = ok
		}
	}
	if len(lv.tagKeys) > 0 {
		lv.null = false
		kcs := make([]int, len(lv.tagKeys))
		for i := uint32(0); i < n; i++ {
			if !m[i] {
				continue
			}
			ok := false
			for ki, k := range lv.tagKeys {
				for _, en := range k.owner(w.lo[t]+i, &kcs[ki]) {
					if d := lv.tagDed[ki]; d >= 0 && vtClass(k.vtype(en)) == vtClass(uint8(d)) {
						continue
					}
					ok = true
					break
				}
			}
			m[i] = ok
		}
	}
	if len(lv.nils) > 0 {
		lv.null = false
		if err := x.nilMatch(p, l, lv, w, m); err != nil {
			return err
		}
	}
	lv.match = m
	return nil
}

func (c *acCol) keepRow(r uint32) bool {
	vs, i := c.cur.at(r)
	if vs.Dict && !vs.IsNull(i) {
		return c.memo.keep(vs.Codes[i], func() bool { return c.pred.KeepValue(valueOf(vs, i, c.cur.c.Dict())) })
	}
	return c.pred.KeepValue(valueOf(vs, i, c.cur.c.Dict()))
}

// nilMatch is the generic `= nil` iterator (vparquet5's nil iterator over the key column):
// owners with generic attributes, none with the key.
func (x *acRun) nilMatch(p *acPlan, l int, lv *acLevel, w *window, m []bool) error {
	cl := cgLevel[l]
	t := acTable[l]
	n := w.n(t)
	anyAttr := make([]bool, n)
	has := make([][]bool, len(lv.nils))
	for i := range has {
		has[i] = make([]bool, n)
	}
	err := x.scanAttrs(w, cl, nil, func(o uint32, key string, vt uint8) {
		if dv, ok := p.ded[cl][key]; ok && vtClass(vt) == vtClass(dv) {
			return
		}
		i := o - w.lo[t]
		anyAttr[i] = true
		for ni, k := range lv.nils {
			if k == key {
				has[ni][i] = true
			}
		}
	})
	if err != nil {
		return err
	}
	for i := range m {
		if !m[i] {
			continue
		}
		if !anyAttr[i] {
			m[i] = false
			continue
		}
		for ni := range lv.nils {
			if has[ni][i] {
				m[i] = false
				break
			}
		}
	}
	return nil
}

// scanAttrs visits (owner, key, vtype) of the point-copy attributes of level l in the window,
// of the owners in alive (window-local; nil: all).
func (x *acRun) scanAttrs(w *window, l int, alive []bool, visit func(o uint32, key string, vt uint8)) error {
	ta := cg.AttrsTable(l)
	t := cg.OwnerTable[l]
	ivs := w.all(ta)
	if alive != nil {
		var owners []uint32
		for i, ok := range alive {
			if ok {
				owners = append(owners, w.lo[t]+uint32(i))
			}
		}
		if len(owners) == 0 {
			return nil
		}
		var err error
		if ivs, err = zoneIvs(w, ta, cg.AtOwner, owners); err != nil {
			return err
		}
		// the window's attribute rows only
		for i := range ivs {
			ivs[i].Lo, ivs[i].Hi = max(ivs[i].Lo, w.lo[ta]), min(ivs[i].Hi, w.hi[ta])
		}
	}
	ld := newLoader(w)
	for _, c := range []int{cg.AtOwner, cg.AtKey, cg.AtVType} {
		ld.col(ta, c, ivs)
	}
	if err := ld.exec(x.r.ctx); err != nil {
		return err
	}
	oc, err := w.cursor(ta, cg.AtOwner)
	if err != nil {
		return err
	}
	kc, err := w.cursor(ta, cg.AtKey)
	if err != nil {
		return err
	}
	vc, err := w.cursor(ta, cg.AtVType)
	if err != nil {
		return err
	}
	for _, iv := range ivs {
		for r := iv.Lo; r < iv.Hi; r++ {
			o := uint32(oc.int(r))
			if o < w.lo[t] || o >= w.hi[t] || (alive != nil && !alive[o-w.lo[t]]) {
				continue
			}
			visit(o, kc.str(r), uint8(vc.int(r)))
		}
	}
	return nil
}

// alive computes, bottom-up, which entities match their level and have a matching child
// (where a deeper level has iterators), then, top-down, which also have matching parents.
func (p *acPlan) alive(w *window, ch *chain) {
	own := func(l int) bool { return p.lv[l].own() }
	n := func(l int) uint32 { return w.n(acTable[l]) }
	orTrue := func(l int, i uint32) bool { return !own(l) || p.lv[l].match[i] }
	kidMatch := func(l int, first []uint32, s uint32) bool {
		lv := p.lv[l]
		a, b := first[s], first[s+1]
		if a == b {
			return lv.null
		}
		for e := a; e < b; e++ {
			if lv.match[e] {
				return true
			}
		}
		return false
	}
	anyIn := func(m []bool, first []uint32, i uint32) bool {
		for c := first[i]; c < first[i+1]; c++ {
			if m[c] {
				return true
			}
		}
		return false
	}
	var spM, scM, resM, trM []bool
	if p.hi <= 3 && p.lo >= 3 {
		spM = make([]bool, n(lvSpan))
		for s := range spM {
			m := orTrue(lvSpan, uint32(s))
			if m && own(lvEvent) {
				m = kidMatch(lvEvent, ch.spEv, uint32(s))
			}
			if m && own(lvLink) {
				m = kidMatch(lvLink, ch.spLn, uint32(s))
			}
			spM[s] = m
		}
	}
	if p.hi <= 2 && p.lo >= 2 {
		scM = make([]bool, n(lvScope))
		for c := range scM {
			m := orTrue(lvScope, uint32(c))
			if m && p.lo >= 3 {
				m = anyIn(spM, ch.scSp, uint32(c))
			}
			scM[c] = m
		}
	}
	if p.hi <= 1 && p.lo >= 1 {
		resM = make([]bool, n(lvRes))
		for r := range resM {
			m := orTrue(lvRes, uint32(r))
			if m && p.lo >= 2 {
				m = anyIn(scM, ch.resSc, uint32(r))
			}
			resM[r] = m
		}
	}
	if p.hi == 0 {
		trM = make([]bool, n(lvTrace))
		for t := range trM {
			m := orTrue(lvTrace, uint32(t))
			if m && p.lo >= 1 {
				m = anyIn(resM, ch.trRes, uint32(t))
			}
			trM[t] = m
		}
	}
	// top-down: an entity is alive if it matches and its parent is alive
	down := func(child []bool, parent []bool, first []uint32) {
		for i := range parent {
			if parent[i] {
				continue
			}
			for c := first[i]; c < first[i+1]; c++ {
				child[c] = false
			}
		}
	}
	if resM != nil && trM != nil {
		down(resM, trM, ch.trRes)
	}
	if scM != nil && resM != nil {
		down(scM, resM, ch.resSc)
	}
	if spM != nil && scM != nil {
		down(spM, scM, ch.scSp)
	}
	p.lv[lvTrace].alive, p.lv[lvRes].alive, p.lv[lvScope].alive, p.lv[lvSpan].alive = trM, resM, scM, spM
	for _, l := range []int{lvEvent, lvLink} {
		lv := p.lv[l]
		if !lv.own() {
			lv.alive = nil
			continue
		}
		a := append([]bool(nil), lv.match...)
		first := ch.spEv
		if l == lvLink {
			first = ch.spLn
		}
		if spM != nil {
			down(a, spM, first)
		}
		lv.alive = a
	}
}

// values reports the selected values of a level's alive owners.
func (x *acRun) values(l int, lv *acLevel, w *window) (bool, error) {
	if lv.alive == nil {
		return false, nil
	}
	t := acTable[l]
	var rows []uint32
	for i, ok := range lv.alive {
		if ok {
			rows = append(rows, w.lo[t]+uint32(i))
		}
	}
	if len(rows) == 0 {
		return false, nil
	}
	for _, c := range lv.cols {
		if !c.sel {
			continue
		}
		if c.k != nil {
			c.kc = 0
			for _, o := range rows {
				for _, en := range c.k.owner(o, &c.kc) {
					if c.k.vtype(en) != c.vt {
						continue
					}
					v := c.k.value(en)
					if v.IsNull() || (c.pred != nil && !c.pred.KeepValue(v)) {
						continue
					}
					if x.value(c.mapv(v)) {
						return true, nil
					}
				}
			}
			continue
		}
		if !c.loaded {
			ld := newLoader(w)
			ld.col(c.t, c.c, rowIvs(rows))
			if err := ld.exec(x.r.ctx); err != nil {
				return false, err
			}
		}
		cur, err := w.cursor(c.t, c.c)
		if err != nil {
			return false, err
		}
		for _, o := range rows {
			v := cur.value(o)
			if v.IsNull() || (c.pred != nil && !c.pred.KeepValue(v)) {
				continue
			}
			if x.value(c.mapv(v)) {
				return true, nil
			}
		}
	}
	for ki, k := range lv.tagKeys {
		kc := 0
		for _, o := range rows {
			for _, en := range k.owner(o, &kc) {
				if d := lv.tagDed[ki]; d >= 0 && vtClass(k.vtype(en)) == vtClass(uint8(d)) {
					continue
				}
				v := k.value(en)
				if v.IsNull() {
					continue
				}
				if x.value(mapKind(v)) {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// names reports the generic attribute keys of a level's alive owners.
func (x *acRun) names(p *acPlan, l int, lv *acLevel, w *window) (bool, error) {
	if lv.alive == nil {
		return false, nil
	}
	cl := cgLevel[l]
	scope := acScope[l]
	stop := false
	err := x.scanAttrs(w, cl, lv.alive, func(_ uint32, key string, vt uint8) {
		if stop {
			return
		}
		if dv, ok := p.ded[cl][key]; ok && vtClass(vt) == vtClass(dv) {
			return
		}
		stop = x.name(key, scope)
	})
	return stop, err
}

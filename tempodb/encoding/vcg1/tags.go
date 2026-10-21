package vcg1

import (
	"context"
	"sort"

	"github.com/grafana/tempo/v3/pkg/traceql"
	"github.com/grafana/tempo/v3/tempodb/backend"
	"github.com/grafana/tempo/v3/tempodb/encoding/common"
	cg "github.com/grafana/tempo/v3/tempodb/encoding/vcg1/cgfile"
	"github.com/grafana/tempo/v3/tempodb/encoding/vparquet5"
)

// Tag search follows vparquet5 (block_search_tags.go): the same scopes, intrinsic and
// well-known columns and dedicated-column rules. Every attribute key of a level is in the
// footer's key dictionary and every value of a key is in that key's scan-copy runs, so
// unfiltered tag names cost the footer and the values of a key cost its runs' dictionaries
// (or value buffers). vparquet5 stores dedicated attributes in their own columns; CG keeps
// them as ordinary attributes, so the union vparquet5 reads (dedicated column + generic
// attributes) is every entry of the key.

// translateTagToAttribute is vparquet5's mapping of v1 tag names.
var translateTagToAttribute = map[string]traceql.Attribute{
	vparquet5.LabelName:                   traceql.NewIntrinsic(traceql.IntrinsicName),
	vparquet5.LabelStatusCode:             traceql.NewIntrinsic(traceql.IntrinsicStatus),
	vparquet5.LabelTraceQLRootName:        traceql.NewIntrinsic(traceql.IntrinsicTraceRootSpan),
	vparquet5.LabelTraceQLRootServiceName: traceql.NewIntrinsic(traceql.IntrinsicTraceRootService),
	vparquet5.LabelTraceID:                traceql.NewIntrinsic(traceql.IntrinsicTraceID),
	vparquet5.LabelSpanID:                 traceql.NewIntrinsic(traceql.IntrinsicSpanID),

	vparquet5.LabelServiceName:      traceql.NewScopedAttribute(traceql.AttributeScopeResource, false, vparquet5.LabelServiceName),
	vparquet5.LabelCluster:          traceql.NewScopedAttribute(traceql.AttributeScopeResource, false, vparquet5.LabelCluster),
	vparquet5.LabelNamespace:        traceql.NewScopedAttribute(traceql.AttributeScopeResource, false, vparquet5.LabelNamespace),
	vparquet5.LabelPod:              traceql.NewScopedAttribute(traceql.AttributeScopeResource, false, vparquet5.LabelPod),
	vparquet5.LabelContainer:        traceql.NewScopedAttribute(traceql.AttributeScopeResource, false, vparquet5.LabelContainer),
	vparquet5.LabelK8sNamespaceName: traceql.NewScopedAttribute(traceql.AttributeScopeResource, false, vparquet5.LabelK8sNamespaceName),
	vparquet5.LabelK8sClusterName:   traceql.NewScopedAttribute(traceql.AttributeScopeResource, false, vparquet5.LabelK8sClusterName),
	vparquet5.LabelK8sPodName:       traceql.NewScopedAttribute(traceql.AttributeScopeResource, false, vparquet5.LabelK8sPodName),
	vparquet5.LabelK8sContainerName: traceql.NewScopedAttribute(traceql.AttributeScopeResource, false, vparquet5.LabelK8sContainerName),
	vparquet5.LabelHTTPMethod:       traceql.NewScopedAttribute(traceql.AttributeScopeSpan, false, vparquet5.LabelHTTPMethod),
	vparquet5.LabelHTTPUrl:          traceql.NewScopedAttribute(traceql.AttributeScopeSpan, false, vparquet5.LabelHTTPUrl),
	vparquet5.LabelHTTPStatusCode:   traceql.NewScopedAttribute(traceql.AttributeScopeSpan, false, vparquet5.LabelHTTPStatusCode),
}

// tagCol is a fixed column holding an intrinsic's values.
type tagCol struct{ t, c int }

// intrinsicTagCols are vparquet5's intrinsicColumnLookups columns (IntrinsicParentID is
// derived where the writer left it null).
var intrinsicTagCols = map[traceql.Intrinsic]tagCol{
	traceql.IntrinsicName:            {cg.TSpans, cg.SpName},
	traceql.IntrinsicStatus:          {cg.TSpans, cg.SpStatusCode},
	traceql.IntrinsicStatusMessage:   {cg.TSpans, cg.SpStatusMessage},
	traceql.IntrinsicDuration:        {cg.TSpans, cg.SpDuration},
	traceql.IntrinsicKind:            {cg.TSpans, cg.SpKind},
	traceql.IntrinsicSpanID:          {cg.TSpans, cg.SpSpanID},
	traceql.IntrinsicParentID:        {cg.TSpans, cg.SpParentSpanID},
	traceql.IntrinsicSpanStartTime:   {cg.TSpans, cg.SpStart},
	traceql.IntrinsicNestedSetLeft:   {cg.TSpans, cg.SpNestedLeft},
	traceql.IntrinsicNestedSetRight:  {cg.TSpans, cg.SpNestedRight},
	traceql.IntrinsicNestedSetParent: {cg.TSpans, cg.SpParentID},
	traceql.IntrinsicChildCount:      {cg.TSpans, cg.SpChildCount},
	traceql.IntrinsicSpanMultiplier:  {cg.TSpans, cg.SpTraceState},

	traceql.IntrinsicTraceRootService: {cg.TTraces, cg.TrRootService},
	traceql.IntrinsicTraceRootSpan:    {cg.TTraces, cg.TrRootSpan},
	traceql.IntrinsicTraceDuration:    {cg.TTraces, cg.TrDuration},
	traceql.IntrinsicTraceID:          {cg.TTraces, cg.TrTraceID},
	traceql.IntrinsicTraceStartTime:   {cg.TTraces, cg.TrStart},

	traceql.IntrinsicEventName:           {cg.TEvents, cg.EvName},
	traceql.IntrinsicEventTimeSinceStart: {cg.TEvents, cg.EvTime},
	traceql.IntrinsicLinkTraceID:         {cg.TLinks, cg.LnTraceID},
	traceql.IntrinsicLinkSpanID:          {cg.TLinks, cg.LnSpanID},

	traceql.IntrinsicInstrumentationName:    {cg.TScopes, cg.ScName},
	traceql.IntrinsicInstrumentationVersion: {cg.TScopes, cg.ScVersion},
}

// nonTraceQLTagCols are vparquet5's nonTraceQLAttributes.
var nonTraceQLTagCols = map[string]tagCol{
	vparquet5.LabelRootServiceName: {cg.TTraces, cg.TrRootService},
	vparquet5.LabelRootSpanName:    {cg.TTraces, cg.TrRootSpan},
}

// tagLevels are the attribute levels in vparquet5's SearchTags order.
var tagLevels = []struct {
	l     int
	scope traceql.AttributeScope
}{
	{cg.LResource, traceql.AttributeScopeResource},
	{cg.LScope, traceql.AttributeScopeInstrumentation},
	{cg.LSpan, traceql.AttributeScopeSpan},
	{cg.LEvent, traceql.AttributeScopeEvent},
	{cg.LLink, traceql.AttributeScopeLink},
}

// tagReq is one tag request: the footer, and the anchor range the search options select.
type tagReq struct {
	ctx   context.Context
	q     *cg.Request
	a, e  int
	whole bool
	ded   [cg.NumLevels]map[string]uint8
}

func (b *backendBlock) tagRequest(ctx context.Context, opts common.SearchOptions) (*tagReq, error) {
	q, err := b.request(ctx)
	if err != nil {
		return nil, err
	}
	a, e, err := b.anchorRange(ctx, q.F, opts)
	if err != nil {
		return nil, err
	}
	r := &tagReq{ctx: ctx, q: q, a: a, e: e, whole: a == 0 && e == len(q.F.Anchors)}
	r.ded[cg.LResource] = dedicatedMapping(b.meta.DedicatedColumns, backend.DedicatedColumnScopeResource)
	r.ded[cg.LSpan] = dedicatedMapping(b.meta.DedicatedColumns, backend.DedicatedColumnScopeSpan)
	r.ded[cg.LEvent] = dedicatedMapping(b.meta.DedicatedColumns, backend.DedicatedColumnScopeEvent)
	return r, nil
}

func (r *tagReq) bytes() uint64 { return uint64(r.q.Stats().Bytes) }

// rows is the file-row range of table t in the request's anchor range.
func (r *tagReq) rows(t int) (uint32, uint32) { return r.q.F.Anchor(r.a, t), r.q.F.Anchor(r.e, t) }

func (b *backendBlock) SearchTags(ctx context.Context, scope traceql.AttributeScope, cb common.TagsCallback, mcb common.MetricsCallback, opts common.SearchOptions) error {
	r, err := b.tagRequest(ctx, opts)
	if err != nil {
		return err
	}
	defer func() { mcb(r.bytes()) }()
	return r.searchTags(scope, func(t string, s traceql.AttributeScope) bool { cb(t, s); return false })
}

func (r *tagReq) searchTags(scope traceql.AttributeScope, cb func(string, traceql.AttributeScope) bool) error {
	for _, tl := range tagLevels {
		if scope != traceql.AttributeScopeNone && scope != tl.scope {
			continue
		}
		if tl.l == cg.LResource {
			// the well-known column has a value for every resource
			if lo, hi := r.rows(cg.TResources); hi > lo && cb(vparquet5.LabelServiceName, tl.scope) {
				return nil
			}
		}
		keys, err := r.levelKeys(tl.l)
		if err != nil {
			return err
		}
		for _, k := range keys {
			if cb(k, tl.scope) {
				return nil
			}
		}
	}
	return nil
}

// levelKeys are the generic attribute keys vparquet5 lists for level l in the request's range.
// Span and resource levels also list their dedicated columns, so every key counts; event
// dedicated columns are not listed, so an event key routed to one counts only for entries of
// another type (vparquet5's generic event attributes).
func (r *tagReq) levelKeys(l int) ([]string, error) {
	f := r.q.F
	if l >= len(f.Keys) {
		return nil, nil
	}
	var keys []string
	if r.whole {
		keys = f.Keys[l]
	} else {
		ta := cg.AttrsTable(l)
		lo, hi := r.rows(ta)
		seen := map[string]struct{}{}
		err := r.q.VisitColumn(r.ctx, ta, cg.AtKey, lo, hi, true, func(src cg.ColumnSource) bool {
			add := func(b []byte) {
				if _, ok := seen[string(b)]; !ok {
					seen[string(b)] = struct{}{}
				}
			}
			for _, e := range src.Entries {
				add(e)
			}
			if v := src.Page; v != nil {
				for i := src.Lo; i < src.Hi; i++ {
					if !v.IsNull(i) {
						add(v.Bytes(i, src.Dict))
					}
				}
			}
			return false
		})
		if err != nil {
			return nil, err
		}
		keys = make([]string, 0, len(seen))
		for k := range seen {
			keys = append(keys, k)
		}
		sort.Strings(keys)
	}
	if l != cg.LEvent || len(r.ded[l]) == 0 {
		return keys, nil
	}
	out := keys[:0:0]
	for _, k := range keys {
		if vt, ok := r.ded[l][k]; ok {
			has, err := r.kmHas(l, k, func(x uint8) bool { return x != vt })
			if err != nil {
				return nil, err
			}
			if !has {
				continue
			}
		}
		out = append(out, k)
	}
	return out, nil
}

// kmHas reports whether key has a scan-copy entry of a value type keep accepts among the
// owners of the request's range.
func (r *tagReq) kmHas(l int, key string, keep func(uint8) bool) (bool, error) {
	full, part, err := r.keyRuns(l, key, keep, false)
	if err != nil || len(full) > 0 || len(part) == 0 {
		return len(full) > 0, err
	}
	lo, hi := r.rows(cg.OwnerTable[l])
	if err := r.q.Load(r.ctx, nil, part); err != nil {
		return false, err
	}
	for _, ref := range part {
		rd := r.q.Run(ref)
		i := sort.Search(len(rd.Owners), func(i int) bool { return rd.Owners[i] >= lo })
		r.q.DropRun(ref)
		if i < len(rd.Owners) && rd.Owners[i] < hi {
			return true, nil
		}
	}
	return false, nil
}

// keyRuns are the scan-copy runs of key at level l, of value types keep accepts, in flushes
// meeting the request's owners: those of flushes wholly inside the range, and the others.
func (r *tagReq) keyRuns(l int, key string, keep func(uint8) bool, values bool) (full, part []cg.RunRef, err error) {
	id, ok := r.q.F.KeyID(l, key)
	if !ok {
		return nil, nil, nil
	}
	km, err := r.q.Km(l)
	if err != nil {
		return nil, nil, err
	}
	lo, hi := r.rows(cg.OwnerTable[l])
	for fi := range km {
		fl := &km[fi]
		if fl.Owner0 >= hi || fl.Owner0+fl.Owners <= lo {
			continue
		}
		inside := fl.Owner0 >= lo && fl.Owner0+fl.Owners <= hi
		a, b := fl.KeyRuns(id)
		for i := a; i < b; i++ {
			run := &fl.Runs[i]
			if !keep(run.VType) || (values && run.VCol == cg.VColNone) {
				continue
			}
			ref := cg.RunRef{Level: l, Flush: fi, Run: i}
			if inside {
				full = append(full, ref)
			} else {
				part = append(part, ref)
			}
		}
	}
	return full, part, nil
}

func (b *backendBlock) SearchTagValues(ctx context.Context, tag string, cb common.TagValuesCallback, mcb common.MetricsCallback, opts common.SearchOptions) error {
	att, ok := translateTagToAttribute[tag]
	if !ok {
		att = traceql.NewAttribute(tag)
	}
	return b.SearchTagValuesV2(ctx, att, func(v traceql.Static) bool {
		cb(v.EncodeToString(false))
		return false
	}, mcb, opts)
}

func (b *backendBlock) SearchTagValuesV2(ctx context.Context, tag traceql.Attribute, cb common.TagValuesCallbackV2, mcb common.MetricsCallback, opts common.SearchOptions) error {
	r, err := b.tagRequest(ctx, opts)
	if err != nil {
		return err
	}
	defer func() { mcb(r.bytes()) }()
	_, err = r.searchTagValues(tag, cb)
	return err
}

// searchTagValues reports every value of tag in the request's range (duplicates possible, as
// vparquet5's per-row-group reads) and whether cb stopped it.
func (r *tagReq) searchTagValues(tag traceql.Attribute, cb func(traceql.Static) bool) (bool, error) {
	if tag.Intrinsic != traceql.IntrinsicNone {
		c, ok := intrinsicTagCols[tag.Intrinsic]
		if !ok {
			return false, nil
		}
		if tag.Intrinsic == traceql.IntrinsicParentID {
			return r.parentIDValues(cb)
		}
		return r.columnValues(c, cb)
	}
	if c, ok := nonTraceQLTagCols[tag.Name]; ok {
		return r.columnValues(c, cb)
	}
	unscoped := tag.Scope == traceql.AttributeScopeNone
	if tag.Name == vparquet5.LabelServiceName && (unscoped || tag.Scope == traceql.AttributeScopeResource) {
		if stop, err := r.columnValues(tagCol{cg.TResources, cg.ResServiceName}, cb); stop || err != nil {
			return stop, err
		}
	}
	all := func(uint8) bool { return true }
	levels := []struct {
		l    int
		ok   bool
		keep func(uint8) bool
	}{
		{cg.LResource, unscoped || tag.Scope == traceql.AttributeScopeResource, all},
		{cg.LSpan, unscoped || tag.Scope == traceql.AttributeScopeSpan, all},
		{cg.LScope, unscoped || tag.Scope == traceql.AttributeScopeInstrumentation, all},
		{cg.LEvent, tag.Scope == traceql.AttributeScopeEvent, all},
		{cg.LLink, tag.Scope == traceql.AttributeScopeLink, all},
	}
	for _, lv := range levels {
		if !lv.ok {
			continue
		}
		keep := lv.keep
		if vt, ok := r.ded[lv.l][tag.Name]; ok && lv.l == cg.LEvent {
			// vparquet5 does not search event dedicated columns
			keep = func(x uint8) bool { return x != vt }
		}
		if stop, err := r.kmValues(lv.l, tag.Name, keep, cb); stop || err != nil {
			return stop, err
		}
	}
	return false, nil
}

// columnValues reports the values of a fixed column in the range: chunk dictionaries for
// byte-array columns, decoded pages otherwise.
func (r *tagReq) columnValues(c tagCol, cb func(traceql.Static) bool) (bool, error) {
	lo, hi := r.rows(c.t)
	stop := false
	err := r.q.VisitColumn(r.ctx, c.t, c.c, lo, hi, true, func(src cg.ColumnSource) bool {
		for _, e := range src.Entries {
			if cb(traceql.NewStaticString(bstr(e))) {
				stop = true
				return true
			}
		}
		if v := src.Page; v != nil {
			for i := src.Lo; i < src.Hi; i++ {
				if v.IsNull(i) {
					continue
				}
				if cb(staticOfValues(v, i, src.Dict)) {
					stop = true
					return true
				}
			}
		}
		return false
	})
	return stop, err
}

// staticOfValues is vparquet5's tag value of a raw column value (callback): strings, ints,
// floats and bools by physical type.
func staticOfValues(v *cg.Values, i int, d *cg.Dict) traceql.Static {
	switch v.Phys {
	case cg.PhysBytes:
		return traceql.NewStaticString(bstr(v.Bytes(i, d)))
	case cg.PhysBool:
		return traceql.NewStaticBool(v.Bools[i])
	case cg.PhysDouble:
		return traceql.NewStaticFloat(v.Floats[i])
	default:
		return traceql.NewStaticInt(int(v.Ints[i]))
	}
}

// parentIDValues reports every span's parent span id (derived where the writer left it null).
func (r *tagReq) parentIDValues(cb func(traceql.Static) bool) (bool, error) {
	for _, wr := range windows(r.q.F, r.a, r.e) {
		w := newWindow(r.q, wr[0], wr[1])
		ld := newLoader(w)
		for _, c := range []int{cg.SpParentSpanID, cg.SpParentID, cg.SpNestedLeft, cg.SpSpanID} {
			ld.allRows(cg.TSpans, c)
		}
		ld.allRows(cg.TTraces, cg.TrFirstSpan)
		if err := ld.exec(r.ctx); err != nil {
			return false, err
		}
		cur, err := w.cursor(cg.TSpans, cg.SpParentSpanID)
		if err != nil {
			return false, err
		}
		pid, err := newParentIDs(w)
		if err != nil {
			return false, err
		}
		for o := w.lo[cg.TSpans]; o < w.hi[cg.TSpans]; o++ {
			b, ok := cur.bytes(o)
			if !ok {
				b = pid.derive(o)
			}
			if cb(traceql.NewStaticString(bstr(b))) {
				return true, nil
			}
		}
		for t := 0; t < cg.NumTables; t++ {
			r.q.DropPagesBefore(t, w.hi[t])
		}
	}
	return false, nil
}

// kmValues reports the values of key's scan-copy runs (of value types keep accepts) for the
// owners in the range: a run of a flush inside the range by its dictionary or value buffer
// alone, a run of a flush crossing the range's ends decoded whole and filtered by owner.
func (r *tagReq) kmValues(l int, key string, keep func(uint8) bool, cb func(traceql.Static) bool) (bool, error) {
	full, part, err := r.keyRuns(l, key, keep, true)
	if err != nil {
		return false, err
	}
	const batch = 64
	for i := 0; i < len(full); i += batch {
		rvs, err := r.q.RunDistinct(r.ctx, full[i:min(i+batch, len(full))])
		if err != nil {
			return false, err
		}
		for _, rv := range rvs {
			for _, e := range rv.Entries {
				if cb(traceql.NewStaticString(bstr(e))) {
					return true, nil
				}
			}
			if v := rv.Vals; v != nil {
				for j := 0; j < v.N; j++ {
					if !v.IsNull(j) && cb(staticOfValues(v, j, rv.Dict)) {
						return true, nil
					}
				}
			}
		}
	}
	lo, hi := r.rows(cg.OwnerTable[l])
	for i := 0; i < len(part); i += batch {
		refs := part[i:min(i+batch, len(part))]
		if err := r.q.Load(r.ctx, nil, refs); err != nil {
			return false, err
		}
		for _, ref := range refs {
			rd := r.q.Run(ref)
			r.q.DropRun(ref)
			if rd.Vals == nil {
				continue
			}
			j := sort.Search(len(rd.Owners), func(i int) bool { return rd.Owners[i] >= lo })
			for ; j < len(rd.Owners) && rd.Owners[j] < hi; j++ {
				if !rd.Vals.IsNull(j) && cb(staticOfValues(rd.Vals, j, rd.Dict)) {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

package vcg1

// The span type and its methods are vparquet5's (block_traceql.go), kept verbatim so attribute
// lookup precedence and structural results are identical.

import (
	"sort"
	"sync"
	"unsafe"

	v1 "github.com/grafana/tempo/v3/pkg/tempopb/trace/v1"
	"github.com/grafana/tempo/v3/pkg/traceql"
)

type attrVal struct {
	a traceql.Attribute
	s traceql.Static
}

// span implements traceql.Span
type span struct {
	spanAttrs            []attrVal
	resourceAttrs        []attrVal
	traceAttrs           []attrVal
	eventAttrs           []attrVal
	linkAttrs            []attrVal
	instrumentationAttrs []attrVal

	id                 []byte
	startTimeUnixNanos uint64
	durationNanos      uint64
	nestedSetParent    int32
	nestedSetLeft      int32
	nestedSetRight     int32

	// metadata used to track the span in the parquet file
	row            uint32 // window-local span row
	trace          uint32 // window-local trace row
	dropped        bool
	cbSpansetFinal bool
	cbSpanset      *traceql.Spanset
}

func (s *span) Release() {
	putSpan(s)
}

func (s *span) AllAttributes() map[traceql.Attribute]traceql.Static {
	atts := make(map[traceql.Attribute]traceql.Static, len(s.spanAttrs)+len(s.resourceAttrs)+len(s.traceAttrs)+len(s.eventAttrs)+len(s.linkAttrs))
	for _, st := range s.traceAttrs {
		if st.s.Type == traceql.TypeNil {
			continue
		}
		atts[st.a] = st.s
	}
	for _, st := range s.resourceAttrs {
		if st.s.Type == traceql.TypeNil {
			continue
		}
		atts[st.a] = st.s
	}
	for _, st := range s.spanAttrs {
		if st.s.Type == traceql.TypeNil {
			continue
		}
		atts[st.a] = st.s
	}
	for _, st := range s.eventAttrs {
		if st.s.Type == traceql.TypeNil {
			continue
		}
		atts[st.a] = st.s
	}
	for _, st := range s.linkAttrs {
		if st.s.Type == traceql.TypeNil {
			continue
		}
		atts[st.a] = st.s
	}
	for _, st := range s.instrumentationAttrs {
		if st.s.Type == traceql.TypeNil {
			continue
		}
		atts[st.a] = st.s
	}
	return atts
}

func (s *span) AllAttributesFunc(cb func(traceql.Attribute, traceql.Static)) {
	if len(s.traceAttrs) > 0 {
		for _, a := range s.traceAttrs {
			cb(a.a, a.s)
		}
	}
	if len(s.resourceAttrs) > 0 {
		for _, a := range s.resourceAttrs {
			cb(a.a, a.s)
		}
	}
	if len(s.instrumentationAttrs) > 0 {
		for _, a := range s.instrumentationAttrs {
			cb(a.a, a.s)
		}
	}
	if len(s.spanAttrs) > 0 {
		for _, a := range s.spanAttrs {
			cb(a.a, a.s)
		}
	}
	if len(s.eventAttrs) > 0 {
		for _, a := range s.eventAttrs {
			cb(a.a, a.s)
		}
	}
	if len(s.linkAttrs) > 0 {
		for _, a := range s.linkAttrs {
			cb(a.a, a.s)
		}
	}
}

func (s *span) AttributeFor(a traceql.Attribute) (traceql.Static, bool) {
	find := func(a traceql.Attribute, attrs []attrVal) *traceql.Static {
		if len(attrs) == 1 {
			if attrs[0].a == a {
				return &attrs[0].s
			}
			return nil
		}
		if len(attrs) == 2 {
			if attrs[0].a == a {
				return &attrs[0].s
			}
			if attrs[1].a == a {
				return &attrs[1].s
			}
			return nil
		}

		for i := range attrs {
			if attrs[i].a == a {
				return &attrs[i].s
			}
		}
		return nil
	}
	findName := func(s string, attrs []attrVal) *traceql.Static {
		if len(attrs) == 1 {
			if attrs[0].a.Name == s {
				return &attrs[0].s
			}
			return nil
		}
		if len(attrs) == 2 {
			if attrs[0].a.Name == s {
				return &attrs[0].s
			}
			if attrs[1].a.Name == s {
				return &attrs[1].s
			}
			return nil
		}

		for i := range attrs {
			if attrs[i].a.Name == s {
				return &attrs[i].s
			}
		}
		return nil
	}

	switch a.Scope {
	case traceql.AttributeScopeResource:
		if len(s.resourceAttrs) > 0 {
			if attr := find(a, s.resourceAttrs); attr != nil {
				return *attr, true
			}
		}
		return traceql.StaticNil, false
	case traceql.AttributeScopeSpan:
		if len(s.spanAttrs) > 0 {
			if attr := find(a, s.spanAttrs); attr != nil {
				return *attr, true
			}
		}
		return traceql.StaticNil, false
	case traceql.AttributeScopeEvent:
		if len(s.eventAttrs) > 0 {
			if attr := find(a, s.eventAttrs); attr != nil {
				return *attr, true
			}
		}
		return traceql.StaticNil, false
	case traceql.AttributeScopeLink:
		if len(s.linkAttrs) > 0 {
			if attr := find(a, s.linkAttrs); attr != nil {
				return *attr, true
			}
		}
		return traceql.StaticNil, false
	case traceql.AttributeScopeInstrumentation:
		if len(s.instrumentationAttrs) > 0 {
			if attr := find(a, s.instrumentationAttrs); attr != nil {
				return *attr, true
			}
		}
		return traceql.StaticNil, false
	}

	if a.Intrinsic != traceql.IntrinsicNone {
		if a.Intrinsic == traceql.IntrinsicNestedSetLeft {
			return traceql.NewStaticInt(int(s.nestedSetLeft)), true
		}
		if a.Intrinsic == traceql.IntrinsicNestedSetRight {
			return traceql.NewStaticInt(int(s.nestedSetRight)), true
		}
		if a.Intrinsic == traceql.IntrinsicNestedSetParent {
			return traceql.NewStaticInt(int(s.nestedSetParent)), true
		}

		// intrinsics are always on the span, trace, event, or link ... for now
		if attr := find(a, s.spanAttrs); attr != nil {
			return *attr, true
		}

		if attr := find(a, s.traceAttrs); attr != nil {
			return *attr, true
		}

		if attr := find(a, s.eventAttrs); attr != nil {
			return *attr, true
		}

		if attr := find(a, s.linkAttrs); attr != nil {
			return *attr, true
		}

		if attr := find(a, s.instrumentationAttrs); attr != nil {
			return *attr, true
		}
	}

	// name search in span, resource, link, and event to give precedence to span
	// we don't need to do a name search at the trace level b/c it is intrinsics only
	if len(s.spanAttrs) > 0 {
		if attr := findName(a.Name, s.spanAttrs); attr != nil {
			return *attr, true
		}
	}

	if len(s.resourceAttrs) > 0 {
		if attr := findName(a.Name, s.resourceAttrs); attr != nil {
			return *attr, true
		}
	}

	if len(s.eventAttrs) > 0 {
		if attr := findName(a.Name, s.eventAttrs); attr != nil {
			return *attr, true
		}
	}

	if len(s.linkAttrs) > 0 {
		if attr := findName(a.Name, s.linkAttrs); attr != nil {
			return *attr, true
		}
	}

	if len(s.instrumentationAttrs) > 0 {
		if attr := findName(a.Name, s.instrumentationAttrs); attr != nil {
			return *attr, true
		}
	}

	return traceql.StaticNil, false
}

func (s *span) ID() []byte {
	return s.id
}

func (s *span) StartTimeUnixNanos() uint64 {
	return s.startTimeUnixNanos
}

func (s *span) DurationNanos() uint64 {
	return s.durationNanos
}

// RowNumber exposes the span's location within the block file. It is not part of the
// traceql.Span interface; callers that need it (e.g. tempo-cli tooling that visualizes
// where matches fall within a block) type-assert for it.
func (s *span) DescendantOf(lhs, rhs []traceql.Span, falseForAll, invert, union bool, buffer []traceql.Span) []traceql.Span {
	if len(lhs) == 0 && len(rhs) == 0 {
		return nil
	}

	if union {
		return descendantOfUnion(lhs, rhs, invert, buffer)
	}

	// sort by nested set left. the goal is to quickly be able to find the first entry in the lhs slice that
	// potentially matches the rhs. after we find this first potential match we just check every single lhs
	// entry til the end of the slice.
	// it might be even better to clone the lhs slice. sort one by left and one by right and search the one that
	// requires less seeking after the search. this would be faster but cloning the slice would be costly in mem
	sortFn := func(i, j int) bool { return lhs[i].(*span).nestedSetLeft > lhs[j].(*span).nestedSetLeft } // sort asc b/c we are interested in lhs nestedSetLeft > rhs nestedSetLeft
	if invert {
		sortFn = func(i, j int) bool { return lhs[i].(*span).nestedSetLeft < lhs[j].(*span).nestedSetLeft } // sort desc b/c we want the inverse relationship. see descendantOf func
	}
	sort.Slice(lhs, sortFn)

	descendantOf := func(a, b *span) bool {
		if a.nestedSetLeft == 0 ||
			b.nestedSetLeft == 0 ||
			a.nestedSetRight == 0 ||
			b.nestedSetRight == 0 {
			// Spans with missing data, never a match.
			return false
		}
		return a.nestedSetLeft > b.nestedSetLeft && a.nestedSetRight < b.nestedSetRight
	}

	for _, r := range rhs {
		matches := false
		findFn := func(i int) bool { return lhs[i].(*span).nestedSetLeft <= r.(*span).nestedSetLeft }
		if invert {
			findFn = func(i int) bool { return lhs[i].(*span).nestedSetLeft >= r.(*span).nestedSetLeft }
		}

		// let's find the first index we need to bother with.
		found := sort.Search(len(lhs), findFn)
		if found == -1 { // if we are less then the entire slice we have to search the entire slice
			found = 0
		}

		for ; found < len(lhs); found++ {
			a := lhs[found].(*span)
			b := r.(*span)
			if invert {
				a, b = b, a
			}

			if descendantOf(b, a) {
				// Returns RHS
				matches = true
				break
			}
		}
		if matches && !falseForAll || // return RHS if there are any matches on the LHS
			!matches && falseForAll { // return RHS if there are no matches on the LHS
			buffer = append(buffer, r)
		}
	}

	return buffer
}

// descendantOfUnion is a special loop designed to handle union descendantOf. technically nestedSetManyManyLoop can logically do this
// but it contains a pathological case where it devolves to O(n^2) in the worst case (a trace with a single series of nested spans).
// this loop more directly handles union descendantof.
//   - iterate the rhs checking to see if it is a descendant of lhs
//   - break out of the rhs loop when the next span will be in a different branch
//   - at this point rSpan has the narrowest span (the leaf span) of the branch
//   - iterate the lhs as long as its in the same branch as rSpan checking for ancestors
//   - go back to rhs iteration and repeat until slices are exhausted
func descendantOfUnion(lhs, rhs []traceql.Span, invert bool, buffer []traceql.Span) []traceql.Span {
	// union is harder b/c we have to find all matches on both the left and rhs
	sort.Slice(lhs, func(i, j int) bool { return lhs[i].(*span).nestedSetLeft < lhs[j].(*span).nestedSetLeft })
	if unsafe.SliceData(lhs) != unsafe.SliceData(rhs) { // if these are pointing to the same slice, no reason to sort again
		sort.Slice(rhs, func(i, j int) bool { return rhs[i].(*span).nestedSetLeft < rhs[j].(*span).nestedSetLeft })
	}

	isAfter := func(a, b *span) bool {
		return b.nestedSetLeft > a.nestedSetRight
	}
	isMatch := func(a, d *span) bool {
		if d.nestedSetLeft == 0 ||
			a.nestedSetLeft == 0 ||
			d.nestedSetRight == 0 ||
			a.nestedSetRight == 0 {
			return false
		}
		return d.nestedSetLeft > a.nestedSetLeft && d.nestedSetRight < a.nestedSetRight
	}

	if invert {
		lhs, rhs = rhs, lhs
	}

	uniqueSpans := make(map[*span]struct{}) // todo: consider a reusable map, like our buffer slice
	addToBuffer := func(s *span) {
		if _, ok := uniqueSpans[s]; !ok {
			buffer = append(buffer, s)
			uniqueSpans[s] = struct{}{}
		}
	}

	lidx := 0
	ridx := 0
	for lidx < len(lhs) && ridx < len(rhs) {
		lSpan := lhs[lidx].(*span)
		rSpan := rhs[ridx].(*span)

		// rhs
		for ; ridx < len(rhs); ridx++ {
			rSpan = rhs[ridx].(*span)

			if isMatch(lSpan, rSpan) {
				addToBuffer(rSpan)
			}

			// test the next span to see if it is still in the tree of lhs. if not bail!
			if ridx+1 < len(rhs) && isAfter(rSpan, rhs[ridx+1].(*span)) {
				ridx++
				break
			}
		}

		// lhs
		// rSpan contains the narrowest span that is in tree for lhs. advance and add lhs until we're out of tree
		for ; lidx < len(lhs); lidx++ {
			lSpan = lhs[lidx].(*span)

			// advance LHS until out of tree of RHS
			if isAfter(rSpan, lSpan) {
				break
			}

			if isMatch(lSpan, rSpan) {
				addToBuffer(lSpan)

				// if rSpan is in tree of lSpan keep on keeping on
				if ridx < len(rhs) && isMatch(lSpan, rhs[ridx].(*span)) {
					break
				}
			}
		}
	}

	return buffer
}

// SiblingOf
func (s *span) SiblingOf(lhs, rhs []traceql.Span, falseForAll, union bool, buffer []traceql.Span) []traceql.Span {
	if len(lhs) == 0 && len(rhs) == 0 {
		return nil
	}

	if union {
		// union is more difficult b/c we have to find all matches on both the left and rhs
		sort.Slice(lhs, func(i, j int) bool { return lhs[i].(*span).nestedSetParent < lhs[j].(*span).nestedSetParent })
		if unsafe.SliceData(lhs) != unsafe.SliceData(rhs) { // if these are pointing to the same slice, no reason to sort again
			sort.Slice(rhs, func(i, j int) bool { return rhs[i].(*span).nestedSetParent < rhs[j].(*span).nestedSetParent })
		}

		siblingOf := func(a, b *span) bool {
			return a.nestedSetParent == b.nestedSetParent &&
				a.nestedSetParent != 0 &&
				b.nestedSetParent != 0 &&
				a != b // a span cannot be its own sibling. note that this only works due to implementation details in the engine. if we ever pipeline structural operators then we would need to use something else for identity. rownumber?
		}

		isValid := func(s *span) bool { return s.nestedSetParent != 0 }
		isAfter := func(a, b *span) bool { return b.nestedSetParent > a.nestedSetParent }

		return nestedSetManyManyLoop(lhs, rhs, isValid, siblingOf, isAfter, falseForAll, false, union, buffer)
	}

	// this is easy. we're just looking for anything on the lhs side with the same nested set parent as the rhs
	sort.Slice(lhs, func(i, j int) bool {
		return lhs[i].(*span).nestedSetParent < lhs[j].(*span).nestedSetParent
	})

	siblingOf := func(a, b *span) bool {
		return a.nestedSetParent == b.nestedSetParent &&
			a.nestedSetParent != 0 &&
			b.nestedSetParent != 0
	}

	for _, r := range rhs {
		matches := false

		if r.(*span).nestedSetParent != 0 {
			// search for nested set parent
			found := sort.Search(len(lhs), func(i int) bool {
				return lhs[i].(*span).nestedSetParent >= r.(*span).nestedSetParent
			})

			if found >= 0 && found < len(lhs) {
				matches = siblingOf(r.(*span), lhs[found].(*span))

				// if we found a match BUT this is the same span as the match we need to check the very next span (if it exists).
				// this works b/c Search method returns the first match for nestedSetParent
				if matches && r.(*span) == lhs[found].(*span) {
					matches = false
					if found+1 < len(lhs) {
						matches = siblingOf(r.(*span), lhs[found+1].(*span))
					}
				}
			}
		}

		if matches && !falseForAll || // return RHS if there are any matches on the LHS
			!matches && falseForAll { // return RHS if there are no matches on the LHS
			buffer = append(buffer, r)
		}
	}
	return buffer
}

// {} > {}
func (s *span) ChildOf(lhs, rhs []traceql.Span, falseForAll, invert, union bool, buffer []traceql.Span) []traceql.Span {
	if len(lhs) == 0 && len(rhs) == 0 {
		return nil
	}

	if union {
		childOf := func(p, c *span) bool {
			return p.nestedSetLeft == c.nestedSetParent &&
				p.nestedSetLeft != 0 &&
				c.nestedSetParent != 0
		}
		isValid := func(s *span) bool { return s.nestedSetLeft != 0 }
		isAfter := func(p, c *span) bool { return c.nestedSetParent > p.nestedSetLeft }

		// the engine will sometimes pass the same slice for both lhs and rhs. this occurs for {} > {}.
		// if lhs is the same slice as rhs we need to make a copy of the slice to sort them by different values
		if unsafe.SliceData(lhs) == unsafe.SliceData(rhs) {
			rhs = append([]traceql.Span{}, rhs...)
		}

		parents := lhs
		children := rhs
		if invert {
			parents, children = children, parents
		}

		sort.Slice(parents, func(i, j int) bool { return parents[i].(*span).nestedSetLeft < parents[j].(*span).nestedSetLeft })
		sort.Slice(children, func(i, j int) bool { return children[i].(*span).nestedSetParent < children[j].(*span).nestedSetParent })

		return nestedSetOneManyLoop(parents, children, isValid, childOf, isAfter, falseForAll, invert, union, buffer)
	}

	// we will search the LHS by either nestedSetLeft or nestedSetParent. if we are doing child we sort by nestedSetLeft
	// so we can quickly find children. if the invert flag is set we are looking for parents and so we sort appropriately
	sortFn := func(i, j int) bool { return lhs[i].(*span).nestedSetLeft < lhs[j].(*span).nestedSetLeft }
	if invert {
		sortFn = func(i, j int) bool { return lhs[i].(*span).nestedSetParent < lhs[j].(*span).nestedSetParent }
	}

	childOf := func(a, b *span) bool {
		return a.nestedSetLeft == b.nestedSetParent &&
			a.nestedSetLeft != 0 &&
			b.nestedSetParent != 0
	}

	sort.Slice(lhs, sortFn)
	for _, r := range rhs {
		findFn := func(i int) bool { return lhs[i].(*span).nestedSetLeft >= r.(*span).nestedSetParent }
		if invert {
			findFn = func(i int) bool { return lhs[i].(*span).nestedSetParent >= r.(*span).nestedSetLeft }
		}

		// search for nested set parent
		matches := false
		found := sort.Search(len(lhs), findFn)
		if found >= 0 && found < len(lhs) {
			if invert {
				matches = childOf(r.(*span), lhs[found].(*span)) // is the rhs a child of the lhs?
			} else {
				matches = childOf(lhs[found].(*span), r.(*span)) // is the lhs a child of the rhs?
			}
		}

		if matches && !falseForAll || // return RHS if there are any matches on the LHS
			!matches && falseForAll { // return RHS if there are no matches on the LHS
			buffer = append(buffer, r)
		}
	}
	return buffer
}

// nestedSetOneManyLoop runs a standard one -> many loop to calculate nested set relationships. It handles all nested set relationships except
// siblingOf and unioned descendantOf. It forward iterates the one and many slices and applies.
func nestedSetOneManyLoop(one, many []traceql.Span, isValid func(*span) bool, isMatch, isAfter func(*span, *span) bool, falseForAll, invert, union bool, buffer []traceql.Span) []traceql.Span {
	var uniqueSpans map[*span]struct{}
	if union {
		uniqueSpans = make(map[*span]struct{}) // todo: consider a reusable map, like our buffer slice
	}

	addToBuffer := func(s *span) {
		if union {
			if _, ok := uniqueSpans[s]; !ok {
				buffer = append(buffer, s)
				uniqueSpans[s] = struct{}{}
			}
		} else {
			buffer = append(buffer, s)
		}
	}

	// note the small differences between this and the !invert loop. technically we could write these both in one piece of code,
	// but this feels better for clarity
	if invert {
		manyIdx := 0
		for _, o := range one {
			oSpan := o.(*span)

			if !isValid(oSpan) {
				continue
			}

			matches := false
			for ; manyIdx < len(many); manyIdx++ {
				mSpan := many[manyIdx].(*span)

				// if the many loop is ahead of the one loop break back to allow the one loop to let it catch up
				if isAfter(oSpan, mSpan) {
					break
				}

				if isMatch(oSpan, mSpan) {
					matches = true
					if union {
						addToBuffer(mSpan)
					} else {
						break
					}
				}
			}

			if (matches && !falseForAll) || (!matches && falseForAll) {
				addToBuffer(oSpan)
			}
		}

		return buffer
	}

	// !invert
	manyIdx := 0
	for _, o := range one {
		oSpan := o.(*span)

		if !isValid(oSpan) {
			continue
		}

		matches := false
		for ; manyIdx < len(many); manyIdx++ {
			mSpan := many[manyIdx].(*span)

			// if the many loop is ahead of the one loop break back to the allow one loop to let it catch up
			if isAfter(oSpan, mSpan) {
				break
			}

			match := isMatch(oSpan, mSpan)
			if (match && !falseForAll) || (!match && falseForAll) {
				matches = true
				addToBuffer(mSpan)
			}
		}

		if matches && union {
			addToBuffer(oSpan)
		}
	}

	// drain the rest of the children if falseForAll
	if falseForAll {
		for ; manyIdx < len(many); manyIdx++ {
			addToBuffer(many[manyIdx].(*span))
		}
	}

	return buffer
}

// nestedSetManyManyLoop handles the generic case when the lhs must be checked multiple times for each rhs. it is currently only
// used for siblingOf
func nestedSetManyManyLoop(lhs, rhs []traceql.Span, isValid func(*span) bool, isMatch, isAfter func(*span, *span) bool, falseForAll, invert, union bool, buffer []traceql.Span) []traceql.Span {
	var uniqueSpans map[*span]struct{}
	if union {
		uniqueSpans = make(map[*span]struct{}) // todo: consider a reusable map, like our buffer slice
	}

	addToBuffer := func(s *span) {
		if union {
			if _, ok := uniqueSpans[s]; !ok {
				buffer = append(buffer, s)
				uniqueSpans[s] = struct{}{}
			}
		} else {
			buffer = append(buffer, s)
		}
	}

	rescanIdx := 0
	lidx := 0
	for _, r := range rhs {
		rSpan := r.(*span)
		if !isValid(rSpan) {
			continue
		}

		// rescan whatever amount of rhs we need to
		lidx = rescanIdx

		matches := false
		for ; lidx < len(lhs); lidx++ {
			lSpan := lhs[lidx].(*span)

			// if left is after right, swap back to right
			if isAfter(rSpan, lSpan) {
				break
			}

			// if we transition forward (trees branches or parents or whatever) store current lidx to rescan
			if isAfter(lhs[rescanIdx].(*span), lSpan) {
				rescanIdx = lidx
			}

			if (!invert && isMatch(rSpan, lSpan)) || (invert && isMatch(lSpan, rSpan)) {
				matches = true
				if union {
					addToBuffer(lSpan)
				}
			}
		}

		if (matches && !falseForAll) || (!matches && falseForAll) {
			addToBuffer(rSpan)
		}
	}

	return buffer
}

func (s *span) setInstrumentationAttrs(attrs []attrVal) {
	s.instrumentationAttrs = append(s.instrumentationAttrs, attrs...)
}

func (s *span) setResourceAttrs(attrs []attrVal) {
	s.resourceAttrs = append(s.resourceAttrs, attrs...)
}

func (s *span) setTraceAttrs(attrs []attrVal) {
	s.traceAttrs = append(s.traceAttrs, attrs...)
}

// attributesMatched counts all attributes in the map as well as metadata fields like start/end/id
func (s *span) attributesMatched() int {
	count := 0
	// todo: attributesMatced is called a lot. we could cache this count on set
	for _, st := range s.spanAttrs {
		if st.s.Type != traceql.TypeNil {
			count++
		}
	}
	for _, st := range s.resourceAttrs {
		if st.s.Type != traceql.TypeNil {
			count++
		}
	}
	for _, st := range s.traceAttrs {
		if st.s.Type != traceql.TypeNil {
			count++
		}
	}
	for _, st := range s.eventAttrs {
		if st.s.Type != traceql.TypeNil {
			count++
		}
	}
	for _, st := range s.linkAttrs {
		if st.s.Type != traceql.TypeNil {
			count++
		}
	}
	for _, st := range s.instrumentationAttrs {
		if st.s.Type != traceql.TypeNil {
			count++
		}
	}
	if s.startTimeUnixNanos != 0 {
		count++
	}
	// don't count duration nanos b/c it is added to the attributes as well as the span struct
	// if s.durationNanos != 0 {
	// 	count++
	// }
	if len(s.id) > 0 {
		count++
	}
	if s.nestedSetLeft > 0 || s.nestedSetRight > 0 || s.nestedSetParent != 0 { // nestedSetParent can be -1 meaning it is a root span
		count++
	}

	return count
}

func (s *span) hasAtLeast(numAttributes int) bool {
	count := 0
	if s.startTimeUnixNanos != 0 {
		count++
		if count >= numAttributes {
			return true
		}
	}
	// don't count duration nanos b/c it is added to the attributes as well as the span struct
	// if s.durationNanos != 0 {
	// 	count++
	// }
	if len(s.id) > 0 {
		count++
		if count >= numAttributes {
			return true
		}
	}
	if s.nestedSetLeft > 0 || s.nestedSetRight > 0 || s.nestedSetParent != 0 { // nestedSetParent can be -1 meaning it is a root span
		count++
		if count >= numAttributes {
			return true
		}
	}
	// todo: attributesMatced is called a lot. we could cache this count on set
	for _, st := range s.spanAttrs {
		if st.s.Type != traceql.TypeNil {
			count++
			if count >= numAttributes {
				return true
			}
		}
	}
	for _, st := range s.resourceAttrs {
		if st.s.Type != traceql.TypeNil {
			count++
			if count >= numAttributes {
				return true
			}
		}
	}
	for _, st := range s.traceAttrs {
		if st.s.Type != traceql.TypeNil {
			count++
			if count >= numAttributes {
				return true
			}
		}
	}
	for _, st := range s.eventAttrs {
		if st.s.Type != traceql.TypeNil {
			count++
			if count >= numAttributes {
				return true
			}
		}
	}
	for _, st := range s.linkAttrs {
		if st.s.Type != traceql.TypeNil {
			count++
			if count >= numAttributes {
				return true
			}
		}
	}
	for _, st := range s.instrumentationAttrs {
		if st.s.Type != traceql.TypeNil {
			count++
			if count >= numAttributes {
				return true
			}
		}
	}

	return false
}

// todo: this sync pool currently massively reduces allocations by pooling spans for certain queries.
// it currently catches spans discarded:
// - in the span collector
// - in the batch collector
// - while converting to spanmeta
// to be fully effective it needs to catch spans thrown away in the query engine. perhaps filter spans
// can return a slice of dropped and kept spansets?
var spanPool = sync.Pool{
	New: func() interface{} {
		return &span{}
	},
}

func putSpan(s *span) {
	s.id = nil
	s.startTimeUnixNanos = 0
	s.durationNanos = 0
	s.row, s.trace, s.dropped = 0, 0, false
	s.cbSpansetFinal = false
	s.cbSpanset = nil
	s.nestedSetParent = 0
	s.nestedSetLeft = 0
	s.nestedSetRight = 0
	s.spanAttrs = s.spanAttrs[:0]
	s.resourceAttrs = s.resourceAttrs[:0]
	s.traceAttrs = s.traceAttrs[:0]
	s.eventAttrs = s.eventAttrs[:0]
	s.linkAttrs = s.linkAttrs[:0]
	s.instrumentationAttrs = s.instrumentationAttrs[:0]

	spanPool.Put(s)
}

func getSpan() *span {
	return spanPool.Get().(*span)
}

var spansetPool = sync.Pool{}

func getSpanset() *traceql.Spanset {
	ss := spansetPool.Get()
	if ss == nil {
		return &traceql.Spanset{
			ReleaseFn: putSpansetAndSpans,
		}
	}

	return ss.(*traceql.Spanset)
}

// putSpanset back into the pool.  Does not repool the spans.
func putSpanset(ss *traceql.Spanset) {
	ss.Attributes = ss.Attributes[:0]
	ss.DurationNanos = 0
	ss.RootServiceName = ""
	ss.RootSpanName = ""
	ss.Scalar = traceql.NewStaticNil()
	ss.StartTimeUnixNanos = 0
	ss.TraceID = nil
	clear(ss.ServiceStats)
	ss.Spans = ss.Spans[:0]

	spansetPool.Put(ss)
}

func putSpansetAndSpans(ss *traceql.Spanset) {
	if ss != nil {
		for _, s := range ss.Spans {
			if span, ok := s.(*span); ok {
				putSpan(span)
			}
		}
		putSpanset(ss)
	}
}

func otlpStatusToTraceqlStatus(v uint64) traceql.Status {
	// Map OTLP status code back to TraceQL enum.
	// For other values, use the raw integer.
	switch v {
	case uint64(v1.Status_STATUS_CODE_UNSET):
		return traceql.StatusUnset
	case uint64(v1.Status_STATUS_CODE_OK):
		return traceql.StatusOk
	case uint64(v1.Status_STATUS_CODE_ERROR):
		return traceql.StatusError
	default:
		return traceql.Status(v)
	}
}

func otlpKindToTraceqlKind(v uint64) traceql.Kind {
	switch v {
	case uint64(v1.Span_SPAN_KIND_UNSPECIFIED):
		return traceql.KindUnspecified
	case uint64(v1.Span_SPAN_KIND_INTERNAL):
		return traceql.KindInternal
	case uint64(v1.Span_SPAN_KIND_SERVER):
		return traceql.KindServer
	case uint64(v1.Span_SPAN_KIND_CLIENT):
		return traceql.KindClient
	case uint64(v1.Span_SPAN_KIND_PRODUCER):
		return traceql.KindProducer
	case uint64(v1.Span_SPAN_KIND_CONSUMER):
		return traceql.KindConsumer
	default:
		return traceql.Kind(v)
	}
}

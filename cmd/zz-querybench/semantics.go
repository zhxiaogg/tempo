// Independent (engine-free) counts for the resource-attribute and event-attribute semantics
// questions. See attrs.go for the file-level rationale.
//
// Everything here is computed by walking raw parquet columns with pkg/parquetquery
// iterators. No traceql engine, no Fetch, no spanset filtering. That is deliberate: the
// engine's answer to `{resource.k = "v"}` only means something when placed next to a number
// derived a different way. If the two agree, the semantics are established; if they do not,
// the engine is doing something other than what it looks like it is doing.
package main

import (
	"fmt"
	"sort"

	pq "github.com/parquet-go/parquet-go"

	"github.com/grafana/tempo/v3/pkg/parquetquery"
)

type valueSpanStat struct {
	Value string `json:"value"`
	// Spans is the number of spans that inherit / carry this value.
	Spans int64 `json:"spans"`
	// Parents is resources (resource scope) or spans (event scope) carrying the pair.
	Parents int64 `json:"parents"`
}

type resourceSemantics struct {
	Key   string `json:"key"`
	Value string `json:"value,omitempty"`

	ResourcesTotal        int64 `json:"resources_total"`
	ResourcesWithKey      int64 `json:"resources_with_key"`
	ResourcesWithKeyValue int64 `json:"resources_with_key_value"`
	// ResourcesWithMultipleValues is a resource carrying the SAME key twice with different
	// values. If this is 0, "the resource's value" is well defined.
	ResourcesWithMultipleValues int64 `json:"resources_with_multiple_values_for_key"`

	SpansTotal int64 `json:"spans_total"`
	// SpansUnderResourcesWithKeyValue is the 3a expectation: every span whose resource
	// carries key=value, counted once.
	SpansUnderResourcesWithKeyValue int64 `json:"spans_under_resources_with_key_value"`
	SpansUnderResourcesWithKey      int64 `json:"spans_under_resources_with_key"`
	// SpansUnderResourcesWithoutKey is the 3d expectation for the nil group.
	SpansUnderResourcesWithoutKey int64 `json:"spans_under_resources_without_key"`

	// DistinctValues / TopValues are the 3d expectation for `by (resource.<key>)`:
	// one group per distinct value, each span counted under its own resource's value.
	DistinctValues int             `json:"distinct_values"`
	TopValues      []valueSpanStat `json:"top_values_by_spans,omitempty"`
}

type eventSemantics struct {
	Key   string `json:"key"`
	Value string `json:"value,omitempty"`

	SpansTotal int64 `json:"spans_total"`
	// EventsTotal / EventAttrRows exclude the nulls a max-definition-level iterator yields
	// for spans with no events at all.
	EventsTotal   int64 `json:"events_total"`
	EventAttrRows int64 `json:"event_attr_rows_total"`

	// --- the key=value existence question (3b) ---
	// MatchingAttrRows counts every event attribute row with key=value: a span with three
	// such events contributes 3.
	MatchingAttrRows int64 `json:"matching_event_attr_rows"`
	// MatchingEvents counts distinct EVENTS carrying it (an event could in principle carry
	// the same key twice).
	MatchingEvents int64 `json:"matching_events"`
	// MatchingSpans counts distinct SPANS with at least one such event. If Tempo's span
	// count for `{event.k = "v"}` equals this and not MatchingAttrRows, the condition is
	// existential and the span is counted once.
	MatchingSpans int64 `json:"matching_spans"`
	// SpansWithMultipleMatchingEvents is what makes the two numbers above distinguishable.
	SpansWithMultipleMatchingEvents int64 `json:"spans_with_multiple_matching_events"`
	MaxMatchingEventsPerSpan        int64 `json:"max_matching_events_per_span"`

	// --- the grouping question (3c) ---
	SpansWithKey    int64 `json:"spans_with_key"`
	SpansWithoutKey int64 `json:"spans_without_key"`
	// SpanValuePairs sums, over spans carrying the key, the number of DISTINCT values that
	// span's events carry. If Tempo emitted a span once per distinct value, the grouped
	// query's total would be this; if it picked one value per span, it would be
	// SpansWithKey.
	SpanValuePairs int64 `json:"span_distinct_value_pairs"`
	// SpansWithMultipleDistinctValues > 0 is what makes 3c answerable at all.
	SpansWithMultipleDistinctValues int64 `json:"spans_with_multiple_distinct_values"`
	MaxDistinctValuesPerSpan        int64 `json:"max_distinct_values_per_span"`

	DistinctValues int             `json:"distinct_values"`
	TopValues      []valueSpanStat `json:"top_values_by_spans,omitempty"`

	// TopValuesFirstWins / TopValuesLastWins are the two ways a "one group per span"
	// implementation could resolve a span whose events disagree: attribute the span to the
	// value on its FIRST matching event (lowest event index) or on its LAST. They differ
	// only on the SpansWithMultipleDistinctValues spans, and comparing them against Tempo's
	// grouped series is what settles 3c.
	TopValuesFirstWins []valueSpanStat `json:"top_values_first_event_wins,omitempty"`
	TopValuesLastWins  []valueSpanStat `json:"top_values_last_event_wins,omitempty"`

	// FirstMultiValueSpanExample names one span (by its packed row number) that carries
	// several distinct values, with those values, so a claim about 3c can be spot-checked.
	ExampleMultiValueSpanValues []string `json:"example_multi_value_span_values,omitempty"`
}

type semanticsResult struct {
	Mode          string `json:"mode"`
	Scope         string `json:"scope"`
	Block         string `json:"block"`
	StartRowGroup int    `json:"start_row_group"`
	RowGroups     int    `json:"row_groups"`

	Method string `json:"method"`

	Resource *resourceSemantics `json:"resource,omitempty"`
	Event    *eventSemantics    `json:"event,omitempty"`

	Env env `json:"env"`
}

func bytePred(s string) parquetquery.Predicate {
	return parquetquery.NewByteInPredicate([][]byte{[]byte(s)})
}

func runSemantics(blockDir string, startRG, nRGs int, scope, key, value string) (*semanticsResult, error) {
	b, err := openBlockFile(blockDir)
	if err != nil {
		return nil, err
	}
	defer b.Close()
	rgs, err := b.rowGroups(startRG, nRGs)
	if err != nil {
		return nil, err
	}

	res := &semanticsResult{
		Mode: "semantics", Scope: scope, Block: blockDir,
		StartRowGroup: startRG, RowGroups: nRGs, Env: currentEnv(),
		Method: "raw pkg/parquetquery column walk; NO traceql engine, NO Fetch, NO spanset " +
			"filtering. Counts here are the independent expectation the engine's answer is " +
			"compared against.",
	}

	switch scope {
	case "resource":
		r, err := b.resourceSemantics(rgs, key, value)
		if err != nil {
			return nil, err
		}
		res.Resource = r
	case "event":
		e, err := b.eventSemantics(rgs, key, value)
		if err != nil {
			return nil, err
		}
		res.Event = e
	default:
		return nil, fmt.Errorf("-sem-scope %q, want resource|event", scope)
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// resource scope
// ---------------------------------------------------------------------------

type rnVal struct {
	rn  parquetquery.RowNumber
	val string
}

func (b *blockFile) resourceSemantics(rgs []pq.RowGroup, key, value string) (*resourceSemantics, error) {
	out := &resourceSemantics{Key: key, Value: value}

	// Pass 1: every (resource, value) for this key, in row order.
	ki, err := b.sync(rgs, colResourceAttrKey, "key", bytePred(key))
	if err != nil {
		return nil, err
	}
	vi, err := b.sync(rgs, colResourceAttrVal, "val", nil)
	if err != nil {
		return nil, err
	}
	it := parquetquery.NewJoinIterator(defLevelResourceAttrs,
		[]parquetquery.Iterator{ki, vi}, nil)

	var (
		withKey    []rnVal
		lastRes    parquetquery.RowNumber
		haveLast   bool
		valsForRes = map[string]struct{}{}
	)
	flushRes := func() {
		if haveLast {
			out.ResourcesWithKey++
			if len(valsForRes) > 1 {
				out.ResourcesWithMultipleValues++
			}
		}
	}
	for {
		r, err := it.Next()
		if err != nil {
			it.Close()
			return nil, err
		}
		if r == nil {
			break
		}
		var v string
		for _, e := range r.Entries {
			if e.Key == "val" {
				v = e.Value.Clone().String()
			}
		}
		resRN := parquetquery.TruncateRowNumber(defLevelResourceSpans, r.RowNumber)
		if !haveLast || resRN != lastRes {
			flushRes()
			lastRes, haveLast = resRN, true
			valsForRes = map[string]struct{}{}
		}
		valsForRes[v] = struct{}{}
		withKey = append(withKey, rnVal{rn: resRN, val: v})
		if v == value {
			// counted below per resource, not per row
			_ = v
		}
	}
	flushRes()
	it.Close()

	// resources carrying key=value, counted once each
	{
		var last parquetquery.RowNumber
		var have bool
		for _, e := range withKey {
			if e.val != value {
				continue
			}
			if !have || e.rn != last {
				out.ResourcesWithKeyValue++
				last, have = e.rn, true
			}
		}
	}

	// Pass 2: walk every span once, attributing it to its resource.
	si, err := b.sync(rgs, colSpanNameForRow, "n", nil)
	if err != nil {
		return nil, err
	}
	defer si.Close()

	perValue := map[string]int64{}
	i := 0
	var resTotal parentCounter
	resTotal.level = defLevelResourceSpans
	for {
		r, err := si.Next()
		if err != nil {
			return nil, err
		}
		if r == nil {
			break
		}
		var n int64
		for _, e := range r.Entries {
			if !e.Value.IsNull() {
				n++
			}
		}
		if n == 0 {
			continue
		}
		out.SpansTotal += n
		resRN := parquetquery.TruncateRowNumber(defLevelResourceSpans, r.RowNumber)
		resTotal.observe(r.RowNumber)

		// advance to this resource
		for i < len(withKey) && rnLess(withKey[i].rn, resRN) {
			i++
		}
		if i >= len(withKey) || withKey[i].rn != resRN {
			out.SpansUnderResourcesWithoutKey += n
			continue
		}
		out.SpansUnderResourcesWithKey += n
		// all values this resource carries for the key
		seen := map[string]struct{}{}
		for j := i; j < len(withKey) && withKey[j].rn == resRN; j++ {
			seen[withKey[j].val] = struct{}{}
		}
		for v := range seen {
			perValue[v] += n
			if v == value {
				out.SpansUnderResourcesWithKeyValue += n
			}
		}
	}
	out.ResourcesTotal = resTotal.n

	out.DistinctValues = len(perValue)
	tv := make([]valueSpanStat, 0, len(perValue))
	for v, n := range perValue {
		tv = append(tv, valueSpanStat{Value: v, Spans: n})
	}
	sort.Slice(tv, func(a, c int) bool {
		if tv[a].Spans != tv[c].Spans {
			return tv[a].Spans > tv[c].Spans
		}
		return tv[a].Value < tv[c].Value
	})
	if len(tv) > 25 {
		tv = tv[:25]
	}
	out.TopValues = tv
	return out, nil
}

// rnLess compares two row numbers lexicographically. Truncated levels hold -1, which sorts
// before any real index, so this is the same order the iterators produce.
func rnLess(a, b parquetquery.RowNumber) bool {
	for i := 0; i < len(a); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// event scope
// ---------------------------------------------------------------------------

func (b *blockFile) eventSemantics(rgs []pq.RowGroup, key, value string) (*eventSemantics, error) {
	out := &eventSemantics{Key: key, Value: value}

	// totals, for denominators
	n, err := b.countSpans(rgs)
	if err != nil {
		return nil, err
	}
	out.SpansTotal = n

	if ec, err := b.countColumn(rgs, colEventName); err == nil {
		out.EventsTotal = ec
	} else {
		return nil, err
	}
	if ac, err := b.countColumn(rgs, colEventAttrKey); err == nil {
		out.EventAttrRows = ac
	} else {
		return nil, err
	}

	// One walk over every event attribute row whose KEY matches, carrying the value along.
	// Rows arrive in row order, so spans can be closed out as their last row goes by and
	// nothing needs a global map.
	ki, err := b.sync(rgs, colEventAttrKey, "key", bytePred(key))
	if err != nil {
		return nil, err
	}
	vi, err := b.sync(rgs, colEventAttrVal, "val", nil)
	if err != nil {
		return nil, err
	}
	it := parquetquery.NewJoinIterator(defLevelSpanEventAttrs,
		[]parquetquery.Iterator{ki, vi}, nil)
	defer it.Close()

	perValueSpans := map[string]int64{}
	perValueFirst := map[string]int64{}
	perValueLast := map[string]int64{}

	var (
		curFirst      string
		curLast       string
		curSpan       parquetquery.RowNumber
		haveSpan      bool
		curVals       = map[string]struct{}{}
		curMatchRows  int64
		curMatchEvent parquetquery.RowNumber
		haveMatchEv   bool
		curMatchEvs   int64
	)
	closeSpan := func() {
		if !haveSpan {
			return
		}
		out.SpansWithKey++
		out.SpanValuePairs += int64(len(curVals))
		if len(curVals) > 1 {
			out.SpansWithMultipleDistinctValues++
			if out.ExampleMultiValueSpanValues == nil {
				vs := make([]string, 0, len(curVals))
				for v := range curVals {
					vs = append(vs, v)
				}
				sort.Strings(vs)
				out.ExampleMultiValueSpanValues = vs
			}
		}
		if int64(len(curVals)) > out.MaxDistinctValuesPerSpan {
			out.MaxDistinctValuesPerSpan = int64(len(curVals))
		}
		for v := range curVals {
			perValueSpans[v]++
		}
		perValueFirst[curFirst]++
		perValueLast[curLast]++
		if curMatchRows > 0 {
			out.MatchingSpans++
			if curMatchEvs > 1 {
				out.SpansWithMultipleMatchingEvents++
			}
			if curMatchEvs > out.MaxMatchingEventsPerSpan {
				out.MaxMatchingEventsPerSpan = curMatchEvs
			}
		}
	}

	for {
		r, err := it.Next()
		if err != nil {
			return nil, err
		}
		if r == nil {
			break
		}
		var v string
		for _, e := range r.Entries {
			if e.Key == "val" {
				v = e.Value.Clone().String()
			}
		}
		spanRN := parquetquery.TruncateRowNumber(defLevelSpan, r.RowNumber)
		if !haveSpan || spanRN != curSpan {
			closeSpan()
			curSpan, haveSpan = spanRN, true
			curVals = map[string]struct{}{}
			curMatchRows, curMatchEvs = 0, 0
			haveMatchEv = false
			curFirst = v
		}
		curVals[v] = struct{}{}
		curLast = v
		if value != "" && v == value {
			out.MatchingAttrRows++
			curMatchRows++
			evRN := parquetquery.TruncateRowNumber(defLevelSpanEvent, r.RowNumber)
			if !haveMatchEv || evRN != curMatchEvent {
				out.MatchingEvents++
				curMatchEvent, haveMatchEv = evRN, true
				curMatchEvs++
			}
		}
	}
	closeSpan()

	out.SpansWithoutKey = out.SpansTotal - out.SpansWithKey
	out.DistinctValues = len(perValueSpans)
	tv := make([]valueSpanStat, 0, len(perValueSpans))
	for v, c := range perValueSpans {
		tv = append(tv, valueSpanStat{Value: v, Spans: c})
	}
	sort.Slice(tv, func(a, c int) bool {
		if tv[a].Spans != tv[c].Spans {
			return tv[a].Spans > tv[c].Spans
		}
		return tv[a].Value < tv[c].Value
	})
	if len(tv) > 25 {
		tv = tv[:25]
	}
	out.TopValues = tv
	out.TopValuesFirstWins = topValueSpans(perValueFirst)
	out.TopValuesLastWins = topValueSpans(perValueLast)
	return out, nil
}

// topValueSpans sorts a value -> span-count map into the reporting shape.
func topValueSpans(m map[string]int64) []valueSpanStat {
	out := make([]valueSpanStat, 0, len(m))
	for v, c := range m {
		out = append(out, valueSpanStat{Value: v, Spans: c})
	}
	sort.Slice(out, func(a, c int) bool {
		if out[a].Spans != out[c].Spans {
			return out[a].Spans > out[c].Spans
		}
		return out[a].Value < out[c].Value
	})
	if len(out) > 25 {
		out = out[:25]
	}
	return out
}

// countColumn counts NON-NULL values present in one column over the window. A SyncIterator
// at a column's own max definition level also yields one null per parent that has no child
// at all (a span with no events, a resource with no attributes); those are not values and
// are not counted.
func (b *blockFile) countColumn(rgs []pq.RowGroup, path string) (int64, error) {
	si, err := b.sync(rgs, path, "v", nil)
	if err != nil {
		return 0, err
	}
	defer si.Close()
	var n int64
	for {
		r, err := si.Next()
		if err != nil {
			return 0, err
		}
		if r == nil {
			break
		}
		for _, e := range r.Entries {
			if !e.Value.IsNull() {
				n++
			}
		}
	}
	return n, nil
}

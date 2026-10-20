// Attribute discovery and independent (non-engine) ground truth for zz-querybench.
//
// Two modes live here, both of them read-only reporting tools rather than benchmarks:
//
//	-mode attrs      What is actually in the block. Per key: how many attribute rows carry
//	                 it, how many distinct PARENTS (resources for resource attrs, spans for
//	                 event attrs) carry it, and the top values by row count. This is how the
//	                 resource/event ground-truth queries were chosen instead of guessed.
//
//	-mode semantics  The numbers that answer "what does a resource/event condition mean",
//	                 computed straight off the raw parquet columns with NO traceql engine
//	                 involved. That is the point: the engine's answer for
//	                 `{resource.k = "v"}` is only meaningful next to an independently
//	                 counted "spans under resources carrying k=v".
//
// Both honour -startrg/-rgs, so every number is over the same row-group window as the rest
// of the harness (FAIRNESS rule 1).
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	pq "github.com/parquet-go/parquet-go"

	"github.com/grafana/tempo/v3/pkg/parquetquery"
)

// Column paths mirrored from vparquet5 (they are unexported there). Every one is asserted
// against the block's own schema before use, so a rename cannot silently produce zeros.
const (
	colResourceAttrKey = "rs.Resource.Attrs.Key"
	colResourceAttrVal = "rs.Resource.Attrs.Value"
	colResourceService = "rs.Resource.ServiceName"

	colEventName      = "rs.ss.Spans.Events.Name"
	colEventAttrKey   = "rs.ss.Spans.Events.Attrs.Key"
	colEventAttrVal   = "rs.ss.Spans.Events.Attrs.Value"
	colSpanNameForRow = "rs.ss.Spans.Name"

	// vparquet5 schema.go
	defLevelResourceSpans  = 1 // a resource
	defLevelResourceAttrs  = 2
	defLevelSpan           = 3
	defLevelSpanEvent      = 4
	defLevelSpanEventAttrs = 5
)

// ---------------------------------------------------------------------------
// shared plumbing
// ---------------------------------------------------------------------------

type blockFile struct {
	f  *os.File
	pf *pq.File
}

func openBlockFile(blockDir string) (*blockFile, error) {
	f, err := os.Open(filepath.Join(blockDir, "data.parquet"))
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	pf, err := pq.OpenFile(f, st.Size())
	if err != nil {
		f.Close()
		return nil, err
	}
	return &blockFile{f: f, pf: pf}, nil
}

func (b *blockFile) Close() { b.f.Close() }

func (b *blockFile) rowGroups(startRG, nRGs int) ([]pq.RowGroup, error) {
	all := b.pf.RowGroups()
	if startRG < 0 || nRGs <= 0 || startRG+nRGs > len(all) {
		return nil, fmt.Errorf("block has %d row groups, asked for [%d,%d)", len(all), startRG, startRG+nRGs)
	}
	return all[startRG : startRG+nRGs], nil
}

func (b *blockFile) col(path string) (idx, maxDef int, err error) {
	idx, _, maxDef = parquetquery.GetColumnIndexByPath(b.pf, path)
	if idx < 0 {
		return 0, 0, fmt.Errorf("column %q not found in this block's schema", path)
	}
	return idx, maxDef, nil
}

// sync builds a plain SyncIterator over one column with no predicate.
func (b *blockFile) sync(rgs []pq.RowGroup, path, as string, pred parquetquery.Predicate) (*parquetquery.SyncIterator, error) {
	idx, maxDef, err := b.col(path)
	if err != nil {
		return nil, err
	}
	opts := []parquetquery.SyncIteratorOpt{
		parquetquery.SyncIteratorOptMaxDefinitionLevel(maxDef),
		parquetquery.SyncIteratorOptSelectAs(as),
	}
	if pred != nil {
		opts = append(opts, parquetquery.SyncIteratorOptPredicate(pred))
	}
	return parquetquery.NewSyncIterator(context.Background(), rgs, idx, opts...), nil
}

// parentCounter counts DISTINCT parent rows in a stream of results that arrives in row
// order. Because the stream is ordered, a parent is distinct exactly when it differs from
// the previous one, so this needs no set and no memory proportional to the data.
type parentCounter struct {
	level int
	have  bool
	last  parquetquery.RowNumber
	n     int64
}

func (p *parentCounter) observe(rn parquetquery.RowNumber) {
	t := parquetquery.TruncateRowNumber(p.level, rn)
	if !p.have || t != p.last {
		p.n++
		p.last = t
		p.have = true
	}
}

// ---------------------------------------------------------------------------
// -mode attrs
// ---------------------------------------------------------------------------

type valueStat struct {
	Value string `json:"value"`
	Rows  int64  `json:"rows"`
	// Parents is the number of distinct parents (resources / spans) carrying this exact
	// key=value pair.
	Parents int64 `json:"parents"`
}

type keyStat struct {
	Key     string `json:"key"`
	Rows    int64  `json:"rows"`
	Parents int64  `json:"parents"`
	// DistinctValues is the number of distinct values seen for this key. Values is the top
	// slice of them by row count.
	DistinctValues int         `json:"distinct_values"`
	Values         []valueStat `json:"top_values,omitempty"`

	// MultiPerParent is true when at least one parent carries this key on more than one
	// child row (rows > parents). For event attributes that is precisely the shape question
	// 3b needs: a key repeated across several events of the SAME span.
	MultiPerParent bool `json:"multi_per_parent"`
}

type scopeStats struct {
	Scope      string `json:"scope"`
	KeyColumn  string `json:"key_column"`
	ValColumn  string `json:"value_column"`
	DefLevel   int    `json:"definition_level"`
	ParentName string `json:"parent"`
	ParentDef  int    `json:"parent_definition_level"`

	AttrRows        int64 `json:"attr_rows"`
	DistinctKeys    int   `json:"distinct_keys"`
	DistinctParents int64 `json:"distinct_parents_with_any_attr"`
	// NullRows are rows the iterator returns for a PARENT THAT HAS NO CHILD at all: a
	// SyncIterator at the column's own max definition level yields one null per empty
	// parent. They are not attributes and are excluded from every other figure here.
	NullRows int64 `json:"null_rows"`

	// Truncated is true if the (key,value) cardinality guard kicked in, so the value
	// histograms are a floor rather than the truth. Key counts are always exact.
	Truncated bool `json:"value_histogram_truncated"`

	Keys []keyStat `json:"keys"`
}

type attrsResult struct {
	Mode          string `json:"mode"`
	Block         string `json:"block"`
	StartRowGroup int    `json:"start_row_group"`
	RowGroups     int    `json:"row_groups"`
	TopN          int    `json:"top_n"`

	// Spans in the window, so every "parents" figure has a denominator.
	Spans int64 `json:"spans"`

	Scopes []scopeStats `json:"scopes"`

	// Event names are a dedicated column, not an attribute, so they get their own histogram.
	EventNames *scopeStats `json:"event_names,omitempty"`
	// Likewise rs.Resource.ServiceName is a dedicated top-level column, NOT a row in
	// rs.Resource.Attrs.
	ServiceNames *scopeStats `json:"service_names,omitempty"`

	Env env `json:"env"`
}

const valueHistogramGuard = 4_000_000

// histogram walks a key/value column pair joined at defLevel and builds the per-key and
// per-(key,value) counts.
func (b *blockFile) histogram(rgs []pq.RowGroup, scope, keyPath, valPath string, defLevel, parentDef int, parentName string, topN int) (*scopeStats, error) {
	ki, err := b.sync(rgs, keyPath, "key", nil)
	if err != nil {
		return nil, err
	}
	vi, err := b.sync(rgs, valPath, "val", nil)
	if err != nil {
		return nil, err
	}
	it := parquetquery.NewJoinIterator(defLevel, []parquetquery.Iterator{ki, vi}, nil)
	defer it.Close()

	st := &scopeStats{
		Scope: scope, KeyColumn: keyPath, ValColumn: valPath, DefLevel: defLevel,
		ParentName: parentName, ParentDef: parentDef,
	}

	type agg struct {
		rows    int64
		parents parentCounter
		vals    map[string]*struct {
			rows    int64
			parents parentCounter
		}
	}
	keys := map[string]*agg{}
	anyParents := parentCounter{level: parentDef}
	nPairs := 0

	for {
		r, err := it.Next()
		if err != nil {
			return nil, err
		}
		if r == nil {
			break
		}
		var k, v string
		var kNull bool
		for _, e := range r.Entries {
			switch e.Key {
			case "key":
				kNull = e.Value.IsNull()
				k = e.Value.Clone().String()
			case "val":
				v = e.Value.Clone().String()
			}
		}
		if kNull {
			st.NullRows++
			continue
		}
		st.AttrRows++
		anyParents.observe(r.RowNumber)

		a := keys[k]
		if a == nil {
			a = &agg{parents: parentCounter{level: parentDef}}
			a.vals = map[string]*struct {
				rows    int64
				parents parentCounter
			}{}
			keys[k] = a
		}
		a.rows++
		a.parents.observe(r.RowNumber)

		vs := a.vals[v]
		if vs == nil {
			if nPairs >= valueHistogramGuard {
				st.Truncated = true
				continue
			}
			vs = &struct {
				rows    int64
				parents parentCounter
			}{parents: parentCounter{level: parentDef}}
			a.vals[v] = vs
			nPairs++
		}
		vs.rows++
		vs.parents.observe(r.RowNumber)
	}

	st.DistinctKeys = len(keys)
	st.DistinctParents = anyParents.n
	for k, a := range keys {
		ks := keyStat{
			Key: k, Rows: a.rows, Parents: a.parents.n,
			DistinctValues: len(a.vals),
			MultiPerParent: a.rows > a.parents.n,
		}
		vals := make([]valueStat, 0, len(a.vals))
		for v, vs := range a.vals {
			vals = append(vals, valueStat{Value: v, Rows: vs.rows, Parents: vs.parents.n})
		}
		sort.Slice(vals, func(i, j int) bool {
			if vals[i].Rows != vals[j].Rows {
				return vals[i].Rows > vals[j].Rows
			}
			return vals[i].Value < vals[j].Value
		})
		if topN > 0 && len(vals) > topN {
			vals = vals[:topN]
		}
		ks.Values = vals
		st.Keys = append(st.Keys, ks)
	}
	sort.Slice(st.Keys, func(i, j int) bool {
		if st.Keys[i].Rows != st.Keys[j].Rows {
			return st.Keys[i].Rows > st.Keys[j].Rows
		}
		return st.Keys[i].Key < st.Keys[j].Key
	})
	return st, nil
}

// singleColumnHistogram is the same thing for a column that is a value with no key beside it
// (event name, resource service name). It is reported as one synthetic "key".
func (b *blockFile) singleColumnHistogram(rgs []pq.RowGroup, scope, path string, defLevel, parentDef int, parentName string, topN int) (*scopeStats, error) {
	si, err := b.sync(rgs, path, "v", nil)
	if err != nil {
		return nil, err
	}
	defer si.Close()

	st := &scopeStats{
		Scope: scope, ValColumn: path, DefLevel: defLevel,
		ParentName: parentName, ParentDef: parentDef,
	}
	counts := map[string]*struct {
		rows    int64
		parents parentCounter
	}{}
	anyParents := parentCounter{level: parentDef}
	for {
		r, err := si.Next()
		if err != nil {
			return nil, err
		}
		if r == nil {
			break
		}
		for _, e := range r.Entries {
			if e.Value.IsNull() {
				st.NullRows++
				continue
			}
			v := e.Value.Clone().String()
			st.AttrRows++
			anyParents.observe(r.RowNumber)
			c := counts[v]
			if c == nil {
				c = &struct {
					rows    int64
					parents parentCounter
				}{parents: parentCounter{level: parentDef}}
				counts[v] = c
			}
			c.rows++
			c.parents.observe(r.RowNumber)
		}
	}
	st.DistinctParents = anyParents.n
	st.DistinctKeys = 1
	vals := make([]valueStat, 0, len(counts))
	for v, c := range counts {
		vals = append(vals, valueStat{Value: v, Rows: c.rows, Parents: c.parents.n})
	}
	sort.Slice(vals, func(i, j int) bool {
		if vals[i].Rows != vals[j].Rows {
			return vals[i].Rows > vals[j].Rows
		}
		return vals[i].Value < vals[j].Value
	})
	full := len(vals)
	if topN > 0 && len(vals) > topN {
		vals = vals[:topN]
	}
	st.Keys = []keyStat{{
		Key: path, Rows: st.AttrRows, Parents: anyParents.n,
		DistinctValues: full, Values: vals,
	}}
	return st, nil
}

// countSpans counts spans by walking one always-present span column. Nulls (yielded once per
// parent that has no span at all) are excluded.
func (b *blockFile) countSpans(rgs []pq.RowGroup) (int64, error) {
	si, err := b.sync(rgs, colSpanNameForRow, "n", nil)
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

func runAttrs(blockDir string, startRG, nRGs, topN int, scopes string) (*attrsResult, error) {
	b, err := openBlockFile(blockDir)
	if err != nil {
		return nil, err
	}
	defer b.Close()
	rgs, err := b.rowGroups(startRG, nRGs)
	if err != nil {
		return nil, err
	}

	res := &attrsResult{
		Mode: "attrs", Block: blockDir, StartRowGroup: startRG, RowGroups: nRGs,
		TopN: topN, Env: currentEnv(),
	}

	spans, err := b.countSpans(rgs)
	if err != nil {
		return nil, err
	}
	res.Spans = spans

	want := func(s string) bool { return scopes == "all" || scopes == s }

	if want("resource") {
		st, err := b.histogram(rgs, "resource", colResourceAttrKey, colResourceAttrVal,
			defLevelResourceAttrs, defLevelResourceSpans, "resource", topN)
		if err != nil {
			return nil, err
		}
		res.Scopes = append(res.Scopes, *st)

		sn, err := b.singleColumnHistogram(rgs, "resource.service.name (dedicated column)",
			colResourceService, defLevelResourceSpans, defLevelResourceSpans, "resource", topN)
		if err != nil {
			return nil, err
		}
		res.ServiceNames = sn
	}
	if want("event") {
		st, err := b.histogram(rgs, "event", colEventAttrKey, colEventAttrVal,
			defLevelSpanEventAttrs, defLevelSpan, "span", topN)
		if err != nil {
			return nil, err
		}
		res.Scopes = append(res.Scopes, *st)

		en, err := b.singleColumnHistogram(rgs, "event name (dedicated column)",
			colEventName, defLevelSpanEvent, defLevelSpan, "span", topN)
		if err != nil {
			return nil, err
		}
		res.EventNames = en
	}
	if want("span") {
		st, err := b.histogram(rgs, "span", columnPathSpanAttrKey, columnPathSpanAttrStr,
			attrDefinitionLevel, defLevelSpan, "span", topN)
		if err != nil {
			return nil, err
		}
		res.Scopes = append(res.Scopes, *st)
	}
	return res, nil
}

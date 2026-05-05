package vparquet5

import (
	"sync"

	"github.com/grafana/tempo/pkg/parquetquery"
	"github.com/grafana/tempo/pkg/traceql"
	"github.com/grafana/tempo/tempodb/backend"
	parquet "github.com/parquet-go/parquet-go"
)

// hasKeyFn reports whether the given attribute key is present in the given scope
// for the current fetch (e.g. block + selected row groups). It must answer
// conservatively: when uncertain, return true so the caller leaves the condition
// alone.
type hasKeyFn func(scope traceql.AttributeScope, key string) bool

// demingleUnscopedConditions rewrites the Scope of each unscoped non-intrinsic
// attribute condition to a concrete scope (Span or Resource) when the data in
// the current fetch only carries the key in that one scope.
//
// This breaks the "mingled" categorization in createAllIterator/categorizeConditions
// that otherwise forces AllConditions=false and disables coalesce_conditions.
//
// Conditions that are intrinsic, already scoped, or whose key is present in both
// (or neither) scope are returned unchanged.
//
// Operator safety: the demotion is sound for every TraceQL operator under the
// invariant "scope Y has zero rows for key K in this fetch". Predicates that
// would otherwise be evaluated against scope Y produce no contribution, so
// collapsing the union to scope X is equivalent.
func demingleUnscopedConditions(conds []traceql.Condition, hasKey hasKeyFn) []traceql.Condition {
	out := make([]traceql.Condition, len(conds))
	for i, c := range conds {
		out[i] = c
		if c.Attribute.Scope != traceql.AttributeScopeNone {
			continue
		}
		if c.Attribute.Intrinsic != traceql.IntrinsicNone {
			continue
		}

		spanHas := hasKey(traceql.AttributeScopeSpan, c.Attribute.Name)
		resHas := hasKey(traceql.AttributeScopeResource, c.Attribute.Name)

		switch {
		case spanHas && !resHas:
			out[i].Attribute.Scope = traceql.AttributeScopeSpan
		case !spanHas && resHas:
			out[i].Attribute.Scope = traceql.AttributeScopeResource
		}
	}
	return out
}

// EnableDemingleUnscopedConditions controls whether createAllIterator runs the
// demingle pass. Exposed as a package-level var so benchmarks (and operators
// in extreme cases) can toggle the optimization. Default: enabled.
var EnableDemingleUnscopedConditions = true

// containsUnscopedNonIntrinsic reports whether any condition would be a
// candidate for demingleUnscopedConditions. Used to short-circuit the demingle
// pass entirely (and skip newParquetHasKeyFn's setup work) when there is
// nothing to rewrite — the common case for most queries.
func containsUnscopedNonIntrinsic(conds []traceql.Condition) bool {
	for _, c := range conds {
		if c.Attribute.Scope == traceql.AttributeScopeNone && c.Attribute.Intrinsic == traceql.IntrinsicNone {
			return true
		}
	}
	return false
}

// newParquetHasKeyFn builds a hasKeyFn backed by:
//  1. the dedicated-column mappings for span and resource scope (a key configured
//     as dedicated in scope X is present in X regardless of dictionary contents).
//  2. for keys not in the dedicated set, the attribute-key column dictionaries
//     across the given row groups.
//
// pf and rgs may be nil — when nil, only the dedicated-column check is consulted.
// This allows callers and tests to exercise the dedicated path in isolation.
func newParquetHasKeyFn(pf *parquet.File, rgs []parquet.RowGroup, dc backend.DedicatedColumns) hasKeyFn {
	spanDedicated := dedicatedColumnsToColumnMapping(dc, backend.DedicatedColumnScopeSpan)
	resDedicated := dedicatedColumnsToColumnMapping(dc, backend.DedicatedColumnScopeResource)

	spanKeyColIdx, resKeyColIdx := -1, -1
	if pf != nil {
		spanKeyColIdx, _, _ = parquetquery.GetColumnIndexByPath(pf, FieldSpanAttrKey)
		resKeyColIdx, _, _ = parquetquery.GetColumnIndexByPath(pf, FieldResourceAttrKey)
	}

	var (
		spanOnce   sync.Once
		spanSet    map[string]struct{}
		spanOpaque bool
		resOnce    sync.Once
		resSet     map[string]struct{}
		resOpaque  bool
	)

	loadSpan := func() {
		if spanKeyColIdx < 0 || len(rgs) == 0 {
			spanOpaque = false
			spanSet = nil
			return
		}
		spanSet, spanOpaque = parquetquery.LoadRowGroupDictionaryStringSet(rgs, spanKeyColIdx)
	}
	loadRes := func() {
		if resKeyColIdx < 0 || len(rgs) == 0 {
			resOpaque = false
			resSet = nil
			return
		}
		resSet, resOpaque = parquetquery.LoadRowGroupDictionaryStringSet(rgs, resKeyColIdx)
	}

	return func(scope traceql.AttributeScope, key string) bool {
		switch scope {
		case traceql.AttributeScopeSpan:
			if _, ok := spanDedicated.get(key); ok {
				return true
			}
			spanOnce.Do(loadSpan)
			if spanOpaque {
				return true // cannot prove absence
			}
			_, ok := spanSet[key]
			return ok
		case traceql.AttributeScopeResource:
			if _, ok := resDedicated.get(key); ok {
				return true
			}
			resOnce.Do(loadRes)
			if resOpaque {
				return true
			}
			_, ok := resSet[key]
			return ok
		}
		return false
	}
}

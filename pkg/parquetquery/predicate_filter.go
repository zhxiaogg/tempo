package parquetquery

import pq "github.com/parquet-go/parquet-go"

// predicateStats counts the chunk/page filtering decisions made by the generic
// keep* helpers. It is optional (nil-safe) and, when supplied, lets callers such
// as InstrumentedPredicate report inspected/kept counts without the Predicate
// interface needing chunk/page methods.
type predicateStats struct {
	InspectedColumnChunks int64
	KeptColumnChunks      int64
	InspectedPages        int64
	KeptPages             int64
}

// predicateNullValue returns the null pq.Value fed to KeepValue when deciding
// whether null rows in a chunk/page could match. It must be the same null
// representation the per-row value reader produces so the skip decision agrees
// with per-row evaluation.
func predicateNullValue() pq.Value { return pq.Value{} }

// keepColumnChunk reports whether any value in the column chunk can match pred.
// It unifies the chunk-level skipping formerly duplicated across every predicate:
// a dictionary any-match for dict-encoded (string) columns, a column-index range
// test via KeepRange, and a null-count test via KeepValue(null).
func keepColumnChunk(pred Predicate, cc *ColumnChunkHelper, stats *predicateStats) bool {
	if stats != nil {
		stats.InspectedColumnChunks++
	}
	keep := keepColumnChunkInner(pred, cc)
	if keep && stats != nil {
		stats.KeptColumnChunks++
	}
	return keep
}

func keepColumnChunkInner(pred Predicate, cc *ColumnChunkHelper) bool {
	if pred == nil {
		return true // no predicate: keep everything
	}
	keepNull := pred.KeepValue(predicateNullValue())

	if d := cc.Dictionary(); d != nil {
		// Dict-encoded (string) chunk: any present value that matches lives in the
		// dictionary; null rows are decided by keepNull since nulls are not stored
		// in the dictionary.
		if keepDictionary(d, pred.KeepValue) {
			return true
		}
		return keepNull && chunkHasNulls(cc)
	}

	ci, err := cc.ColumnIndex()
	if err != nil || ci == nil {
		return true // no column index: cannot skip
	}
	for i := 0; i < ci.NumPages(); i++ {
		if ci.NullPage(i) {
			// All-null page: min/max are not recorded, only the null decision applies.
			if keepNull && ci.NullCount(i) > 0 {
				return true
			}
			continue
		}
		if pred.KeepRange(ci.MinValue(i), ci.MaxValue(i)) {
			return true
		}
		if keepNull && ci.NullCount(i) > 0 {
			return true
		}
	}
	return false
}

func chunkHasNulls(cc *ColumnChunkHelper) bool {
	ci, err := cc.ColumnIndex()
	if err != nil || ci == nil {
		return true // unknown: assume nulls may be present
	}
	for i := 0; i < ci.NumPages(); i++ {
		if ci.NullCount(i) > 0 {
			return true
		}
	}
	return false
}

// keepPage reports whether any value in the page can match pred, combining a
// page-bounds range test via KeepRange with a null test via KeepValue(null).
func keepPage(pred Predicate, pg pq.Page, stats *predicateStats) bool {
	if stats != nil {
		stats.InspectedPages++
	}
	keep := keepPageInner(pred, pg)
	if keep && stats != nil {
		stats.KeptPages++
	}
	return keep
}

func keepPageInner(pred Predicate, pg pq.Page) bool {
	if pred == nil {
		return true // no predicate: keep everything
	}
	keepNull := pred.KeepValue(predicateNullValue())
	if keepNull && pg.NumNulls() > 0 {
		return true
	}
	if pg.NumValues()-pg.NumNulls() <= 0 {
		// No present values on this page; only the null decision (handled above) applies.
		return false
	}
	if min, max, ok := pg.Bounds(); ok {
		return pred.KeepRange(min, max)
	}
	return true // no bounds recorded: cannot skip
}

// keepDictionary reports whether any dictionary entry matches keepValue.
func keepDictionary(dict pq.Dictionary, keepValue func(pq.Value) bool) bool {
	for i, l := 0, dict.Len(); i < l; i++ {
		if keepValue(dict.Index(int32(i))) {
			return true
		}
	}
	return false
}

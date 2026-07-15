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

// columnChunkMatches reports whether any value in the column chunk can match pred.
// It combines a dictionary any-match for dict-encoded (string) columns, a column-index
// range test via KeepRange, and a null-count test via KeepValue(null). Stats-aware.
func columnChunkMatches(pred Predicate, cc *ColumnChunkHelper, stats *predicateStats) bool {
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

// dictionaryKeepBitmap resolves keep against every distinct dictionary value once,
// returning a bitmap where entry i is true iff dict.Index(i) matches. Paid once per
// column chunk rather than once per row.
func dictionaryKeepBitmap(dict pq.Dictionary, keep func(pq.Value) bool) []bool {
	out := make([]bool, dict.Len())
	for i := range out {
		out[i] = keep(dict.Index(int32(i)))
	}
	return out
}

// anyTrue reports whether any entry in b is true.
func anyTrue(b []bool) bool {
	for _, v := range b {
		if v {
			return true
		}
	}
	return false
}

// keepColumnChunk decides whether to keep cc and, as a side effect, resolves the
// current chunk's dictionary fast-path bitmap (c.indexReaderMatches). On the eligible
// null-rejecting dict path the bitmap is built once here and reused by every page
// (indexReaderFor), so the dictionary is scanned once per chunk rather than twice.
func (c *SyncIterator) keepColumnChunk(cc *ColumnChunkHelper) bool {
	if c.filter == nil {
		return true // no predicate: keep everything, no bitmap
	}
	// Reuse path: build the per-index keep bitmap once and derive the any-match from it.
	// Eligibility already implies c.stats == nil (no InstrumentedPredicate), so there is
	// nothing to count here.
	if c.dictFastPathEligible() {
		if dict := cc.Dictionary(); dict != nil && !c.pred.KeepValue(predicateNullValue()) {
			c.indexReaderMatches = dictionaryKeepBitmap(dict, c.pred.KeepValue)
			return anyTrue(c.indexReaderMatches)
		}
	}
	// Non-reuse path (Instrumented wrapper, keep-null predicate, or non-dict column):
	// no cached bitmap; keep the short-circuiting any-match + stats.
	c.indexReaderMatches = nil
	return columnChunkMatches(c.pred, cc, c.stats)
}

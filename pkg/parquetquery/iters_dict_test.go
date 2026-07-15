package parquetquery

import (
	"context"
	"testing"

	"github.com/parquet-go/parquet-go"
	"github.com/stretchr/testify/require"
)

type dictResult struct {
	rn   RowNumber
	val  string
	null bool
}

// collectDict runs it to exhaustion, capturing each result's row number, value, and null flag.
func collectDict(t *testing.T, it Iterator) []dictResult {
	t.Helper()
	var out []dictResult
	for {
		res, err := it.Next()
		require.NoError(t, err)
		if res == nil {
			break
		}
		v := res.Entries[0].Value
		out = append(out, dictResult{rn: res.RowNumber, val: string(v.ByteArray()), null: v.IsNull()})
	}
	return out
}

// TestDictFastPathParity asserts the dictionary-index fast path returns the same rows
// (row numbers + values, in order) as an oracle. The oracle is an unfiltered enumeration
// of the column — which takes the per-row path, since no predicate means no keep bitmap —
// filtered in Go by the predicate. It also asserts the fast path is actually exercised.
func TestDictFastPathParity(t *testing.T) {
	sp := func(s string) *string { return &s }

	reqStrings := func(ss ...string) func(*parquet.Writer) {
		return func(w *parquet.Writer) {
			for _, s := range ss {
				require.NoError(t, w.Write(&testDictString{s}))
			}
		}
	}
	optStrings := func(ss ...*string) func(*parquet.Writer) {
		return func(w *parquet.Writer) {
			for _, s := range ss {
				require.NoError(t, w.Write(&testOptString{s}))
			}
		}
	}

	cases := []struct {
		name string
		pred func() Predicate
		// chunkSkipped is true when the chunk-level dictionary any-match skips the whole
		// chunk, so no page is read and the value-level fast path never engages.
		chunkSkipped bool
		write        func(*parquet.Writer)
	}{
		{
			name:  "string equal, some match",
			pred:  func() Predicate { return NewStringEqualPredicate("b") },
			write: reqStrings("a", "b", "c", "b", "a", "b"),
		},
		{
			name:         "string equal, no match (chunk skipped)",
			pred:         func() Predicate { return NewStringEqualPredicate("zzz") },
			chunkSkipped: true,
			write:        reqStrings("a", "b", "c"),
		},
		{
			name:  "string IN (map path)",
			pred:  func() Predicate { return NewStringInPredicate([]string{"a", "c", "n0", "n1", "n2", "n3", "n4", "n5"}) },
			write: reqStrings("a", "b", "c", "d", "a"),
		},
		{
			name:  "substring",
			pred:  func() Predicate { return NewSubstringPredicate("b") },
			write: reqStrings("abc", "xyz", "bbb", "qqq", "cab"),
		},
		{
			name:  "optional column with nulls, equal",
			pred:  func() Predicate { return NewStringEqualPredicate("b") },
			write: optStrings(sp("a"), nil, sp("b"), nil, sp("b"), sp("c")),
		},
		{
			name:         "optional column all null (chunk skipped)",
			pred:         func() Predicate { return NewStringEqualPredicate("b") },
			chunkSkipped: true,
			write:        optStrings(nil, nil, nil),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := buildFile(t, tc.write)

			// Oracle: enumerate every row (unfiltered => per-row path), then filter in Go.
			all := collectDict(t, NewSyncIterator(context.TODO(), r.RowGroups(), 0, SyncIteratorOptSelectAs("v")))
			pred := tc.pred()
			var want []dictResult
			for _, e := range all {
				var v parquet.Value
				if !e.null {
					v = parquet.ValueOf(e.val)
				}
				if pred.KeepValue(v) {
					want = append(want, e)
				}
			}

			// Fast path: the same predicate over the dict-encoded column.
			it := NewSyncIterator(context.TODO(), r.RowGroups(), 0,
				SyncIteratorOptPredicate(tc.pred()), SyncIteratorOptSelectAs("v"))
			got := collectDict(t, it)
			fastPages := it.indexReaderPagesUsed
			it.Close()

			if tc.chunkSkipped {
				require.Zero(t, fastPages, "chunk-skipped case reads no page")
			} else {
				require.Positive(t, fastPages, "fast path should serve a dict-encoded page")
			}
			require.Equal(t, want, got, "fast-path results must equal the oracle")
		})
	}
}

package parquetquery

import (
	"bytes"
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

// collectDict runs the iterator to exhaustion, returning the results and the number
// of pages served via the dictionary fast path.
func collectDict(t *testing.T, r *parquet.File, pred Predicate, disableFast bool) ([]dictResult, int) {
	t.Helper()
	opts := []SyncIteratorOpt{SyncIteratorOptPredicate(pred), SyncIteratorOptSelectAs("v")}
	if disableFast {
		opts = append(opts, SyncIteratorOptDisableDictFastPath())
	}
	it := NewSyncIterator(context.TODO(), r.RowGroups(), 0, opts...)
	defer it.Close()

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
	return out, it.indexReaderPagesUsed
}

// TestDictFastPathParity asserts the dictionary-index fast path returns byte-identical
// results (row numbers + values) to the per-row path, across predicate types and
// null/required columns, and that it is actually exercised.
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
			buf := new(bytes.Buffer)
			w := parquet.NewWriter(buf)
			tc.write(w)
			require.NoError(t, w.Flush())
			require.NoError(t, w.Close())
			r, err := parquet.OpenFile(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
			require.NoError(t, err)

			slow, slowPages := collectDict(t, r, tc.pred(), true)
			require.Equal(t, 0, slowPages, "per-row path must not use the fast path")

			fast, fastPages := collectDict(t, r, tc.pred(), false)
			if tc.chunkSkipped {
				require.Zero(t, fastPages, "chunk-skipped case reads no page")
			} else {
				require.Positive(t, fastPages, "fast path should serve a dict-encoded page")
			}

			require.Equal(t, slow, fast, "fast-path results must equal per-row results")
		})
	}
}

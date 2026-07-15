package parquetquery

import (
	"context"
	"fmt"
	"testing"

	"github.com/parquet-go/parquet-go"
)

// BenchmarkSyncIteratorDictPushdown compares the dictionary fast path against the per-row
// byte-compare path for an exact-match string filter, the dominant shape of live-store
// metrics queries. The per-row baseline uses a PLAIN-encoded column (no dictionary, so the
// fast path cannot engage); the pushdown case uses a dict-encoded column with the same data.
func BenchmarkSyncIteratorDictPushdown(b *testing.B) {
	type dictRow struct {
		S string `parquet:",dict"`
	}
	type plainRow struct {
		S string `parquet:",plain"`
	}

	// Low-cardinality column (like span:name / service.name): a small set of distinct
	// values repeated across many rows.
	alphabet := make([]string, 32)
	for i := range alphabet {
		alphabet[i] = fmt.Sprintf("operation-name-%02d", i)
	}
	targets := []string{alphabet[3], alphabet[17], alphabet[28]}

	ctx := context.Background()
	dictRows := make([]dictRow, 200_000)
	plainRows := make([]plainRow, 200_000)
	for i := range dictRows {
		dictRows[i] = dictRow{S: alphabet[i%len(alphabet)]}
		plainRows[i] = plainRow{S: alphabet[i%len(alphabet)]}
	}
	dictPF := createFileWith(b, ctx, dictRows)
	plainPF := createFileWith(b, ctx, plainRows)
	dictIdx, _, _ := GetColumnIndexByPath(dictPF, "S")
	plainIdx, _, _ := GetColumnIndexByPath(plainPF, "S")

	run := func(b *testing.B, pf *parquet.File, idx int) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			iter := NewSyncIterator(ctx, pf.RowGroups(), idx,
				SyncIteratorOptSelectAs("S"),
				SyncIteratorOptPredicate(NewStringInPredicate(targets)))
			var count int
			for {
				res, err := iter.Next()
				if err != nil {
					b.Fatal(err)
				}
				if res == nil {
					break
				}
				count++
			}
			iter.Close()
			if count == 0 {
				b.Fatal("expected matches")
			}
		}
	}

	b.Run("per-row", func(b *testing.B) { run(b, plainPF, plainIdx) })
	b.Run("dict-pushdown", func(b *testing.B) { run(b, dictPF, dictIdx) })
}

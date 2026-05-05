package parquetquery

import (
	"bytes"

	pq "github.com/parquet-go/parquet-go"
)

// AnyRowGroupDictionaryContainsString returns true if any of the given row groups'
// dictionary at the specified column index contains the given string value.
//
// Returns true conservatively when:
//   - the column index is out of range for any row group, or
//   - any column chunk does not have a dictionary page (e.g. unencoded column).
//
// In those cases the caller cannot prove absence, so we report presence.
func AnyRowGroupDictionaryContainsString(rgs []pq.RowGroup, columnIndex int, value string) bool {
	target := []byte(value)
	for _, rg := range rgs {
		chunks := rg.ColumnChunks()
		if columnIndex < 0 || columnIndex >= len(chunks) {
			return true
		}
		if rowGroupDictContains(chunks[columnIndex], target) {
			return true
		}
	}
	return false
}

// rowGroupDictContains is split out so its ColumnChunkHelper is closed via defer
// per row group, releasing the buffered first page and the underlying pages reader.
func rowGroupDictContains(cc pq.ColumnChunk, target []byte) bool {
	helper := &ColumnChunkHelper{ColumnChunk: cc}
	defer helper.Close()

	d := helper.Dictionary()
	if d == nil {
		// No dictionary page available; cannot prove absence.
		return true
	}
	n := d.Len()
	for i := 0; i < n; i++ {
		if bytes.Equal(d.Index(int32(i)).ByteArray(), target) {
			return true
		}
	}
	return false
}

// LoadRowGroupDictionaryStringSet reads the column-chunk dictionary at the given
// column index from each row group and returns the union of dictionary string
// values as a set. The bool return value reports whether the result is opaque —
// true when the column is missing or any row group lacks a dictionary, in which
// case the returned set is nil and the caller must treat membership as unknown
// (typically: assume present, the same conservative choice as
// AnyRowGroupDictionaryContainsString).
func LoadRowGroupDictionaryStringSet(rgs []pq.RowGroup, columnIndex int) (set map[string]struct{}, opaque bool) {
	out := make(map[string]struct{})
	for _, rg := range rgs {
		chunks := rg.ColumnChunks()
		if columnIndex < 0 || columnIndex >= len(chunks) {
			return nil, true
		}
		if !appendRowGroupDictKeys(chunks[columnIndex], out) {
			return nil, true
		}
	}
	return out, false
}

// appendRowGroupDictKeys is split out so its ColumnChunkHelper is closed via
// defer per row group. Returns false if the column chunk had no dictionary.
func appendRowGroupDictKeys(cc pq.ColumnChunk, out map[string]struct{}) bool {
	helper := &ColumnChunkHelper{ColumnChunk: cc}
	defer helper.Close()

	d := helper.Dictionary()
	if d == nil {
		return false
	}
	n := d.Len()
	for i := 0; i < n; i++ {
		out[string(d.Index(int32(i)).ByteArray())] = struct{}{}
	}
	return true
}

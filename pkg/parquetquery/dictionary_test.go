package parquetquery

import (
	"bytes"
	"testing"

	pq "github.com/parquet-go/parquet-go"
	"github.com/stretchr/testify/require"
)

type testKeyRow struct {
	Key string `parquet:",dict"`
}

func writeTestKeyRowGroup(t *testing.T, vals []string) pq.RowGroup {
	t.Helper()
	buf := &bytes.Buffer{}
	w := pq.NewGenericWriter[testKeyRow](buf)
	for _, v := range vals {
		_, err := w.Write([]testKeyRow{{Key: v}})
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	r, err := pq.OpenFile(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	require.NoError(t, err)
	return r.RowGroups()[0]
}

func TestAnyRowGroupDictionaryContainsString_PresentInOne(t *testing.T) {
	rg1 := writeTestKeyRowGroup(t, []string{"alpha", "beta"})
	rg2 := writeTestKeyRowGroup(t, []string{"gamma", "delta"})

	require.True(t, AnyRowGroupDictionaryContainsString([]pq.RowGroup{rg1, rg2}, 0, "alpha"))
	require.True(t, AnyRowGroupDictionaryContainsString([]pq.RowGroup{rg1, rg2}, 0, "delta"))
}

func TestAnyRowGroupDictionaryContainsString_AbsentInAll(t *testing.T) {
	rg1 := writeTestKeyRowGroup(t, []string{"alpha", "beta"})
	rg2 := writeTestKeyRowGroup(t, []string{"gamma", "delta"})

	require.False(t, AnyRowGroupDictionaryContainsString([]pq.RowGroup{rg1, rg2}, 0, "epsilon"))
}

func TestAnyRowGroupDictionaryContainsString_EmptyRowGroups(t *testing.T) {
	require.False(t, AnyRowGroupDictionaryContainsString(nil, 0, "anything"))
}

func TestAnyRowGroupDictionaryContainsString_OutOfRangeColumnIsConservative(t *testing.T) {
	rg := writeTestKeyRowGroup(t, []string{"alpha"})
	require.True(t, AnyRowGroupDictionaryContainsString([]pq.RowGroup{rg}, 99, "alpha"))
}

func TestAnyRowGroupDictionaryContainsString_RepeatedCallsAreSafe(t *testing.T) {
	rg := writeTestKeyRowGroup(t, []string{"alpha", "beta"})
	for i := 0; i < 100; i++ {
		require.True(t, AnyRowGroupDictionaryContainsString([]pq.RowGroup{rg}, 0, "alpha"))
		require.False(t, AnyRowGroupDictionaryContainsString([]pq.RowGroup{rg}, 0, "missing"))
	}
}

func TestLoadRowGroupDictionaryStringSet_UnionAcrossRowGroups(t *testing.T) {
	rg1 := writeTestKeyRowGroup(t, []string{"alpha", "beta"})
	rg2 := writeTestKeyRowGroup(t, []string{"gamma", "delta"})

	set, opaque := LoadRowGroupDictionaryStringSet([]pq.RowGroup{rg1, rg2}, 0)
	require.False(t, opaque)
	require.Len(t, set, 4)
	for _, k := range []string{"alpha", "beta", "gamma", "delta"} {
		_, ok := set[k]
		require.True(t, ok, "missing %s", k)
	}
}

func TestLoadRowGroupDictionaryStringSet_OutOfRangeIsOpaque(t *testing.T) {
	rg := writeTestKeyRowGroup(t, []string{"alpha"})
	set, opaque := LoadRowGroupDictionaryStringSet([]pq.RowGroup{rg}, 99)
	require.True(t, opaque)
	require.Nil(t, set)
}

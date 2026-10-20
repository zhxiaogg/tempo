package cgfile

import (
	"context"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

func testDecoder(t *testing.T) *decoder {
	z, err := zstd.NewReader(nil)
	require.NoError(t, err)
	t.Cleanup(z.Close)
	return &decoder{zstd: z}
}

// Worked examples from the spec (real bytes of the test block).

func TestDecodeDeltaRaw(t *testing.T) {
	data := []byte{0x80, 0x01, 0x04, 0x0b, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	v, err := testDecoder(t).decodeBuf(data, &Buf{Len: 10, Flags: 0x0a}, 11, PhysInt32, 0, nil)
	require.NoError(t, err)
	require.Equal(t, make([]int64, 11), v.Ints)
	require.Nil(t, v.Valid)
}

func TestDecodeAllNullDeltaLength(t *testing.T) {
	data := []byte{0xea, 0x39, 0x00, 0x80, 0x01, 0x04, 0x00, 0x00}
	v, err := testDecoder(t).decodeBuf(data, &Buf{Len: 8, Flags: 0x1b, DefLen: 3, Nulls: 3701}, 3701, PhysBytes, 0, nil)
	require.NoError(t, err)
	require.Len(t, v.Valid, 3701)
	for i := range v.N {
		require.True(t, v.IsNull(i))
		require.Nil(t, v.Bytes(i, nil))
	}
}

func TestDecodeNullableOneValue(t *testing.T) {
	data := []byte{0xfc, 0x81, 0x05, 0x00, 0x03, 0x01, 0xd4, 0x78, 0x00, 0x80, 0x01, 0x04, 0x01, 0x04, 0x7b, 0x7d}
	v, err := testDecoder(t).decodeBuf(data, &Buf{Len: 16, Flags: 0x1b, DefLen: 9, Nulls: 48815}, 48816, PhysBytes, 0, nil)
	require.NoError(t, err)
	for i := range v.N {
		if i == 41086 {
			require.Equal(t, "{}", string(v.Bytes(i, nil)))
		} else {
			require.True(t, v.IsNull(i), i)
		}
	}
}

func TestDecodeDictWidthZero(t *testing.T) {
	data := []byte{0x00, 0x9e, 0xdd, 0x05}
	v, err := testDecoder(t).decodeBuf(data, &Buf{Len: 4, Flags: 0x09}, 46927, PhysInt32, 1, []int64{7})
	require.NoError(t, err)
	for _, x := range v.Ints {
		require.Equal(t, int64(7), x)
	}
}

func TestDecodeBoolWithNulls(t *testing.T) {
	data := []byte{0x84, 0x17, 0x00, 0x03, 0x01, 0xfa, 0x05, 0x00, 0x00}
	v, err := testDecoder(t).decodeBuf(data, &Buf{Len: 9, Flags: 0x18, DefLen: 8, Nulls: 1862}, 1863, PhysBool, 0, nil)
	require.NoError(t, err)
	for i := range v.N {
		require.Equal(t, i != 1474, v.IsNull(i), i)
		require.False(t, v.Bools[i])
	}
}

func TestEmptyGroupSection(t *testing.T) {
	g, err := parseGroup([]byte{0x0c, 0x01, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, 6)
	require.NoError(t, err)
	require.Empty(t, g.Chunks)
	require.Empty(t, g.Pages)
}

func TestKmSection(t *testing.T) {
	sec := []byte{
		0x0c, 0x05, 0x08, 0x00, 0x00, 0x00, 0x09, 0x00, 0x00, 0x04, 0x08, 0x09, 0x04,
		0x01, 0x00, 0xfb, 0x90, 0x12, // HEAD
		0xd1, 0xdb, 0xee, 0x84, 0x05, 0xac, 0x8c, 0x13, // REGION
		0xcd, 0x1e, 0x99, 0x20, 0xdd, 0xcb, 0x12, 0xe9, 0x01, // BLEN
		0x00, 0x01, 0x01, 0x01, // RKEY
		0, 0, 0, 0, 0, 0, 0, 0, // RVT
		0x04, 0xc9, 0x08, 0xc9, 0x08, 0xa1, 0xd4, 0x11, 0x22, // RROWS
		0, 0, 0, 0, // RMISC
	}
	km, err := parseKm(sec)
	require.NoError(t, err)
	require.Len(t, km, 1)
	k := km[0]
	require.Equal(t, uint32(0), k.Owner0)
	require.Equal(t, uint32(297083), k.Owners)
	require.Equal(t, int64(1352379857), k.Off)
	require.Equal(t, int64(312876), k.Len)
	require.Len(t, k.Runs, 4)
	wantRows := []uint32{1097, 1097, 289313, 34}
	wantLen := []uint32{3917, 4121, 304605, 233}
	off := k.Off
	for i, r := range k.Runs {
		require.Equal(t, uint32(i), r.Key)
		require.Equal(t, wantRows[i], r.Rows)
		require.Equal(t, wantLen[i], r.Len)
		require.Equal(t, off, r.Off)
		off += int64(r.Len)
	}
	require.Equal(t, Region{Off: k.Runs[2].Off, End: k.Runs[2].Off + 304605}, k.KeyRegion(2))
}

func TestRunHeader(t *testing.T) {
	b := make([]byte, 3917)
	copy(b, []byte{0x0c, 0x02, 0xc4, 0x0b, 0x01, 0x1c, 0x02, 0x03, 0x01, 0x79, 0x02, 0xe7, 0x11})
	rb, err := ParseRunHeader(b, 1000)
	require.NoError(t, err)
	require.Equal(t, int64(1013), rb.Owner.Off)
	require.Equal(t, uint32(1476), rb.Owner.Len)
	require.Equal(t, DictDesc{Off: 1013 + 1476, Len: 28, Entries: 2}, rb.Dict)
	require.NotNil(t, rb.Val)
	require.Equal(t, uint8(EncRLEDict), rb.Val.Encoding())
	require.Len(t, rb.Links, 1)
	require.Equal(t, uint32(2279), rb.Links[0].Len)
}

type memReader struct {
	b    []byte
	reqs [][2]int64
}

func (m *memReader) ReadRange(_ context.Context, off int64, buf []byte) error {
	m.reqs = append(m.reqs, [2]int64{off, int64(len(buf))})
	copy(buf, m.b[off:])
	return nil
}

func TestFetcherWindows(t *testing.T) {
	data := make([]byte, 100)
	for i := range data {
		data[i] = byte(i)
	}
	m := &memReader{b: data}
	f := NewFetcher(m, 16)
	reg := Region{Off: 10, End: 90}
	ranges := []Range{{Off: 12, Len: 3, Region: reg}, {Off: 40, Len: 20, Region: reg}, {Off: 13, Len: 1, Region: reg}}
	require.NoError(t, f.Fetch(context.Background(), ranges))
	// windows [10,26), [26,42), [42,58), [58,74)
	require.Equal(t, [][2]int64{{10, 16}, {26, 16}, {42, 16}, {58, 16}}, m.reqs)
	require.Equal(t, Stats{Requests: 4, Bytes: 64, Waves: 1}, f.Stats)
	b, err := f.Bytes(ranges[1])
	require.NoError(t, err)
	require.Equal(t, data[40:60], b)
	// held windows are not asked again; the last window is clipped to the region
	require.NoError(t, f.Fetch(context.Background(), []Range{{Off: 41, Len: 49, Region: reg}}))
	require.Len(t, m.reqs, 5)
	require.Equal(t, [2]int64{74, 16}, m.reqs[4])
	require.Equal(t, int64(2), f.Stats.Waves)
	b, err = f.Bytes(Range{Off: 41, Len: 49, Region: reg})
	require.NoError(t, err)
	require.Equal(t, data[41:90], b)
}

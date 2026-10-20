package cgfile

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/klauspost/compress/zstd"
	"github.com/parquet-go/parquet-go/encoding/delta"
	"github.com/parquet-go/parquet-go/encoding/rle"
)

// NullCode is the dictionary code of a null row.
const NullCode = math.MaxUint32

// Values is one decoded buffer (a page of a column, or a run buffer), one entry per row.
// Exactly one value representation is set, by physical type:
//
//   - INT32 / INT64: Ints (INT32 sign-extended; unsigned model fields are the low 32 bits).
//   - DOUBLE: Floats. BOOLEAN: Bools.
//   - BYTE_ARRAY: Codes into a dictionary (Dict = true; NullCode for nulls) or plain values
//     Data[Offs[i]:Offs[i+1]] (Dict = false; nulls are empty).
//
// Null rows hold zero values; Valid is nil when the buffer has no nulls.
type Values struct {
	First uint32 // file row of entry 0 (group pages; 0 for run buffers)
	N     int
	Phys  Phys
	Valid []bool

	Ints   []int64
	Floats []float64
	Bools  []bool

	Dict  bool
	Codes []uint32
	Data  []byte
	Offs  []uint32
}

// IsNull reports whether entry i is null.
func (v *Values) IsNull(i int) bool { return v.Valid != nil && !v.Valid[i] }

// Raw returns plain byte-array entry i (Dict must be false).
func (v *Values) Raw(i int) []byte { return v.Data[v.Offs[i]:v.Offs[i+1]] }

// Bytes returns byte-array entry i, resolving codes through d (nil for null).
func (v *Values) Bytes(i int, d *Dict) []byte {
	if v.IsNull(i) {
		return nil
	}
	if v.Dict {
		return d.Get(v.Codes[i])
	}
	return v.Raw(i)
}

// decoder holds per-request scratch buffers.
type decoder struct {
	zstd *zstd.Decoder
	vbuf []byte
	lv   []uint8
	i32  []int32
	i64  []int64
	idx  []int32
}

var (
	levels1   = rle.Encoding{BitWidth: 1}
	dictIdx   rle.DictionaryEncoding
	deltaInts delta.BinaryPackedEncoding
	deltaLens delta.LengthByteArrayEncoding
)

// unzstd returns the value section uncompressed (in scratch, valid until the next call).
func (d *decoder) unzstd(b []byte) ([]byte, error) {
	out, err := d.zstd.DecodeAll(b, d.vbuf[:0])
	if err != nil {
		return nil, fmt.Errorf("cgfile: zstd: %w", err)
	}
	d.vbuf = out
	return out, nil
}

// dictPage decompresses a dictionary page into a new buffer (it outlives the scratch).
func (d *decoder) dictPage(b []byte) ([]byte, error) {
	out, err := d.zstd.DecodeAll(b, nil)
	if err != nil {
		return nil, fmt.Errorf("cgfile: dictionary zstd: %w", err)
	}
	return out, nil
}

// decodeBuf decodes one buffer of rows entries. dictLen bounds dictionary codes; dictInts
// materializes an INT32 dictionary.
func (d *decoder) decodeBuf(data []byte, b *Buf, rows int, phys Phys, dictLen int, dictInts []int64) (*Values, error) {
	if len(data) != int(b.Len) {
		return nil, fmt.Errorf("cgfile: buffer has %d bytes, want %d", len(data), b.Len)
	}
	v := &Values{N: rows, Phys: phys}
	nulls := 0
	if b.HasNulls() {
		if int(b.DefLen) > len(data) {
			return nil, errShort
		}
		lv, err := levels1.DecodeLevels(d.lv[:0], data[:b.DefLen])
		if err != nil {
			return nil, fmt.Errorf("cgfile: def levels: %w", err)
		}
		d.lv = lv
		if len(lv) < rows {
			return nil, fmt.Errorf("cgfile: %d def levels, want %d", len(lv), rows)
		}
		v.Valid = make([]bool, rows)
		for i, l := range lv[:rows] {
			if l != 0 {
				v.Valid[i] = true
			} else {
				nulls++
			}
		}
		if nulls != int(b.Nulls) {
			return nil, fmt.Errorf("cgfile: def levels have %d nulls, footer says %d", nulls, b.Nulls)
		}
	}
	nn := rows - nulls
	vs := data[b.DefLen:]
	if nn > 0 && !b.Raw() {
		var err error
		if vs, err = d.unzstd(vs); err != nil {
			return nil, err
		}
	}
	if nn == 0 {
		vs = nil
	}
	var err error
	switch phys {
	case PhysBytes:
		err = d.decodeBytes(v, vs, b.Encoding(), nn, dictLen)
	case PhysBool:
		err = d.decodeBools(v, vs, b.Encoding(), nn)
	case PhysDouble:
		err = d.decodeDoubles(v, vs, b.Encoding(), nn)
	default:
		err = d.decodeInts(v, vs, b.Encoding(), nn, phys, dictInts)
	}
	if err != nil {
		return nil, err
	}
	return v, nil
}

// indices decodes n RLE_DICTIONARY indices (bit-width byte + hybrid) into d.idx.
func (d *decoder) indices(vs []byte, n, dictLen int) ([]int32, error) {
	if n == 0 {
		return d.idx[:0], nil
	}
	if len(vs) == 0 {
		return nil, errShort
	}
	var idx []int32
	if vs[0] == 0 {
		// width 0: every index is 0 (rle has no bytes per value)
		idx = d.idx[:0]
		for range n {
			idx = append(idx, 0)
		}
	} else {
		var err error
		if idx, err = dictIdx.DecodeInt32(d.idx[:0], vs); err != nil {
			return nil, fmt.Errorf("cgfile: dictionary indices: %w", err)
		}
	}
	d.idx = idx
	if len(idx) < n {
		return nil, fmt.Errorf("cgfile: %d dictionary indices, want %d", len(idx), n)
	}
	idx = idx[:n]
	for _, i := range idx {
		if i < 0 || int(i) >= dictLen {
			return nil, fmt.Errorf("cgfile: dictionary index %d out of range (%d entries)", i, dictLen)
		}
	}
	return idx, nil
}

func (d *decoder) decodeBytes(v *Values, vs []byte, enc uint8, nn, dictLen int) error {
	switch enc {
	case EncRLEDict:
		idx, err := d.indices(vs, nn, dictLen)
		if err != nil {
			return err
		}
		v.Dict = true
		v.Codes = make([]uint32, v.N)
		if v.Valid == nil {
			for i, x := range idx {
				v.Codes[i] = uint32(x)
			}
			return nil
		}
		j := 0
		for i, ok := range v.Valid {
			if ok {
				v.Codes[i] = uint32(idx[j])
				j++
			} else {
				v.Codes[i] = NullCode
			}
		}
		return nil
	case EncDeltaLength:
		var data []byte
		var offs []uint32
		if nn > 0 {
			var err error
			// offsets has nn+1 entries
			data, offs, err = deltaLens.DecodeByteArray(nil, vs, nil)
			if err != nil {
				return fmt.Errorf("cgfile: delta length byte array: %w", err)
			}
			if len(offs) != nn+1 {
				return fmt.Errorf("cgfile: %d byte-array values, want %d", len(offs)-1, nn)
			}
		}
		v.Data = data
		if v.Valid == nil && nn > 0 {
			v.Offs = offs
			return nil
		}
		v.Offs = make([]uint32, v.N+1)
		j := 0
		for i, ok := range v.Valid {
			if ok {
				j++
				v.Offs[i+1] = offs[j]
			} else {
				v.Offs[i+1] = v.Offs[i]
			}
		}
		return nil
	}
	return fmt.Errorf("cgfile: byte-array buffer with encoding %d", enc)
}

func (d *decoder) decodeBools(v *Values, vs []byte, enc uint8, nn int) error {
	if enc != EncPlain {
		return fmt.Errorf("cgfile: boolean buffer with encoding %d", enc)
	}
	if len(vs) < (nn+7)/8 {
		return errShort
	}
	v.Bools = make([]bool, v.N)
	j := 0
	for i := range v.Bools {
		if v.Valid != nil && !v.Valid[i] {
			continue
		}
		v.Bools[i] = vs[j>>3]>>(j&7)&1 != 0
		j++
	}
	return nil
}

func (d *decoder) decodeDoubles(v *Values, vs []byte, enc uint8, nn int) error {
	if enc != EncPlain {
		return fmt.Errorf("cgfile: double buffer with encoding %d", enc)
	}
	if len(vs) < 8*nn {
		return errShort
	}
	v.Floats = make([]float64, v.N)
	j := 0
	for i := range v.Floats {
		if v.Valid != nil && !v.Valid[i] {
			continue
		}
		v.Floats[i] = math.Float64frombits(binary.LittleEndian.Uint64(vs[8*j:]))
		j++
	}
	return nil
}

func (d *decoder) decodeInts(v *Values, vs []byte, enc uint8, nn int, phys Phys, dictInts []int64) error {
	v.Ints = make([]int64, v.N)
	if nn == 0 {
		return nil
	}
	// dense non-null values
	var src func(j int) int64
	switch enc {
	case EncPlain:
		w := 8
		if phys == PhysInt32 {
			w = 4
		}
		if len(vs) < w*nn {
			return errShort
		}
		if w == 4 {
			src = func(j int) int64 { return int64(int32(binary.LittleEndian.Uint32(vs[4*j:]))) }
		} else {
			src = func(j int) int64 { return int64(binary.LittleEndian.Uint64(vs[8*j:])) }
		}
	case EncDeltaBinary:
		if phys == PhysInt32 {
			out, err := deltaInts.DecodeInt32(d.i32[:0], vs)
			if err != nil {
				return fmt.Errorf("cgfile: delta binary packed: %w", err)
			}
			d.i32 = out
			if len(out) != nn {
				return fmt.Errorf("cgfile: %d delta values, want %d", len(out), nn)
			}
			src = func(j int) int64 { return int64(out[j]) }
		} else {
			out, err := deltaInts.DecodeInt64(d.i64[:0], vs)
			if err != nil {
				return fmt.Errorf("cgfile: delta binary packed: %w", err)
			}
			d.i64 = out
			if len(out) != nn {
				return fmt.Errorf("cgfile: %d delta values, want %d", len(out), nn)
			}
			src = func(j int) int64 { return out[j] }
		}
	case EncRLEDict:
		idx, err := d.indices(vs, nn, len(dictInts))
		if err != nil {
			return err
		}
		src = func(j int) int64 { return dictInts[idx[j]] }
	default:
		return fmt.Errorf("cgfile: int buffer with encoding %d", enc)
	}
	if v.Valid == nil {
		for i := range v.Ints {
			v.Ints[i] = src(i)
		}
		return nil
	}
	j := 0
	for i, ok := range v.Valid {
		if ok {
			v.Ints[i] = src(j)
			j++
		}
	}
	return nil
}

// plainByteArrays splits a PLAIN BYTE_ARRAY dictionary page (u32 LE length + bytes each).
func plainByteArrays(page []byte, want uint32) ([][]byte, error) {
	out := make([][]byte, 0, want)
	for p := 0; p < len(page); {
		if p+4 > len(page) {
			return nil, errShort
		}
		n := int(binary.LittleEndian.Uint32(page[p:]))
		p += 4
		if n > len(page)-p {
			return nil, errShort
		}
		out = append(out, page[p:p+n:p+n])
		p += n
	}
	if uint32(len(out)) != want {
		return nil, fmt.Errorf("cgfile: dictionary has %d entries, footer says %d", len(out), want)
	}
	return out, nil
}

// plainInt32s materializes a PLAIN INT32 dictionary page.
func plainInt32s(page []byte) []int64 {
	out := make([]int64, len(page)/4)
	for i := range out {
		out[i] = int64(int32(binary.LittleEndian.Uint32(page[4*i:])))
	}
	return out
}

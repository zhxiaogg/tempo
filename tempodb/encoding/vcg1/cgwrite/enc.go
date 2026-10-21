package cgwrite

import (
	"encoding/binary"
	"math"

	"github.com/klauspost/compress/zstd"
	"github.com/parquet-go/parquet-go/encoding/delta"
	"github.com/parquet-go/parquet-go/encoding/rle"

	cg "github.com/grafana/tempo/v3/tempodb/encoding/vcg1/cgfile"
)

func putUvarint(b []byte, v uint64) []byte { return binary.AppendUvarint(b, v) }

func putVarint(b []byte, v int64) []byte { return binary.AppendVarint(b, v) }

func putBytes(b, v []byte) []byte { return append(binary.AppendUvarint(b, uint64(len(v))), v...) }

// zone is a buffer's min/max as the footer stores it.
type zone struct {
	kind     cg.ZoneKind
	min, max int64
	bmin     []byte
	bmax     []byte
}

// buf is one written buffer (def levels + value section) as the footer or a run header
// describes it. Offsets are implied by running sums, so they are not kept.
type buf struct {
	len    uint32
	defLen uint32
	nulls  uint32
	enc    uint8
	raw    bool
	zone   zone
}

func (b *buf) flags() uint8 {
	f := b.enc
	if b.raw {
		f |= 0x08
	}
	if b.nulls > 0 {
		f |= 0x10
	}
	return f | uint8(b.zone.kind)<<5
}

// coder holds the encoders and scratch buffers. It is not safe for concurrent use.
type coder struct {
	zw      *zstd.Encoder
	scratch []byte
	z       []byte
	lv      []uint8
	lvb     []byte
	i32     []int32
	data    []byte
}

var (
	deltaInts delta.BinaryPackedEncoding
	deltaLens delta.LengthByteArrayEncoding
	levels1   = rle.Encoding{BitWidth: 1}
	dictIdx   rle.DictionaryEncoding
)

func newCoder(level zstd.EncoderLevel) (*coder, error) {
	zw, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(level),
		zstd.WithEncoderCRC(false),
		zstd.WithEncoderConcurrency(1),
		zstd.WithZeroFrames(true),
		zstd.WithLowerEncoderMem(true),
	)
	if err != nil {
		return nil, err
	}
	return &coder{zw: zw}, nil
}

func (c *coder) close() { _ = c.zw.Close() }

// compress returns zstd(b) in a scratch buffer valid until the next call.
func (c *coder) compress(b []byte) []byte {
	c.z = c.zw.EncodeAll(b, c.z[:0])
	return c.z
}

// finish appends one buffer to out: def levels (iff valid has a null), then the value
// section, zstd-compressed unless that is not smaller.
func (c *coder) finish(out []byte, valid []bool, values []byte, enc uint8, z zone) ([]byte, buf) {
	start := len(out)
	b := buf{enc: enc, zone: z}
	for _, ok := range valid {
		if !ok {
			b.nulls++
		}
	}
	if b.nulls > 0 {
		c.lv = c.lv[:0]
		for _, ok := range valid {
			if ok {
				c.lv = append(c.lv, 1)
			} else {
				c.lv = append(c.lv, 0)
			}
		}
		d, _ := levels1.EncodeLevels(c.lvb[:0], c.lv)
		c.lvb = d
		b.defLen = uint32(len(d))
		out = append(out, d...)
	}
	zb := c.compress(values)
	if len(zb) >= len(values) {
		b.raw = true
		out = append(out, values...)
	} else {
		out = append(out, zb...)
	}
	b.len = uint32(len(out) - start)
	return out, b
}

func (c *coder) deltaI32(v []int32) []byte {
	c.scratch, _ = deltaInts.EncodeInt32(c.scratch[:0], v)
	return c.scratch
}

func (c *coder) deltaI64(v []int64) []byte {
	c.scratch, _ = deltaInts.EncodeInt64(c.scratch[:0], v)
	return c.scratch
}

// deltaLen encodes byte arrays data[offs[i]:offs[i+1]] as DELTA_LENGTH_BYTE_ARRAY.
func (c *coder) deltaLen(data []byte, offs []uint32) []byte {
	if len(offs) == 0 {
		offs = []uint32{0}
	}
	c.scratch, _ = deltaLens.EncodeByteArray(c.scratch[:0], data, offs)
	return c.scratch
}

// rleDict encodes dictionary indices as RLE_DICTIONARY (bit-width byte + hybrid).
func (c *coder) rleDict(idx []int32) []byte {
	c.scratch, _ = dictIdx.EncodeInt32(c.scratch[:0], idx)
	return c.scratch
}

func plainI32(dst []byte, v []int32) []byte {
	for _, x := range v {
		dst = binary.LittleEndian.AppendUint32(dst, uint32(x))
	}
	return dst
}

func plainI64(dst []byte, v []int64) []byte {
	for _, x := range v {
		dst = binary.LittleEndian.AppendUint64(dst, uint64(x))
	}
	return dst
}

func plainF64(dst []byte, v []float64) []byte {
	for _, x := range v {
		dst = binary.LittleEndian.AppendUint64(dst, math.Float64bits(x))
	}
	return dst
}

func plainBool(dst []byte, v []bool) []byte {
	n := len(dst)
	dst = append(dst, make([]byte, (len(v)+7)/8)...)
	for i, x := range v {
		if x {
			dst[n+i>>3] |= 1 << (i & 7)
		}
	}
	return dst
}

func intZone(v []int64) zone {
	if len(v) == 0 {
		return zone{}
	}
	z := zone{kind: cg.ZoneInt, min: v[0], max: v[0]}
	for _, x := range v[1:] {
		z.min, z.max = min(z.min, x), max(z.max, x)
	}
	return z
}

func intZone32(v []int32) zone {
	if len(v) == 0 {
		return zone{}
	}
	z := zone{kind: cg.ZoneInt, min: int64(v[0]), max: int64(v[0])}
	for _, x := range v[1:] {
		z.min, z.max = min(z.min, int64(x)), max(z.max, int64(x))
	}
	return z
}

// strDict is a byte-array dictionary in first-occurrence order (Parquet's dictionary encoder:
// size = PLAIN dictionary page bytes, 4 + len per entry).
type strDict struct {
	ids  map[string]int32
	data []byte
	ends []uint32
	size int
}

func newStrDict() *strDict { return &strDict{ids: map[string]int32{}} }

func (d *strDict) id(v []byte) int32 {
	if i, ok := d.ids[string(v)]; ok {
		return i
	}
	i := int32(len(d.ends))
	d.data = append(d.data, v...)
	d.ends = append(d.ends, uint32(len(d.data)))
	d.ids[string(d.data[len(d.data)-len(v):])] = i
	d.size += 4 + len(v)
	return i
}

func (d *strDict) n() int { return len(d.ends) }

// page is the PLAIN BYTE_ARRAY dictionary page (u32 LE length + bytes per entry).
func (d *strDict) page(dst []byte) []byte {
	var p uint32
	for _, e := range d.ends {
		dst = binary.LittleEndian.AppendUint32(dst, e-p)
		dst = append(dst, d.data[p:e]...)
		p = e
	}
	return dst
}

func (d *strDict) memBytes() int { return len(d.data) + 4*len(d.ends) + 48*len(d.ids) }

// i32Dict is an INT32 dictionary in first-occurrence order.
type i32Dict struct {
	ids  map[int32]int32
	vals []int32
}

func newI32Dict() *i32Dict { return &i32Dict{ids: map[int32]int32{}} }

func (d *i32Dict) id(v int32) int32 {
	if i, ok := d.ids[v]; ok {
		return i
	}
	i := int32(len(d.vals))
	d.ids[v] = i
	d.vals = append(d.vals, v)
	return i
}

package cgfile

import (
	"encoding/binary"
	"errors"
)

var errShort = errors.New("cgfile: truncated input")

// cursor reads footer fields with a sticky error: after the first failure every read returns
// zero and err is kept.
type cursor struct {
	b   []byte
	p   int
	err error
}

func (c *cursor) fail(err error) {
	if c.err == nil {
		c.err = err
	}
	c.p = len(c.b)
}

func (c *cursor) uvarint() uint64 {
	if c.err != nil {
		return 0
	}
	v, n := binary.Uvarint(c.b[c.p:])
	if n <= 0 {
		c.fail(errShort)
		return 0
	}
	c.p += n
	return v
}

func (c *cursor) u32() uint32 {
	v := c.uvarint()
	if v > 1<<32-1 {
		c.fail(errors.New("cgfile: varint overflows u32"))
		return 0
	}
	return uint32(v)
}

func (c *cursor) svarint() int64 {
	u := c.uvarint()
	return int64(u>>1) ^ -int64(u&1)
}

func (c *cursor) u8() byte {
	if c.err != nil {
		return 0
	}
	if c.p >= len(c.b) {
		c.fail(errShort)
		return 0
	}
	v := c.b[c.p]
	c.p++
	return v
}

// bytes returns a length-prefixed byte string (a sub-slice of the input).
func (c *cursor) bytes() []byte {
	n := c.uvarint()
	if c.err != nil {
		return nil
	}
	if n > uint64(len(c.b)-c.p) {
		c.fail(errShort)
		return nil
	}
	v := c.b[c.p : c.p+int(n) : c.p+int(n)]
	c.p += int(n)
	return v
}

func (c *cursor) done() bool { return c.p == len(c.b) }

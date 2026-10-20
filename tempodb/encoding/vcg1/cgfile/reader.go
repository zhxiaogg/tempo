package cgfile

import (
	"context"
	"fmt"

	"github.com/klauspost/compress/zstd"
)

// Reader is the block-level state of one block.cg object: immutable after Open and safe for
// concurrent use. All per-request state lives in a Request.
type Reader struct {
	rr      RangeReader
	size    int64
	trailer Trailer
	window  int64
	zstd    *zstd.Decoder
	shared  bool // zstd is owned by the caller
	known   bool // footer length known up front: each request reads footer + trailer at once
}

// Option configures a Reader.
type Option func(*Reader)

// WithWindow sets the read window size (default DefaultWindow).
func WithWindow(n int64) Option { return func(r *Reader) { r.window = n } }

// WithDecoder makes the Reader use a caller-owned zstd decoder (not closed by Close).
func WithDecoder(d *zstd.Decoder) Option {
	return func(r *Reader) {
		if r.zstd != nil && !r.shared {
			r.zstd.Close()
		}
		r.zstd, r.shared = d, true
	}
}

// OpenKnown is Open for a caller that knows the object size and the compressed footer length
// (Tempo's block meta carries both): no I/O here; every request reads footer and trailer as ONE
// request of exactly their bytes and validates the trailer.
func OpenKnown(rr RangeReader, size int64, footerLen uint32, opts ...Option) (*Reader, error) {
	if size < int64(len(Magic))+TrailerLen || int64(footerLen) > size-TrailerLen-int64(len(Magic)) {
		return nil, fmt.Errorf("cgfile: footer of %d bytes does not fit in %d", footerLen, size)
	}
	r := &Reader{rr: rr, size: size, trailer: Trailer{FooterLen: footerLen, Variant: footerVariant, Version: fileVersion}, window: DefaultWindow, known: true}
	for _, o := range opts {
		o(r)
	}
	if r.zstd == nil {
		dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
		if err != nil {
			return nil, err
		}
		r.zstd = dec
	}
	return r, nil
}

// Open reads and validates the trailer (the last TrailerLen bytes) of an object of size bytes.
func Open(ctx context.Context, rr RangeReader, size int64, opts ...Option) (*Reader, error) {
	if size < int64(len(Magic))+TrailerLen {
		return nil, fmt.Errorf("cgfile: object of %d bytes is too small", size)
	}
	buf := make([]byte, TrailerLen)
	if err := rr.ReadRange(ctx, size-TrailerLen, buf); err != nil {
		return nil, fmt.Errorf("cgfile: read trailer: %w", err)
	}
	t, err := ParseTrailer(buf)
	if err != nil {
		return nil, err
	}
	if int64(t.FooterLen) > size-TrailerLen-int64(len(Magic)) {
		return nil, fmt.Errorf("cgfile: footer of %d bytes does not fit in %d", t.FooterLen, size)
	}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
	if err != nil {
		return nil, err
	}
	r := &Reader{rr: rr, size: size, trailer: t, window: DefaultWindow, zstd: dec}
	for _, o := range opts {
		o(r)
	}
	return r, nil
}

// Close releases the zstd decoder (unless it is shared).
func (r *Reader) Close() {
	if !r.shared {
		r.zstd.Close()
	}
}

// Size is the object size.
func (r *Reader) Size() int64 { return r.size }

// FooterLen is the compressed footer size from the trailer.
func (r *Reader) FooterLen() int { return int(r.trailer.FooterLen) }

// NewRequest starts a request: it reads the footer (one request of exactly its bytes) and
// decodes its top level. Sections are decoded on demand and cached in the request.
func (r *Reader) NewRequest(ctx context.Context) (*Request, error) {
	q := r.newRequest()
	n := int(r.trailer.FooterLen)
	if r.known {
		b, err := q.fetch.ReadExact(ctx, r.size-TrailerLen-int64(n), n+TrailerLen)
		if err != nil {
			return nil, err
		}
		t, err := ParseTrailer(b[n:])
		if err != nil {
			return nil, err
		}
		if t.FooterLen != r.trailer.FooterLen {
			return nil, fmt.Errorf("cgfile: footer is %d bytes, expected %d", t.FooterLen, r.trailer.FooterLen)
		}
		if q.F, err = DecodeFooter(r.zstd, b[:n]); err != nil {
			return nil, err
		}
		return q, nil
	}
	b, err := q.fetch.ReadExact(ctx, r.size-TrailerLen-int64(n), n)
	if err != nil {
		return nil, err
	}
	if q.F, err = DecodeFooter(r.zstd, b); err != nil {
		return nil, err
	}
	return q, nil
}

// NewRequestWithFooter starts a request on an already decoded footer (no footer read).
func (r *Reader) NewRequestWithFooter(f *Footer) *Request {
	q := r.newRequest()
	q.F = f
	return q
}

func (r *Reader) newRequest() *Request {
	return &Request{
		rd:    r,
		fetch: NewFetcher(r.rr, r.window),
		dec:   decoder{zstd: r.zstd},
		cols:  make(map[colKey]*Column),
		runs:  make(map[RunRef]*RunData),
	}
}

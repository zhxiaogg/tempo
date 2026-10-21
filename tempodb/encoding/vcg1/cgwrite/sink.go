package cgwrite

import "io"

// sink is the output: writes are buffered and passed on in parts of at least part bytes (the
// last part may be smaller), so an object-store append sees few, large parts.
type sink struct {
	w    io.Writer
	buf  []byte
	part int
	pos  uint64 // bytes written so far (the offset of the next write)
}

func (s *sink) write(b []byte) (uint64, error) {
	off := s.pos
	s.pos += uint64(len(b))
	s.buf = append(s.buf, b...)
	if len(s.buf) >= s.part {
		return off, s.flush()
	}
	return off, nil
}

func (s *sink) flush() error {
	if len(s.buf) == 0 {
		return nil
	}
	_, err := s.w.Write(s.buf)
	s.buf = s.buf[:0]
	return err
}

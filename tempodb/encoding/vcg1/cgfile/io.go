package cgfile

import (
	"context"
	"fmt"
	"sort"
)

// RangeReader reads len(buf) bytes of the block object at off.
type RangeReader interface {
	ReadRange(ctx context.Context, off int64, buf []byte) error
}

// DefaultWindow is Tempo's read buffer size: parquet-go reads a column chunk through a 1 MiB
// buffered reader, so backend requests are (at most) 1 MiB windows of the chunk.
const DefaultWindow = 1 << 20

// Region is a contiguous byte region a window may span: a run of back-to-back chunks of a
// group, or the runs of one key in a key-major flush. Windows never cross its end.
type Region struct {
	Off, End int64
}

// Range is a byte range a caller needs, tagged with the region it lies in.
type Range struct {
	Off    int64
	Len    int64
	Region Region
}

// Stats counts the backend requests of one request.
type Stats struct {
	Requests int64
	Bytes    int64
	Waves    int64
}

// Add accumulates o into s.
func (s *Stats) Add(o Stats) {
	s.Requests += o.Requests
	s.Bytes += o.Bytes
	s.Waves += o.Waves
}

// Fetcher is a per-request read cache. A range is read through windows of up to the window
// size starting at its first byte not yet held (capped at its region end and at the next held
// window); each window is fetched once and kept for the rest of the request. Not safe for
// concurrent use.
type Fetcher struct {
	r       RangeReader
	window  int64
	windows []held // sorted by offset, disjoint
	Stats   Stats

	todo []window
}

type window struct{ off, end int64 }

type held struct {
	off int64
	b   []byte
}

func (h held) end() int64 { return h.off + int64(len(h.b)) }

// NewFetcher returns a fetcher reading windows of the given size (DefaultWindow if <= 0).
func NewFetcher(r RangeReader, windowSize int64) *Fetcher {
	if windowSize <= 0 {
		windowSize = DefaultWindow
	}
	return &Fetcher{r: r, window: windowSize}
}

func checkRange(r Range) error {
	if r.Off < r.Region.Off || r.Off+r.Len > r.Region.End {
		return fmt.Errorf("cgfile: range [%d,+%d) outside its region [%d,%d)", r.Off, r.Len, r.Region.Off, r.Region.End)
	}
	return nil
}

// at returns the index of the first held window ending after off.
func (f *Fetcher) at(off int64) int {
	return sort.Search(len(f.windows), func(i int) bool { return f.windows[i].end() > off })
}

// Fetch reads, as one wave, the windows covering the ranges that the request does not hold
// yet. Windows are issued sequentially in offset order (one request in flight).
func (f *Fetcher) Fetch(ctx context.Context, ranges []Range) error {
	rs := make([]Range, 0, len(ranges))
	for _, r := range ranges {
		if r.Len <= 0 {
			continue
		}
		if err := checkRange(r); err != nil {
			return err
		}
		rs = append(rs, r)
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].Off < rs[j].Off })
	f.todo = f.todo[:0]
	for _, r := range rs {
		for p, end := r.Off, r.Off+r.Len; p < end; {
			if n := len(f.todo); n > 0 && p < f.todo[n-1].end {
				p = f.todo[n-1].end // planned windows are ascending: only the last can cover p
				continue
			}
			i := f.at(p)
			if i < len(f.windows) && f.windows[i].off <= p {
				p = f.windows[i].end()
				continue
			}
			e := min(p+f.window, r.Region.End)
			if i < len(f.windows) {
				e = min(e, f.windows[i].off)
			}
			f.todo = append(f.todo, window{p, e})
			p = e
		}
	}
	if len(f.todo) == 0 {
		return nil
	}
	f.Stats.Waves++
	for _, w := range f.todo {
		buf := make([]byte, w.end-w.off)
		if err := f.r.ReadRange(ctx, w.off, buf); err != nil {
			return fmt.Errorf("cgfile: read [%d,%d): %w", w.off, w.end, err)
		}
		f.Stats.Requests++
		f.Stats.Bytes += int64(len(buf))
		f.windows = append(f.windows, held{w.off, buf})
	}
	sort.Slice(f.windows, func(i, j int) bool { return f.windows[i].off < f.windows[j].off })
	return nil
}

// ReadExact reads exactly [off, off+n) as its own request and wave, not cached.
func (f *Fetcher) ReadExact(ctx context.Context, off int64, n int) ([]byte, error) {
	buf := make([]byte, n)
	if err := f.r.ReadRange(ctx, off, buf); err != nil {
		return nil, fmt.Errorf("cgfile: read [%d,%d): %w", off, off+int64(n), err)
	}
	f.Stats.Waves++
	f.Stats.Requests++
	f.Stats.Bytes += int64(n)
	return buf, nil
}

// Bytes returns the bytes of a range already fetched. The result aliases the held window
// unless the range crosses a window boundary (then it is a copy). Callers must not modify it.
func (f *Fetcher) Bytes(r Range) ([]byte, error) {
	if r.Len <= 0 {
		return nil, nil
	}
	if err := checkRange(r); err != nil {
		return nil, err
	}
	end := r.Off + r.Len
	i := f.at(r.Off)
	if i == len(f.windows) || f.windows[i].off > r.Off {
		return nil, fmt.Errorf("cgfile: [%d,%d) not fetched", r.Off, end)
	}
	w := f.windows[i]
	if end <= w.end() {
		s := r.Off - w.off
		return w.b[s : s+r.Len : s+r.Len], nil
	}
	out := make([]byte, 0, r.Len)
	out = append(out, w.b[r.Off-w.off:]...)
	for p := w.end(); p < end; {
		if i++; i == len(f.windows) || f.windows[i].off != p {
			return nil, fmt.Errorf("cgfile: [%d,%d) not fetched", p, end)
		}
		w = f.windows[i]
		out = append(out, w.b[:min(int64(len(w.b)), end-p)]...)
		p = w.end()
	}
	return out, nil
}

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

// Region is a contiguous byte region read as windows aligned to its start: a group chunk, or
// the runs of one key in a key-major flush.
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

// Fetcher is a per-request read cache. A range is read as the windows of its region that
// cover it; each window is fetched once and kept for the rest of the request. Not safe for
// concurrent use.
type Fetcher struct {
	r       RangeReader
	window  int64
	windows map[int64][]byte // window start -> bytes (windows of disjoint regions never collide)
	Stats   Stats

	todo []window
}

type window struct{ off, end int64 }

// NewFetcher returns a fetcher reading windows of the given size (DefaultWindow if <= 0).
func NewFetcher(r RangeReader, windowSize int64) *Fetcher {
	if windowSize <= 0 {
		windowSize = DefaultWindow
	}
	return &Fetcher{r: r, window: windowSize, windows: make(map[int64][]byte)}
}

func (f *Fetcher) span(r Range) (k0, k1 int64, err error) {
	if r.Len <= 0 {
		return 0, -1, nil
	}
	if r.Off < r.Region.Off || r.Off+r.Len > r.Region.End {
		return 0, 0, fmt.Errorf("cgfile: range [%d,+%d) outside its region [%d,%d)", r.Off, r.Len, r.Region.Off, r.Region.End)
	}
	return (r.Off - r.Region.Off) / f.window, (r.Off + r.Len - 1 - r.Region.Off) / f.window, nil
}

// Fetch reads, as one wave, every window covering the ranges that the request does not hold
// yet. Windows are issued sequentially in offset order (one request in flight).
func (f *Fetcher) Fetch(ctx context.Context, ranges []Range) error {
	f.todo = f.todo[:0]
	for _, r := range ranges {
		k0, k1, err := f.span(r)
		if err != nil {
			return err
		}
		for k := k0; k <= k1; k++ {
			a := r.Region.Off + k*f.window
			if _, ok := f.windows[a]; ok {
				continue
			}
			f.todo = append(f.todo, window{a, min(a+f.window, r.Region.End)})
		}
	}
	if len(f.todo) == 0 {
		return nil
	}
	sort.Slice(f.todo, func(i, j int) bool { return f.todo[i].off < f.todo[j].off })
	f.Stats.Waves++
	for i, w := range f.todo {
		if i > 0 && w.off == f.todo[i-1].off {
			continue
		}
		buf := make([]byte, w.end-w.off)
		if err := f.r.ReadRange(ctx, w.off, buf); err != nil {
			return fmt.Errorf("cgfile: read [%d,%d): %w", w.off, w.end, err)
		}
		f.Stats.Requests++
		f.Stats.Bytes += int64(len(buf))
		f.windows[w.off] = buf
	}
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

// Bytes returns the bytes of a range already fetched. The result aliases the cached window
// unless the range crosses a window boundary (then it is a copy). Callers must not modify it.
func (f *Fetcher) Bytes(r Range) ([]byte, error) {
	k0, k1, err := f.span(r)
	if err != nil {
		return nil, err
	}
	if k1 < k0 {
		return nil, nil
	}
	get := func(k int64) ([]byte, int64, error) {
		a := r.Region.Off + k*f.window
		w, ok := f.windows[a]
		if !ok {
			return nil, 0, fmt.Errorf("cgfile: window %d of region [%d,%d) not fetched", k, r.Region.Off, r.Region.End)
		}
		return w, a, nil
	}
	w, a, err := get(k0)
	if err != nil {
		return nil, err
	}
	if k0 == k1 {
		s := r.Off - a
		return w[s : s+r.Len : s+r.Len], nil
	}
	out := make([]byte, 0, r.Len)
	out = append(out, w[r.Off-a:]...)
	for k := k0 + 1; k <= k1; k++ {
		if w, a, err = get(k); err != nil {
			return nil, err
		}
		need := r.Off + r.Len - a
		out = append(out, w[:min(int64(len(w)), need)]...)
	}
	return out, nil
}

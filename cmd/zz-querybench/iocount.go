package main

// Backend I/O accounting for the Go server: every object-store call Tempo's read path makes
// (backend.RawReader.ReadRange for ranged reads, Read for whole objects) is counted as one
// request, with the bytes it returned. Requests are served strictly one at a time, so the delta
// of these counters across a handler is exactly that request's I/O. Printed on the zzqb line as
// requests=<n> bytes=<n>, the same keys the Rust servers print.
//
// -backend-latency mocks object-store first-byte latency: every call waits that long before it
// is issued. Tempo reads each column chunk through one buffered reader (parquet ReadModeAsync: one
// page of read-ahead per chunk; ReadBufferCount is set but unused by vparquet5), so calls
// in flight together overlap their wait, exactly as concurrent GETs would. The Rust servers
// apply the same delay per request in their object reader (--io-latency).

import (
	"context"
	"io"
	"sync/atomic"
	"time"

	"github.com/grafana/tempo/v3/tempodb/backend"
)

var ioRequests, ioBytes atomic.Int64

// backendLatency is the mocked per-request latency (0 = none).
var backendLatency time.Duration

func waitLatency(ctx context.Context) error {
	if backendLatency <= 0 {
		return nil
	}
	t := time.NewTimer(backendLatency)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type countingRawReader struct {
	backend.RawReader
}

func (c countingRawReader) ReadRange(ctx context.Context, name string, keypath backend.KeyPath, offset uint64, buffer []byte, ci *backend.CacheInfo) error {
	if err := waitLatency(ctx); err != nil {
		return err
	}
	err := c.RawReader.ReadRange(ctx, name, keypath, offset, buffer, ci)
	ioRequests.Add(1)
	if err == nil {
		ioBytes.Add(int64(len(buffer)))
	}
	return err
}

func (c countingRawReader) Read(ctx context.Context, name string, keypath backend.KeyPath, ci *backend.CacheInfo) (io.ReadCloser, int64, error) {
	if err := waitLatency(ctx); err != nil {
		return nil, 0, err
	}
	rc, n, err := c.RawReader.Read(ctx, name, keypath, ci)
	ioRequests.Add(1)
	if err != nil {
		return rc, n, err
	}
	return countingReadCloser{rc}, n, nil
}

type countingReadCloser struct{ io.ReadCloser }

func (c countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	ioBytes.Add(int64(n))
	return n, err
}

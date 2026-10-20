// tempopb.Querier/FindTraceByID served through Tempo's real per-block path.
package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/bits-and-blooms/bloom/v3"
	"github.com/google/uuid"
	"github.com/grafana/dskit/user"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/grafana/tempo/v3/pkg/tempopb"
	"github.com/grafana/tempo/v3/pkg/validation"
	"github.com/grafana/tempo/v3/tempodb/backend"
	"github.com/grafana/tempo/v3/tempodb/encoding"
	"github.com/grafana/tempo/v3/tempodb/encoding/common"
)

// bloomPassReader is the backend.Reader the trace-by-id path reads through. It forwards
// every call to the real local reader except reads of `bloom-<n>` objects, which it answers
// with a 1-bit bloom filter whose only bit is set, so filter.Test is true for every id.
//
// Why: this block directory holds only data.parquet, index and meta.json — its bloom-<n>
// objects were not copied — and vparquet5.(*backendBlock).FindTraceByID begins with
// checkBloom, which would fail with ErrDoesNotExist. The pass-through filter turns the bloom
// check into a no-op (every id proceeds to the parquet lookup) without touching Tempo
// library code. In production the bloom read is a cache / object-store hit that rules out
// absent ids; it is not part of this measurement on any server. All 200 benchmark ids are
// present in the block, so the bloom could not have short-circuited any of them anyway.
type bloomPassReader struct {
	backend.Reader
	bloom []byte
}

func newBloomPassReader(r backend.Reader) (*bloomPassReader, error) {
	f := bloom.New(1, 1)
	f.Add([]byte{0}) // m=1: every location is bit 0, so Test(any) == true
	var buf bytes.Buffer
	if _, err := f.WriteTo(&buf); err != nil {
		return nil, err
	}
	return &bloomPassReader{Reader: r, bloom: buf.Bytes()}, nil
}

func (r *bloomPassReader) Read(ctx context.Context, name string, blockID uuid.UUID, tenantID string, ci *backend.CacheInfo) ([]byte, error) {
	if strings.HasPrefix(name, "bloom-") {
		return r.bloom, nil
	}
	return r.Reader.Read(ctx, name, blockID, tenantID, ci)
}

// FindTraceByID mirrors what modules/querier.(*Querier).FindTraceByID does for
// QueryMode=blocks against a store holding exactly this one block, i.e.
// tempodb.(*readerWriter).Find's per-block job: encoding.OpenBlock(meta, r) then
// block.FindTraceByID(ctx, id, opts). Deliberate differences:
//
//   - no blocklist poll and no includeBlock(): the single block's meta.json is read once at
//     server start (the same metadata a blocklist poll would hold) and the block is always
//     searched. includeBlock only filters on BlockStart/BlockEnd and the time range, both of
//     which the benchmark leaves open;
//   - the bloom check passes every id (bloomPassReader above: this block has no bloom objects);
//   - opts = common.DefaultSearchOptions() (the base the querier starts from; the querier
//     adds MaxBytesPerTrace, which vparquet5's FindTraceByID does not read), no
//     rw.cfg.Search.ApplyToOptions, no cache tiers (the local backend has none);
//   - checkIndex is left exactly as Tempo runs it by default: VPARQUET_INDEX is unset, so the
//     row group is found by the binary search over row-group minimum ids, not by the index file;
//   - the querier's trace.Combiner is not applied. With one partial trace, Combiner.Consume
//     stores it and Result() returns it unchanged (SortTrace only runs after a real combine),
//     so the returned trace is identical; only the span-dedup hash map is not built;
//   - tenant from x-scope-orgid or -tenant; no tracing spans or Prometheus observations.
//
// The block is opened per request and no decoded page survives a request (FAIRNESS 15b).
func (s *querierServer) FindTraceByID(ctx context.Context, req *tempopb.TraceByIDRequest) (*tempopb.TraceByIDResponse, error) {
	t := startRPC(ctx, "FindTraceByID")

	if !validation.ValidTraceID(req.TraceID) {
		return nil, status.Error(codes.InvalidArgument, "invalid trace id")
	}
	tenantID, err := user.ExtractOrgID(ctx)
	if err != nil || tenantID == "" {
		tenantID = s.defaultTenant
	}
	if tenantID != s.meta.TenantID {
		// A store has no blocks for another tenant: not found, as Tempo would answer.
		t.done(fmt.Sprintf("found=0 spans=0 tenant=%s", tenantID))
		return &tempopb.TraceByIDResponse{Metrics: &tempopb.TraceByIDMetrics{}}, nil
	}

	blk, err := encoding.OpenBlock(s.meta, s.tbiReader)
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("error opening block for reading, blockID: %s: %v", s.meta.BlockID, err))
	}
	found, err := blk.FindTraceByID(ctx, req.TraceID, common.DefaultSearchOptions())
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("error finding trace by id, blockID: %s: %v", s.meta.BlockID, err))
	}

	resp := &tempopb.TraceByIDResponse{Metrics: &tempopb.TraceByIDMetrics{}}
	if found != nil {
		resp.Trace = found.Trace
		tempopb.MergeTraceByIDMetrics(resp.Metrics, found.Metrics)
	}
	spans := 0
	if resp.Trace != nil {
		for _, rs := range resp.Trace.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				spans += len(ss.Spans)
			}
		}
	}
	f := 0
	if resp.Trace != nil {
		f = 1
	}
	t.done(fmt.Sprintf("found=%d spans=%d inspected_bytes=%d", f, spans, resp.Metrics.InspectedBytes))
	return resp, nil
}

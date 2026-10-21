package vparquet5

import (
	"context"

	tempo_io "github.com/grafana/tempo/v3/pkg/io"
	"github.com/grafana/tempo/v3/pkg/tempopb"
	"github.com/grafana/tempo/v3/tempodb/backend"
	"github.com/grafana/tempo/v3/tempodb/encoding/common"
)

// TraceConverter converts traces to vParquet5 rows exactly as CreateBlock does (dedicated
// columns, nested set, service stats, root names), for encodings built on vParquet5's rows.
type TraceConverter struct {
	res, span, event dedicatedColumnMapping
}

// NewTraceConverter returns a converter for a block's dedicated-column config.
func NewTraceConverter(dc backend.DedicatedColumns) *TraceConverter {
	return &TraceConverter{
		res:   dedicatedColumnsToColumnMapping(dc, backend.DedicatedColumnScopeResource),
		span:  dedicatedColumnsToColumnMapping(dc, backend.DedicatedColumnScopeSpan),
		event: dedicatedColumnsToColumnMapping(dc, backend.DedicatedColumnScopeEvent),
	}
}

// Convert converts tr into ot (reused when not nil) and reports whether the trace is connected.
func (c *TraceConverter) Convert(id common.ID, tr *tempopb.Trace, ot *Trace) (*Trace, bool) {
	return traceToParquetWithMapping(id, tr, ot, c.res, c.span, c.event)
}

// FilterDedicatedColumns drops the dedicated columns vParquet5 does not support, as
// CreateBlock does for the new block's meta.
func FilterDedicatedColumns(dc backend.DedicatedColumns) backend.DedicatedColumns {
	return filterDedicatedColumns(dc)
}

// CreateBlockFromRows is CreateBlock for rows that are already vParquet5 rows: each row from
// next (nil at the end) is written as it is, without the proto round trip, and row groups are
// cut as CreateBlock cuts them. Rows must be in trace-ID order and are not reused.
func CreateBlockFromRows(ctx context.Context, cfg *common.BlockConfig, meta *backend.BlockMeta, next func(context.Context) (*Trace, error), r backend.Reader, to backend.Writer) (*backend.BlockMeta, error) {
	s, newMeta := newStreamingBlock(ctx, cfg, meta, r, to, tempo_io.NewBufferedWriter)
	for {
		tr, err := next(ctx)
		if err != nil {
			return nil, err
		}
		if tr == nil {
			break
		}
		if err := s.Add(tr, 0, 0); err != nil {
			return nil, err
		}
		if s.EstimatedBufferedBytes() > cfg.RowGroupSizeBytes || s.RowGroupFull() {
			if _, err := s.Flush(); err != nil {
				return nil, err
			}
		}
	}
	if _, err := s.Complete(); err != nil {
		return nil, err
	}
	return newMeta, nil
}

package vcg1

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-kit/log"
	kitlevel "github.com/go-kit/log/level"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/grafana/tempo/v3/tempodb/backend"
	"github.com/grafana/tempo/v3/tempodb/encoding/common"
	cg "github.com/grafana/tempo/v3/tempodb/encoding/vcg1/cgfile"
	"github.com/grafana/tempo/v3/tempodb/encoding/vparquet5"
)

var tracer = otel.Tracer("tempodb/encoding/vcg1")

// NewCompactor returns a compactor of vCG1 blocks with the semantics of vparquet5's.
func NewCompactor(opts common.CompactionOptions) *Compactor {
	return &Compactor{opts: opts}
}

// Compactor merges vCG1 blocks the way vparquet5.Compactor merges vParquet5 blocks: an ordered
// merge by trace id across the inputs; traces whose id is in several inputs are taken as is
// when their rows are equal, kept from the first input when together they exceed
// MaxBytesPerTrace (the others' spans are reported discarded), and otherwise combined with
// vparquet5.Combiner (span dedupe, rebatching, nested set and service stats recomputed); the
// output is cut into blocks of totalObjects/OutputBlocks traces; every callback is called as
// vparquet5 calls it, with the size estimates vparquet5 computes from the parquet row.
//
// Each input is read in order window by window (cgfile.TraceIterator) and its traces are
// rebuilt as vParquet5 rows, which go to the writer as they are unless they need combining:
// memory is bounded by one window per input plus the writer's buffers.
type Compactor struct {
	opts common.CompactionOptions
}

var _ common.Compactor = (*Compactor)(nil)

// IterWindowSpans is the window of the input iterators (spans per window).
var IterWindowSpans = cg.DefaultIterWindowSpans

// ctxCheckEvery is how often (traces) the merge checks for cancellation between windows.
const ctxCheckEvery = 1024

func (c *Compactor) Compact(ctx context.Context, l log.Logger, r backend.Reader, w backend.Writer, inputs []*backend.BlockMeta) (newCompactedBlocks []*backend.BlockMeta, err error) {
	ctx, span := tracer.Start(ctx, "vcg1.compactor.Compact")
	defer span.End()

	if len(inputs) == 0 {
		return nil, nil
	}
	dcHash := inputs[0].DedicatedColumnsHash()
	var (
		compactionLevel uint32
		totalRecords    int64
		minBlockStart   time.Time
		maxBlockEnd     time.Time
		iters           = make([]*cg.TraceIterator, 0, len(inputs))
	)
	defer func() {
		for _, it := range iters {
			it.Close()
		}
	}()
	for _, m := range inputs {
		// The block selector only groups blocks of one version and one dedicated-column
		// config; a job mixing them is refused rather than misread.
		if m.Version != VersionString {
			return nil, fmt.Errorf("vCG1 compactor: block %s is %s, not %s", m.BlockID, m.Version, VersionString)
		}
		if m.DedicatedColumnsHash() != dcHash {
			return nil, fmt.Errorf("vCG1 compactor: block %s has different dedicated columns than block %s", m.BlockID, inputs[0].BlockID)
		}
		totalRecords += m.TotalObjects
		if m.CompactionLevel > compactionLevel {
			compactionLevel = m.CompactionLevel
		}
		if m.StartTime.Before(minBlockStart) || minBlockStart.IsZero() {
			minBlockStart = m.StartTime
		}
		if m.EndTime.After(maxBlockEnd) {
			maxBlockEnd = m.EndTime
		}

		b := &backendBlock{meta: m, r: r}
		rd, err := b.reader()
		if err != nil {
			return nil, fmt.Errorf("error opening block %s: %w", m.BlockID, err)
		}
		q, err := rd.NewRequest(ctx)
		if err != nil {
			return nil, fmt.Errorf("error reading footer of block %s: %w", m.BlockID, err)
		}
		iters = append(iters, rd.NewTraceIterator(q.F, m.DedicatedColumns, IterWindowSpans))
	}

	var (
		replicationFactor   = inputs[0].ReplicationFactor
		nextCompactionLevel = compactionLevel + 1
		lvl                 = int(compactionLevel)
		est                 = vparquet5.NewSizeEstimator(inputs[0].DedicatedColumns)
		outputBlocks        = max(int64(c.opts.OutputBlocks), 1)
		recordsPerBlock     = totalRecords / outputBlocks
		mg                  = &merger{c: c, iters: iters, est: est, lvl: lvl, rf: int(replicationFactor)}
		cur                 *blockWriter
		pendingObjects      int // traces added since the last ObjectsWritten
	)

	// ObjectsWritten and BytesWritten are reported with every append of block.cg to the
	// backend (vparquet5 reports them per flushed row group).
	onWrite := func(n int) {
		if c.opts.ObjectsWritten != nil && pendingObjects > 0 {
			c.opts.ObjectsWritten(lvl, pendingObjects)
		}
		pendingObjects = 0
		if c.opts.BytesWritten != nil {
			c.opts.BytesWritten(lvl, n)
		}
	}
	finish := func() error {
		cur.meta.StartTime = minBlockStart
		cur.meta.EndTime = maxBlockEnd
		_, span := tracer.Start(ctx, "vcg1.compactor.finishBlock")
		defer span.End()
		if _, err := cur.complete(ctx); err != nil {
			return fmt.Errorf("error shipping block to backend, blockID %s: %w", cur.meta.BlockID.String(), err)
		}
		m := cur.meta
		kitlevel.Info(l).Log(
			"msg", "wrote compacted block",
			"version", m.Version,
			"tenantID", m.TenantID,
			"blockID", m.BlockID.String(),
			"startTime", m.StartTime.String(),
			"endTime", m.EndTime.String(),
			"totalObjects", m.TotalObjects,
			"size", m.Size_,
			"compactionLevel", m.CompactionLevel,
			"totalRecords", m.TotalRecords,
			"bloomShardCount", m.BloomShardCount,
			"footerSize", m.FooterSize,
			"replicationFactor", m.ReplicationFactor,
			"dedicatedColumns", fmt.Sprintf("%+v", m.DedicatedColumns),
		)
		span.SetAttributes(attribute.String("blockID", m.BlockID.String()))
		cur = nil
		return nil
	}

	for n := 0; ; n++ {
		if n%ctxCheckEvery == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		id, row, err := mg.next(ctx)
		if err != nil {
			return nil, fmt.Errorf("error iterating input blocks: %w", err)
		}
		if id == nil {
			break
		}
		if row == nil {
			continue // dropped
		}

		if cur == nil {
			meta := &backend.BlockMeta{
				BlockID:           backend.NewUUID(),
				TenantID:          inputs[0].TenantID,
				TotalObjects:      recordsPerBlock, // just an estimate, for the bloom
				ReplicationFactor: replicationFactor,
				DedicatedColumns:  inputs[0].DedicatedColumns,
			}
			if cur, err = newBlockWriter(ctx, &c.opts.BlockConfig, meta, w, onWrite); err != nil {
				return nil, err
			}
			cur.meta.CompactionLevel = nextCompactionLevel
			newCompactedBlocks = append(newCompactedBlocks, cur.meta)
		}

		if c.opts.TraceWritten != nil {
			c.opts.TraceWritten(lvl, est.ProtoSize(row))
		}
		if err := cur.add(row); err != nil {
			return nil, err
		}
		pendingObjects++

		if cur.meta.TotalObjects >= recordsPerBlock {
			if err := finish(); err != nil {
				return nil, err
			}
		}
	}
	if cur != nil {
		if err := finish(); err != nil {
			return nil, err
		}
	}
	return newCompactedBlocks, nil
}

// merger is the ordered merge of the inputs (vparquet5's MultiBlockIterator with the
// compactor's combine and DropObject).
type merger struct {
	c     *Compactor
	iters []*cg.TraceIterator
	est   *vparquet5.SizeEstimator
	lvl   int
	rf    int

	lowest []int
	rows   []*vparquet5.Trace
}

// next returns the next trace id and its row (nil row: dropped by DropObject), or a nil id at
// the end.
func (m *merger) next(ctx context.Context) ([]byte, *vparquet5.Trace, error) {
	var lowestID []byte
	m.lowest = m.lowest[:0]
	for i, it := range m.iters {
		id, err := it.Peek(ctx)
		if err != nil {
			return nil, nil, err
		}
		if id == nil {
			continue
		}
		switch c := bytes.Compare(id, lowestID); {
		case lowestID == nil || c < 0:
			lowestID = id
			m.lowest = append(m.lowest[:0], i)
		case c == 0:
			m.lowest = append(m.lowest, i)
		}
	}
	if lowestID == nil {
		return nil, nil, nil
	}
	lowestID = append([]byte(nil), lowestID...)

	drop := m.c.opts.DropObject
	if len(m.lowest) == 1 {
		it := m.iters[m.lowest[0]]
		if drop != nil && drop(lowestID) {
			return lowestID, nil, it.Skip(ctx)
		}
		row, err := it.Next(ctx)
		return lowestID, row, err
	}

	m.rows = m.rows[:0]
	for _, i := range m.lowest {
		row, err := m.iters[i].Next(ctx)
		if err != nil {
			return nil, nil, err
		}
		if row == nil {
			return nil, nil, errors.New("unexpected nil object from lowest iterator")
		}
		m.rows = append(m.rows, row)
	}
	row, err := m.combine(m.rows)
	if err != nil {
		return nil, nil, fmt.Errorf("combining: %w", err)
	}
	clear(m.rows)
	if drop != nil && drop(lowestID) {
		return lowestID, nil, nil
	}
	return lowestID, row, nil
}

// combine is vparquet5's compactor combine over rebuilt rows.
func (m *merger) combine(rows []*vparquet5.Trace) (*vparquet5.Trace, error) {
	opts := &m.c.opts
	equal := true
	for _, row := range rows[1:] {
		if equal = rowsEqual(rows[0], row); !equal {
			break
		}
	}
	if equal {
		return rows[0], nil
	}

	if opts.MaxBytesPerTrace > 0 {
		sum := 0
		for _, row := range rows {
			sum += m.est.ProtoSize(row)
		}
		if sum > opts.MaxBytesPerTrace {
			// trace too large to compact
			if opts.SpansDiscarded != nil {
				for _, row := range rows[1:] {
					opts.SpansDiscarded(vparquet5.CountSpans(row))
				}
			}
			return rows[0], nil
		}
	}

	cmb := vparquet5.NewCombiner()
	deduped := 0
	for i, row := range rows {
		deduped += cmb.ConsumeWithFinal(row, i == len(rows)-1)
	}
	if opts.DedupedSpans != nil {
		opts.DedupedSpans(m.rf, deduped)
	}
	tr, _, connected := cmb.Result()
	if !connected && opts.DisconnectedTrace != nil {
		opts.DisconnectedTrace()
	}
	if tr != nil && tr.RootSpanName == "" && opts.RootlessTrace != nil {
		opts.RootlessTrace()
	}
	if opts.ObjectsCombined != nil {
		opts.ObjectsCombined(m.lvl, 1)
	}
	return tr, nil
}

package vcg1

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"

	"github.com/grafana/tempo/v3/pkg/cache"
	"github.com/grafana/tempo/v3/pkg/dataquality"
	"github.com/grafana/tempo/v3/tempodb/backend"
	"github.com/grafana/tempo/v3/tempodb/encoding/common"
	cg "github.com/grafana/tempo/v3/tempodb/encoding/vcg1/cgfile"
	"github.com/grafana/tempo/v3/tempodb/encoding/vcg1/cgwrite"
	"github.com/grafana/tempo/v3/tempodb/encoding/vparquet5"
)

// AnchorCutter is an optional extension of the common.Iterator given to CreateBlock: after
// each Next, CutAnchor reports whether that trace must start a new anchor (so a caller can
// align anchors, i.e. StartPage/TotalPages shards, with boundaries of its own, such as the
// row groups of a source block).
type AnchorCutter interface {
	CutAnchor() bool
}

// RowIterator is an optional extension of the common.Iterator given to CreateBlock: NextRow
// returns the next trace as a vParquet5 row built under meta.DedicatedColumns (nil at the
// end), and CreateBlock writes it as is, without the proto round trip (as vparquet5.CreateBlock
// does for its own WAL rows). A source block's rows keep their stored order and precomputed
// fields this way; converting them to proto and back would re-sort and recompute them.
type RowIterator interface {
	NextRow(ctx context.Context) (*vparquet5.Trace, error)
}

// appender writes one object through backend.Writer appends.
type appender struct {
	ctx      context.Context
	w        backend.Writer
	name     string
	blockID  uuid.UUID
	tenantID string
	tracker  backend.AppendTracker
	onWrite  func(n int) // optional: called after every append
}

func (a *appender) Write(p []byte) (int, error) {
	var err error
	a.tracker, err = a.w.Append(a.ctx, a.name, a.blockID, a.tenantID, a.tracker, p)
	if err == nil && a.onWrite != nil {
		a.onWrite(len(p))
	}
	return len(p), err
}

func (a *appender) Close() error { return a.w.CloseAppend(a.ctx, a.tracker) }

var _ io.WriteCloser = (*appender)(nil)

// blockWriter writes one block: block.cg streamed through the backend's appends, the bloom
// filters as vParquet5 writes them, and the block meta.
type blockWriter struct {
	cfg   *common.BlockConfig
	to    backend.Writer
	meta  *backend.BlockMeta
	bloom *common.ShardedBloomFilter
	out   *appender
	w     *cgwrite.Writer
}

// newBlockWriter starts the block of meta (tenant, block id, start and end, replication factor,
// dedicated columns; TotalObjects is the estimate the bloom filter is sized for).
func newBlockWriter(ctx context.Context, cfg *common.BlockConfig, meta *backend.BlockMeta, to backend.Writer, onWrite func(int)) (*blockWriter, error) {
	newMeta := backend.NewBlockMeta(meta.TenantID, uuid.UUID(meta.BlockID), VersionString)
	newMeta.StartTime = meta.StartTime
	newMeta.EndTime = meta.EndTime
	newMeta.ReplicationFactor = meta.ReplicationFactor
	newMeta.DedicatedColumns = vparquet5.FilterDedicatedColumns(meta.DedicatedColumns)

	out := &appender{ctx: ctx, w: to, name: cg.ObjectName, blockID: uuid.UUID(meta.BlockID), tenantID: meta.TenantID, onWrite: onWrite}
	w, err := cgwrite.New(out, cgwrite.Options{Dedicated: newMeta.DedicatedColumns})
	if err != nil {
		return nil, err
	}
	return &blockWriter{
		cfg:   cfg,
		to:    to,
		meta:  newMeta,
		bloom: common.NewBloom(cfg.BloomFP, uint(cfg.BloomShardSizeBytes), uint(meta.TotalObjects)),
		out:   out,
		w:     w,
	}, nil
}

// add writes one trace (in trace-ID order).
func (b *blockWriter) add(row *vparquet5.Trace) error {
	if err := b.w.Add(row); err != nil {
		return err
	}
	b.bloom.Add(row.TraceID)
	b.meta.ObjectAdded(0, 0) // start and end come from the input meta
	return nil
}

// complete writes the rest of block.cg, the no-compact flag if configured, the blooms and the
// meta, and returns the meta.
func (b *blockWriter) complete(ctx context.Context) (*backend.BlockMeta, error) {
	res, err := b.w.Close()
	if err != nil {
		return nil, err
	}
	if err := b.out.Close(); err != nil {
		return nil, err
	}
	m := b.meta
	m.Size_ = res.Size
	m.FooterSize = res.FooterLen
	m.TotalRecords = uint32(res.Anchors)
	m.BloomShardCount = uint32(b.bloom.GetShardCount())

	if b.cfg.CreateWithNoCompactFlag {
		// before the meta, so the block is never compactable before it is complete
		if err := b.to.WriteNoCompactFlag(ctx, uuid.UUID(m.BlockID), m.TenantID); err != nil {
			return nil, fmt.Errorf("unexpected error writing nocompact flag: %w", err)
		}
	}
	blooms, err := b.bloom.Marshal()
	if err != nil {
		return nil, err
	}
	ci := &backend.CacheInfo{Meta: m, Role: cache.RoleBloom}
	for n, bl := range blooms {
		if err := b.to.Write(ctx, common.BloomName(n), uuid.UUID(m.BlockID), m.TenantID, bl, ci); err != nil {
			return nil, fmt.Errorf("unexpected error writing bloom-%d: %w", n, err)
		}
	}
	if err := b.to.WriteBlockMeta(ctx, m); err != nil {
		return nil, fmt.Errorf("unexpected error writing meta: %w", err)
	}
	return m, nil
}

// CreateBlock writes the traces of i (in trace-ID order, as vparquet5.CreateBlock expects) as a
// CG block: block.cg streamed through to's appends, bloom filters as vParquet5 writes them, and
// the block meta.
func CreateBlock(ctx context.Context, cfg *common.BlockConfig, meta *backend.BlockMeta, i common.Iterator, _ backend.Reader, to backend.Writer) (*backend.BlockMeta, error) {
	bw, err := newBlockWriter(ctx, cfg, meta, to, nil)
	if err != nil {
		return nil, err
	}
	conv := vparquet5.NewTraceConverter(bw.meta.DedicatedColumns)
	cutter, _ := i.(AnchorCutter)
	rows, _ := i.(RowIterator)
	for {
		var row *vparquet5.Trace
		if rows != nil {
			if row, err = rows.NextRow(ctx); err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			if row == nil {
				break
			}
		} else {
			id, tr, err := i.Next(ctx)
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			if tr == nil || errors.Is(err, io.EOF) {
				break
			}
			// Copy the id: the iterator may reuse it.
			id = append([]byte(nil), id...)
			// A fresh row per trace: reusing one keeps the largest shape seen at every nesting
			// level, with the old traces' strings, alive for the whole block.
			var connected bool
			row, connected = conv.Convert(id, tr, nil)
			if !connected {
				dataquality.WarnDisconnectedTrace(meta.TenantID, dataquality.PhaseTraceWalToComplete)
			}
			if row.RootSpanName == "" {
				dataquality.WarnRootlessTrace(meta.TenantID, dataquality.PhaseTraceWalToComplete)
			}
		}
		if cutter != nil && cutter.CutAnchor() {
			bw.w.CutAnchor()
		}
		if err := bw.add(row); err != nil {
			return nil, err
		}
	}
	return bw.complete(ctx)
}

// CopyBlock copies a block's objects (block.cg, blooms, the optional shards.json, the
// no-compact flag) and writes toMeta, as vparquet5.CopyBlock does.
func CopyBlock(ctx context.Context, fromMeta, toMeta *backend.BlockMeta, from backend.Reader, to backend.Writer) error {
	rd, size, err := from.StreamReader(ctx, cg.ObjectName, uuid.UUID(fromMeta.BlockID), fromMeta.TenantID)
	if err != nil {
		return fmt.Errorf("error reading %s: %w", cg.ObjectName, err)
	}
	err = to.StreamWriter(ctx, cg.ObjectName, uuid.UUID(toMeta.BlockID), toMeta.TenantID, rd, size)
	rd.Close()
	if err != nil {
		return err
	}

	cpy := func(name string, ci *backend.CacheInfo) error {
		if ci != nil {
			ci.Meta = fromMeta
		}
		b, err := from.Read(ctx, name, uuid.UUID(fromMeta.BlockID), fromMeta.TenantID, ci)
		if err != nil {
			return fmt.Errorf("error reading %s: %w", name, err)
		}
		if ci != nil {
			ci.Meta = toMeta
		}
		return to.Write(ctx, name, uuid.UUID(toMeta.BlockID), toMeta.TenantID, b, ci)
	}
	for n := 0; n < common.ValidateShardCount(int(fromMeta.BloomShardCount)); n++ {
		if err := cpy(common.BloomName(n), &backend.CacheInfo{Role: cache.RoleBloom}); err != nil {
			return err
		}
	}
	if err := cpy(ShardsName, nil); err != nil && !errors.Is(err, backend.ErrDoesNotExist) {
		return err
	}

	noCompact, err := from.HasNoCompactFlag(ctx, uuid.UUID(fromMeta.BlockID), fromMeta.TenantID)
	if err != nil {
		return err
	}
	if noCompact {
		if err := to.WriteNoCompactFlag(ctx, uuid.UUID(toMeta.BlockID), toMeta.TenantID); err != nil {
			return err
		}
	}
	return to.WriteBlockMeta(ctx, toMeta)
}

// validate checks the trailer (magic, version, footer length against the meta) and that every
// bloom shard exists: cheap, not complete.
func validate(ctx context.Context, meta *backend.BlockMeta, r backend.Reader) error {
	if meta == nil {
		return errors.New("block meta is nil")
	}
	if meta.Size_ < uint64(len(cg.Magic)+cg.TrailerLen) {
		return fmt.Errorf("vCG1 block of %d bytes is too small", meta.Size_)
	}
	b := make([]byte, cg.TrailerLen)
	if err := r.ReadRange(ctx, cg.ObjectName, uuid.UUID(meta.BlockID), meta.TenantID, meta.Size_-cg.TrailerLen, b, nil); err != nil {
		return fmt.Errorf("failed to read vCG1 trailer: %w", err)
	}
	t, err := cg.ParseTrailer(b)
	if err != nil {
		return err
	}
	if t.FooterLen != meta.FooterSize {
		return fmt.Errorf("unexpected vCG1 footer size: %d, meta says %d", t.FooterLen, meta.FooterSize)
	}
	head := make([]byte, len(cg.Magic))
	if err := r.ReadRange(ctx, cg.ObjectName, uuid.UUID(meta.BlockID), meta.TenantID, 0, head, nil); err != nil {
		return fmt.Errorf("failed to read vCG1 magic: %w", err)
	}
	if string(head) != cg.Magic {
		return fmt.Errorf("invalid vCG1 magic: %x", head)
	}
	one := make([]byte, 1)
	for n := 0; n < int(meta.BloomShardCount); n++ {
		if err := r.ReadRange(ctx, common.BloomName(n), uuid.UUID(meta.BlockID), meta.TenantID, 0, one, nil); err != nil {
			return fmt.Errorf("failed to read first byte of bloom(%d): %w", n, err)
		}
	}
	return nil
}

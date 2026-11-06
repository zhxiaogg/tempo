package vcg1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/bits-and-blooms/bloom/v3"
	"github.com/google/uuid"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/klauspost/compress/zstd"

	"github.com/grafana/tempo/v3/pkg/cache"
	"github.com/grafana/tempo/v3/pkg/tempopb"
	"github.com/grafana/tempo/v3/pkg/util"
	"github.com/grafana/tempo/v3/tempodb/backend"
	"github.com/grafana/tempo/v3/tempodb/encoding/common"
	"github.com/grafana/tempo/v3/tempodb/encoding/vcg1/cgfile"
)

// ShardsName is the optional test-only object mapping source vParquet5 row groups to anchor
// ranges ([[a0, a1), ...]), so StartPage/TotalPages select the traces vParquet5 would.
const ShardsName = "shards.json"

type backendBlock struct {
	meta *backend.BlockMeta
	r    backend.Reader
}

var _ common.BackendBlock = (*backendBlock)(nil)

var (
	zstdOnce sync.Once
	zstdDec  *zstd.Decoder
	zstdErr  error
)

func decoder() (*zstd.Decoder, error) {
	zstdOnce.Do(func() { zstdDec, zstdErr = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0)) })
	return zstdDec, zstdErr
}

// reader opens block.cg (no I/O: the block meta carries the object and footer sizes). window
// is the read window (<= 0: cgfile.DefaultWindow).
func (b *backendBlock) reader(window int) (*cgfile.Reader, error) {
	if b.meta.Size_ == 0 || b.meta.FooterSize == 0 {
		return nil, fmt.Errorf("vCG1 block %s: meta has no size/footerSize", b.meta.BlockID)
	}
	dec, err := decoder()
	if err != nil {
		return nil, err
	}
	rr := cgfile.BackendRangeReader{R: b.r, Name: cgfile.ObjectName, BlockID: uuid.UUID(b.meta.BlockID), TenantID: b.meta.TenantID, CacheInfo: &backend.CacheInfo{Meta: b.meta}}
	opts := []cgfile.Option{cgfile.WithDecoder(dec)}
	if window > 0 {
		opts = append(opts, cgfile.WithWindow(int64(window)))
	}
	return cgfile.OpenKnown(rr, int64(b.meta.Size_), b.meta.FooterSize, opts...)
}

// request starts a request on the block: one read of exactly the footer and trailer, whose
// sizes the block meta carries, through the footer cache role (as vparquet5 reads its footer:
// every job on the block and every trace-by-ID lookup past the bloom starts with it). Reads
// use the options' read buffer size as the window.
func (b *backendBlock) request(ctx context.Context, opts common.SearchOptions) (*cgfile.Request, error) {
	rd, err := b.reader(opts.ReadBufferSize)
	if err != nil {
		return nil, err
	}
	n := int(b.meta.FooterSize)
	buf := make([]byte, n+cgfile.TrailerLen)
	ci := &backend.CacheInfo{Meta: b.meta, Role: cache.RoleParquetFooter}
	if err := b.r.ReadRange(ctx, cgfile.ObjectName, uuid.UUID(b.meta.BlockID), b.meta.TenantID, b.meta.Size_-uint64(len(buf)), buf, ci); err != nil {
		return nil, fmt.Errorf("vCG1 block %s: reading footer: %w", b.meta.BlockID, err)
	}
	t, err := cgfile.ParseTrailer(buf[n:])
	if err != nil {
		return nil, err
	}
	if t.FooterLen != b.meta.FooterSize {
		return nil, fmt.Errorf("vCG1 block %s: footer is %d bytes, meta says %d", b.meta.BlockID, t.FooterLen, b.meta.FooterSize)
	}
	dec, err := decoder()
	if err != nil {
		return nil, err
	}
	f, err := cgfile.DecodeFooter(dec, buf[:n])
	if err != nil {
		return nil, err
	}
	q := rd.NewRequestWithFooter(f)
	q.CountRead(len(buf))
	return q, nil
}

// shardCacheSize bounds the per-process cache of shard maps (one small entry per block,
// negative entries included, so a block without shards.json is not probed per request).
const shardCacheSize = 4096

var shardCache, _ = lru.New[string, [][2]int](shardCacheSize) // block id + tenant -> shard map (nil: none)

// shards returns the block's shard map (test addressing, not format), cached per process in an
// LRU of shardCacheSize blocks.
func (b *backendBlock) shards(ctx context.Context) ([][2]int, error) {
	k := b.meta.TenantID + "/" + b.meta.BlockID.String()
	if v, ok := shardCache.Get(k); ok {
		return v, nil
	}
	var out [][2]int
	raw, err := b.r.Read(ctx, ShardsName, uuid.UUID(b.meta.BlockID), b.meta.TenantID, nil)
	switch {
	case errors.Is(err, backend.ErrDoesNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("vCG1 %s: %w", ShardsName, err)
		}
	}
	shardCache.Add(k, out)
	return out, nil
}

// anchorRange maps StartPage/TotalPages to an anchor range: source row groups through
// shards.json, else one page per anchor. TotalPages == 0 is the whole block.
func (b *backendBlock) anchorRange(ctx context.Context, f *cgfile.Footer, opts common.SearchOptions) (int, int, error) {
	na := len(f.Anchors)
	if opts.TotalPages <= 0 {
		return 0, na, nil
	}
	sh, err := b.shards(ctx)
	if err != nil {
		return 0, 0, err
	}
	if sh == nil {
		a := min(opts.StartPage, na)
		return a, min(a+opts.TotalPages, na), nil
	}
	if opts.StartPage >= len(sh) {
		return na, na, nil
	}
	end := min(opts.StartPage+opts.TotalPages, len(sh))
	return min(sh[opts.StartPage][0], na), min(sh[end-1][1], na), nil
}

func (b *backendBlock) BlockMeta() *backend.BlockMeta { return b.meta }

func (b *backendBlock) Validate(ctx context.Context) error { return validate(ctx, b.meta, b.r) }

// checkBloom reports whether the block's bloom filter may hold id: the bloom shard of the id,
// read as vparquet5 reads it (through the bloom cache role).
func (b *backendBlock) checkBloom(ctx context.Context, id common.ID) (bool, error) {
	name := common.BloomName(common.ShardKeyForTraceID(id, int(b.meta.BloomShardCount)))
	raw, err := b.r.Read(ctx, name, uuid.UUID(b.meta.BlockID), b.meta.TenantID, &backend.CacheInfo{Meta: b.meta, Role: cache.RoleBloom})
	if err != nil {
		return false, fmt.Errorf("error retrieving bloom %s (%s, %s): %w", name, b.meta.TenantID, b.meta.BlockID, err)
	}
	filter := &bloom.BloomFilter{}
	if _, err := filter.ReadFrom(bytes.NewReader(raw)); err != nil {
		return false, fmt.Errorf("error parsing bloom (%s, %s): %w", b.meta.TenantID, b.meta.BlockID, err)
	}
	return filter.Test(id), nil
}

// FindTraceByID checks the bloom filter first (as vparquet5 does: in production a lookup fans
// out over every block of the time range, and the bloom rules most of them out without
// touching block.cg), then reads the footer and the trace.
func (b *backendBlock) FindTraceByID(ctx context.Context, id common.ID, opts common.SearchOptions) (*tempopb.TraceByIDResponse, error) {
	if !opts.SkipBloom {
		found, err := b.checkBloom(ctx, id)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, nil
		}
	}
	if opts.TraceByIDReadBufferSize > 0 {
		opts.ReadBufferSize = opts.TraceByIDReadBufferSize
	}
	q, err := b.request(ctx, opts)
	if err != nil {
		return nil, err
	}
	tr, err := q.FindTraceByID(ctx, util.PadTraceIDTo16Bytes(id))
	if err != nil {
		return nil, err
	}
	return &tempopb.TraceByIDResponse{Trace: tr, Metrics: &tempopb.TraceByIDMetrics{InspectedBytes: uint64(q.Stats().Bytes)}}, nil
}

func (b *backendBlock) Search(context.Context, *tempopb.SearchRequest, common.SearchOptions) (*tempopb.SearchResponse, error) {
	return &tempopb.SearchResponse{}, nil
}

package vcg1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"

	"github.com/grafana/tempo/v3/pkg/tempopb"
	"github.com/grafana/tempo/v3/pkg/traceql"
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

// request starts a request on the block: one read of exactly the footer and trailer, whose
// sizes the block meta carries.
func (b *backendBlock) request(ctx context.Context) (*cgfile.Request, error) {
	if b.meta.Size_ == 0 || b.meta.FooterSize == 0 {
		return nil, fmt.Errorf("vCG1 block %s: meta has no size/footerSize", b.meta.BlockID)
	}
	dec, err := decoder()
	if err != nil {
		return nil, err
	}
	rr := cgfile.BackendRangeReader{R: b.r, Name: cgfile.ObjectName, BlockID: uuid.UUID(b.meta.BlockID), TenantID: b.meta.TenantID, CacheInfo: &backend.CacheInfo{Meta: b.meta}}
	rd, err := cgfile.OpenKnown(rr, int64(b.meta.Size_), b.meta.FooterSize, cgfile.WithDecoder(dec))
	if err != nil {
		return nil, err
	}
	return rd.NewRequest(ctx)
}

var shardCache sync.Map // block id + tenant -> [][2]int (nil: no shards.json)

// shards returns the block's shard map, read once per process (test addressing, not format).
func (b *backendBlock) shards(ctx context.Context) ([][2]int, error) {
	k := b.meta.TenantID + "/" + b.meta.BlockID.String()
	if v, ok := shardCache.Load(k); ok {
		return v.([][2]int), nil
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
	shardCache.Store(k, out)
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

func (b *backendBlock) Validate(context.Context) error { return nil }

func (b *backendBlock) FindTraceByID(ctx context.Context, id common.ID, _ common.SearchOptions) (*tempopb.TraceByIDResponse, error) {
	q, err := b.request(ctx)
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

func (b *backendBlock) SearchTags(context.Context, traceql.AttributeScope, common.TagsCallback, common.MetricsCallback, common.SearchOptions) error {
	return util.ErrUnsupported
}

func (b *backendBlock) SearchTagValues(context.Context, string, common.TagValuesCallback, common.MetricsCallback, common.SearchOptions) error {
	return util.ErrUnsupported
}

func (b *backendBlock) SearchTagValuesV2(context.Context, traceql.Attribute, common.TagValuesCallbackV2, common.MetricsCallback, common.SearchOptions) error {
	return util.ErrUnsupported
}

func (b *backendBlock) FetchTagValues(context.Context, traceql.FetchTagValuesRequest, traceql.FetchTagValuesCallback, common.MetricsCallback, common.SearchOptions) error {
	return util.ErrUnsupported
}

func (b *backendBlock) FetchTagNames(context.Context, traceql.FetchTagsRequest, traceql.FetchTagsCallback, common.MetricsCallback, common.SearchOptions) error {
	return util.ErrUnsupported
}

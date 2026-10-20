package cgfile

import (
	"context"

	"github.com/google/uuid"

	"github.com/grafana/tempo/v3/tempodb/backend"
)

// ObjectName is the name of the block object.
const ObjectName = "block.cg"

// BackendRangeReader reads one object of a block through a Tempo backend.Reader.
type BackendRangeReader struct {
	R         backend.Reader
	Name      string
	BlockID   uuid.UUID
	TenantID  string
	CacheInfo *backend.CacheInfo
}

// ReadRange implements RangeReader.
func (b BackendRangeReader) ReadRange(ctx context.Context, off int64, buf []byte) error {
	return b.R.ReadRange(ctx, b.Name, b.BlockID, b.TenantID, uint64(off), buf, b.CacheInfo)
}

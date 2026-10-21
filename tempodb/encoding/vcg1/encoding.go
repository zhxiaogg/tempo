// Package vcg1 serves CG blocks (one object, block.cg; see package cgfile) through Tempo's
// per-block read path: Fetch reproduces vparquet5's storage contract for the TraceQL engine and
// FindTraceByID returns what vparquet5 returns. CreateBlock writes blocks (package cgwrite) and
// the Compactor merges them with vparquet5's compaction semantics; the WAL is not supported.
package vcg1

import (
	"context"
	"errors"
	"io/fs"
	"time"

	"github.com/grafana/tempo/v3/tempodb/backend"
	"github.com/grafana/tempo/v3/tempodb/encoding/common"
)

// VersionString is the block format name in meta.json.
const VersionString = "vCG1"

var errUnsupported = errors.New("vCG1: the WAL is not supported")

// Encoding implements encoding.VersionedEncoding for CG blocks.
type Encoding struct{}

func (Encoding) Version() string { return VersionString }

// OpenBlock does no I/O: every request reads the footer itself.
func (Encoding) OpenBlock(meta *backend.BlockMeta, r backend.Reader) (common.BackendBlock, error) {
	return &backendBlock{meta: meta, r: r}, nil
}

func (Encoding) NewCompactor(opts common.CompactionOptions) common.Compactor {
	return NewCompactor(opts)
}

func (Encoding) CompactionSupported() bool { return true }

func (Encoding) WritesSupported() bool { return true }

func (Encoding) CreateBlock(ctx context.Context, cfg *common.BlockConfig, meta *backend.BlockMeta, i common.Iterator, r backend.Reader, to backend.Writer) (*backend.BlockMeta, error) {
	return CreateBlock(ctx, cfg, meta, i, r, to)
}

func (Encoding) CopyBlock(ctx context.Context, meta *backend.BlockMeta, from backend.Reader, to backend.Writer) error {
	return CopyBlock(ctx, meta, meta, from, to)
}

func (Encoding) MigrateBlock(ctx context.Context, fromMeta, toMeta *backend.BlockMeta, from backend.Reader, to backend.Writer) error {
	return CopyBlock(ctx, fromMeta, toMeta, from, to)
}

func (Encoding) OpenWALBlock(string, string, time.Duration, time.Duration) (common.WALBlock, error, error) {
	return nil, nil, errUnsupported
}

func (Encoding) CreateWALBlock(*backend.BlockMeta, string, string, time.Duration) (common.WALBlock, error) {
	return nil, errUnsupported
}

func (Encoding) OwnsWALBlock(fs.DirEntry) bool { return false }

// Package vcg1 serves CG blocks (one object, block.cg; see package cgfile) through Tempo's
// per-block read path: Fetch reproduces vparquet5's storage contract for the TraceQL engine and
// FindTraceByID returns what vparquet5 returns. Blocks are read only: every write, compaction
// and WAL method is unsupported.
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

var errReadOnly = errors.New("vCG1 blocks are read-only: writes, compaction and WAL are not supported")

// Encoding implements encoding.VersionedEncoding for CG blocks.
type Encoding struct{}

func (Encoding) Version() string { return VersionString }

// OpenBlock does no I/O: every request reads the footer itself.
func (Encoding) OpenBlock(meta *backend.BlockMeta, r backend.Reader) (common.BackendBlock, error) {
	return &backendBlock{meta: meta, r: r}, nil
}

func (Encoding) NewCompactor(common.CompactionOptions) common.Compactor { return nil }

func (Encoding) CompactionSupported() bool { return false }

func (Encoding) WritesSupported() bool { return false }

func (Encoding) CreateBlock(context.Context, *common.BlockConfig, *backend.BlockMeta, common.Iterator, backend.Reader, backend.Writer) (*backend.BlockMeta, error) {
	return nil, errReadOnly
}

func (Encoding) CopyBlock(context.Context, *backend.BlockMeta, backend.Reader, backend.Writer) error {
	return errReadOnly
}

func (Encoding) MigrateBlock(context.Context, *backend.BlockMeta, *backend.BlockMeta, backend.Reader, backend.Writer) error {
	return errReadOnly
}

func (Encoding) OpenWALBlock(string, string, time.Duration, time.Duration) (common.WALBlock, error, error) {
	return nil, nil, errReadOnly
}

func (Encoding) CreateWALBlock(*backend.BlockMeta, string, string, time.Duration) (common.WALBlock, error) {
	return nil, errReadOnly
}

func (Encoding) OwnsWALBlock(fs.DirEntry) bool { return false }

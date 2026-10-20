package main

// -mode opencost: what Tempo pays to open the block on every SearchBlock / QueryRange /
// FindTraceByID. Each of those calls encoding.OpenBlock(...).Fetch / FindTraceByID, which runs
// vparquet5's openForSearch: a fresh BackendReaderAt and parquet.OpenFile (footer read and
// decode, schema). This mode repeats exactly that call sequence with the same file options;
// the one omission is openForSearch's unexported read-buffer wrapper (newCachedReaderAt), which
// only changes how the footer bytes are fetched, not how much is decoded. The Rust servers parse
// their metadata once at startup instead, so this cost is in every Go number and in no Rust one.

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"time"

	"github.com/google/uuid"
	"github.com/parquet-go/parquet-go"

	"github.com/grafana/tempo/v3/tempodb/backend"
	"github.com/grafana/tempo/v3/tempodb/backend/local"
	"github.com/grafana/tempo/v3/tempodb/encoding/vparquet5"
)

type openCostResult struct {
	Mode       string   `json:"mode"`
	Iters      int      `json:"iters"`
	Open       durStats `json:"open"`
	BytesRead  uint64   `json:"bytes_read_per_open"`
	AllocBytes uint64   `json:"alloc_bytes_per_open"`
	Env        env      `json:"env"`
}

func runOpenCost(blockDir, tenant string, iters int) (*openCostResult, error) {
	if blockDir == "" {
		return nil, errors.New("-mode opencost needs -block")
	}
	raw, _, _, err := local.New(&local.Config{Path: filepath.Dir(filepath.Dir(blockDir))})
	if err != nil {
		return nil, err
	}
	r := backend.NewReader(raw)
	id, err := uuid.Parse(filepath.Base(blockDir))
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	meta, err := r.BlockMeta(ctx, id, tenant)
	if err != nil {
		return nil, err
	}
	schema, _, _ := vparquet5.SchemaWithDynamicChanges(meta.DedicatedColumns)
	opts := []parquet.FileOption{
		parquet.SkipBloomFilters(true),
		parquet.SkipPageIndex(true),
		parquet.FileReadMode(parquet.ReadModeAsync),
		parquet.FileSchema(schema),
		parquet.ReadBufferSize(parquet.DefaultFileConfig().ReadBufferSize),
	}
	res := &openCostResult{Mode: "opencost", Iters: iters, Env: currentEnv()}
	var ns []int64
	for i := -3; i < iters; i++ {
		var m0, m1 runtime.MemStats
		runtime.ReadMemStats(&m0)
		t0 := time.Now()
		ra := vparquet5.NewBackendReaderAt(ctx, r, vparquet5.DataFileName, meta)
		if _, err := parquet.OpenFile(ra, int64(meta.Size_), opts...); err != nil {
			return nil, err
		}
		d := time.Since(t0).Nanoseconds()
		runtime.ReadMemStats(&m1)
		if i >= 0 {
			ns = append(ns, d)
			res.BytesRead = ra.BytesRead()
			res.AllocBytes = m1.TotalAlloc - m0.TotalAlloc
		}
	}
	res.Open = mkStats(ns, true)
	return res, nil
}

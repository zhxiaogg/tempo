// Package cgfile reads CG trace blocks (one object, block.cg: group chunks of column pages,
// key-major attribute runs, and a footer that locates every byte).
//
// A Reader holds the block-level state (trailer, zstd decoder) and is safe for concurrent
// use. Each request creates a Request, which reads the footer once and keeps everything it
// fetches and decodes: bytes are read through a Fetcher as 1 MiB windows of the region a
// range lies in (a group chunk, or one key's runs in a key-major flush), one wave per Load.
//
// Columns are decoded per page (Request.Load / LoadRows); byte-array dictionary pages carry
// codes into the request's merged dictionary of the column (Column.Dict), which holds each
// value once. Key-major runs are found by key (Footer.KeyID, Request.KeyRuns) and decoded
// whole (RunData). FindTraceByID rebuilds a trace as vparquet5 returns it.
package cgfile

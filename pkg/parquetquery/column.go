package parquetquery

import "github.com/parquet-go/parquet-go"

type ColumnChunkHelper struct {
	parquet.ColumnChunk
	pages     parquet.Pages
	firstPage parquet.Page
	err       error
}

// Dictionary makes it easier to access the dictionary for this column chunk which
// is only accessible through the first page. Internally keeps some open buffers
// to reuse later which are accessed through the other methods. If there is no dictionary
// for this column chunk or an error occurs, return nil.
func (h *ColumnChunkHelper) Dictionary() parquet.Dictionary {
	if h.pages == nil {
		h.pages = h.Pages()
	}

	if h.firstPage == nil {
		h.firstPage, h.err = h.pages.ReadPage()
	}

	if h.firstPage == nil {
		// Maybe there was an error
		return nil
	}

	return h.firstPage.Dictionary()
}

// NextPage wraps pages.ReadPage and helps reuse already open buffers.
func (h *ColumnChunkHelper) NextPage() (parquet.Page, error) {
	if h.err != nil {
		return nil, h.err
	}

	if h.firstPage != nil {
		// Clear and return the already buffered first page.
		// Caller takes ownership of it.
		pg := h.firstPage
		h.firstPage = nil
		return pg, nil
	}

	if h.pages == nil {
		h.pages = h.Pages()
	}

	return h.pages.ReadPage()
}

func (h *ColumnChunkHelper) Close() error {
	if h.firstPage != nil {
		parquet.Release(h.firstPage)
		h.firstPage = nil
	}

	if h.pages != nil {
		err := h.pages.Close()
		h.pages = nil
		return err
	}

	return nil
}

// FooterStats are a column chunk's statistics from the file footer, which is read
// with the file. The column index holds the same per page, but costs a separate read.
type FooterStats struct {
	NumValues int64 // including nulls
	NullCount int64
	// Min and Max are exact (unlike the column index, they are not truncated) and
	// set only when HasBounds. Chunks of only nulls, and columns that skip page
	// bounds, have none.
	Min, Max  parquet.Value
	HasBounds bool
}

// FooterStats returns the chunk's footer statistics. ok is false when there are none
// to trust: the chunk isn't read from a file, or no statistics were written. Writers
// need not fill in the null count, so a zero count is trusted only alongside bounds,
// which the same writer wrote with it.
func (h *ColumnChunkHelper) FooterStats() (stats FooterStats, ok bool) {
	fc, isFile := h.ColumnChunk.(*parquet.FileColumnChunk)
	if !isFile {
		return FooterStats{}, false
	}
	stats = FooterStats{NumValues: fc.NumValues(), NullCount: fc.NullCount()}
	stats.Min, stats.Max, stats.HasBounds = fc.Bounds()
	return stats, stats.HasBounds || stats.NullCount > 0
}

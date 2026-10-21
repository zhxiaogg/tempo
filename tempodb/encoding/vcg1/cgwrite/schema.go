package cgwrite

import (
	cg "github.com/grafana/tempo/v3/tempodb/encoding/vcg1/cgfile"
)

// Writer limits (the reference writer's defaults).
const (
	// PageBytes is the uncompressed bytes of a group page (all its columns).
	PageBytes = 256 << 10
	// AttrPageBytes is the page size of the point-copy attribute tables, which are read whole.
	AttrPageBytes = 4 << 20
	// ChunkBytes bounds a group chunk: a page that would take the pending chunk (compressed
	// pages + uncompressed dictionaries) past it starts the next chunk.
	ChunkBytes = 1 << 20
	// KmBytes bounds the buffered scan-copy entries of all levels together (exact bytes of the
	// buffers); past it the level holding the most is flushed.
	KmBytes = 256 << 20
	// PartBytes is the size of the appends to the backend (object-store multipart parts).
	PartBytes = 8 << 20

	// runRawMin and runRawDistinct: a run's strings get the run's own dictionary unless it has
	// at least runRawMin entries and more than runRawDistinct of them are distinct.
	runRawMin      = 64
	runRawDistinct = 0.5

	fileVersion   = 2
	footerVariant = 1
	numStreams    = 12
)

// enc is how a column's values are encoded.
type enc uint8

const (
	encDelta   enc = iota // DELTA_BINARY_PACKED
	encDictStr            // RLE_DICTIONARY, DELTA_LENGTH_BYTE_ARRAY fallback per chunk
	encRaw                // DELTA_LENGTH_BYTE_ARRAY
	encPlain              // PLAIN
	encDictInt            // RLE_DICTIONARY over an INT32 dictionary
)

// Encoding ids of the value sections.
const (
	idPlain    = cg.EncPlain
	idRLEDict  = cg.EncRLEDict
	idDelta    = cg.EncDeltaBinary
	idDeltaLen = cg.EncDeltaLength
)

// colEnc is the encoding class of every stored column (cgfile's schema gives type, nullability
// and zones).
func colEnc(t, c int) enc {
	switch t {
	case cg.TTraces:
		switch c {
		case cg.TrTraceID:
			return encRaw
		case cg.TrRootService, cg.TrRootSpan:
			return encDictStr
		}
		return encDelta
	case cg.TServiceStats:
		if c == cg.SsServiceName {
			return encDictStr
		}
		return encDelta
	case cg.TResources:
		if c == cg.ResServiceName {
			return encDictStr
		}
		return encDelta
	case cg.TScopes:
		if c == cg.ScName || c == cg.ScVersion {
			return encDictStr
		}
		return encDelta
	case cg.TSpans:
		switch c {
		case cg.SpSpanID, cg.SpParentSpanID, cg.SpTraceState, cg.SpStatusMessage:
			return encRaw
		case cg.SpName:
			return encDictStr
		}
		return encDelta
	case cg.TEvents:
		if c == cg.EvName {
			return encDictStr
		}
		return encDelta
	case cg.TLinks:
		switch c {
		case cg.LnTraceID, cg.LnSpanID, cg.LnTraceState:
			return encRaw
		}
		return encDelta
	}
	if (t-7)%3 == 0 { // point-copy attributes
		switch c {
		case cg.AtOwner:
			return encDelta
		case cg.AtKey, cg.AtStr:
			return encDictStr
		case cg.AtVType:
			return encDictInt
		case cg.AtStrRaw, cg.AtUnsupported:
			return encRaw
		}
		return encPlain
	}
	switch c { // elements
	case cg.ElAttrRow:
		return encDelta
	case cg.ElStr:
		return encDictStr
	case cg.ElStrRaw:
		return encRaw
	}
	return encPlain
}

// width is the uncompressed bytes per value counted for page cuts (byte arrays add their
// length).
func width(p cg.Phys) int {
	switch p {
	case cg.PhysInt64, cg.PhysDouble:
		return 8
	case cg.PhysBool:
		return 1
	}
	return 4
}

// group is a column group: a name, its column ids in page order and its page size.
type group struct {
	name      string
	cols      []int
	pageBytes int
}

// defaultGroups are the reference writer's default column groups. Key-major tables have none.
func defaultGroups(t int) []group {
	g := func(name string, cols ...int) group { return group{name: name, cols: cols, pageBytes: PageBytes} }
	switch t {
	case cg.TTraces:
		return []group{
			g("tid", cg.TrTraceID),
			g("tmeta", cg.TrStart, cg.TrDuration, cg.TrRootService, cg.TrRootSpan, cg.TrFirstStat, cg.TrFirstResource, cg.TrFirstSpan),
			g("trest", cg.TrEnd),
		}
	case cg.TServiceStats:
		return []group{g("ss", cg.SsServiceName, cg.SsSpanCount, cg.SsErrorCount)}
	case cg.TResources:
		return []group{g("res", cg.ResServiceName, cg.ResDropped, cg.ResFirstScope, cg.ResFirstSpan)}
	case cg.TScopes:
		return []group{g("scope", cg.ScName, cg.ScVersion, cg.ScDropped, cg.ScFirstSpan)}
	case cg.TSpans:
		return []group{
			g("sname", cg.SpName),
			g("sfilter", cg.SpKind, cg.SpStatusCode),
			g("sdur", cg.SpDuration),
			g("sstart", cg.SpStart),
			g("sid", cg.SpSpanID),
			g("stree", cg.SpParentID, cg.SpNestedLeft, cg.SpNestedRight, cg.SpChildCount),
			g("r15", cg.SpRounded15),
			g("r60", cg.SpRounded60),
			g("r300", cg.SpRounded300),
			g("r3600", cg.SpRounded3600),
			g("slinks", cg.SpFirstEvent, cg.SpFirstLink),
			g("srest", cg.SpParentSpanID, cg.SpTraceState, cg.SpStatusMessage, cg.SpDroppedAttrs, cg.SpDroppedEvents, cg.SpDroppedLinks),
		}
	case cg.TEvents:
		return []group{g("ev", cg.EvTime, cg.EvName, cg.EvDropped)}
	case cg.TLinks:
		return []group{g("ln", cg.LnTraceID, cg.LnSpanID, cg.LnTraceState, cg.LnDropped)}
	}
	if (t-7)%3 == 2 {
		return nil
	}
	cols := make([]int, len(cg.Cols(t)))
	for i := range cols {
		cols[i] = i
	}
	return []group{{name: "all", cols: cols, pageBytes: AttrPageBytes}}
}

// anchorGroup is the traces group whose pages carry the anchors: the one holding first_span.
const anchorGroup = 1

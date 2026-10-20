package cgfile

// Phys is the physical type of a column. It is not stored in the file: the schema below
// provides it for every (table, column id).
type Phys uint8

const (
	PhysInt32 Phys = iota
	PhysInt64
	PhysDouble
	PhysBool
	PhysBytes
)

// Table ids.
const (
	TTraces = iota
	TServiceStats
	TResources
	TScopes
	TSpans
	TEvents
	TLinks

	NumTables = 22
	NumLevels = 5
)

// Attribute levels.
const (
	LResource = iota
	LScope
	LSpan
	LEvent
	LLink
)

// OwnerTable is the owner table of each attribute level.
var OwnerTable = [NumLevels]int{TResources, TScopes, TSpans, TEvents, TLinks}

// AttrsTable is the point-copy attribute table of level l.
func AttrsTable(l int) int { return 7 + 3*l }

// ElemsTable is the array-element table of level l.
func ElemsTable(l int) int { return 8 + 3*l }

// KmTable is the key-major (scan copy) table of level l.
func KmTable(l int) int { return 9 + 3*l }

// Column ids per table.
const (
	TrTraceID = iota
	TrStart
	TrEnd
	TrDuration
	TrRootService
	TrRootSpan
	TrFirstStat
	TrFirstResource
	TrFirstSpan
)

const (
	SsServiceName = iota + 1
	SsSpanCount
	SsErrorCount
)

const (
	ResServiceName = iota + 1
	ResDropped
	ResFirstScope
	ResFirstSpan
)

const (
	ScName = iota + 1
	ScVersion
	ScDropped
	ScFirstSpan
)

const (
	SpSpanID = iota + 1
	SpParentSpanID
	SpParentID
	SpNestedLeft
	SpNestedRight
	SpChildCount
	SpName
	SpKind
	SpTraceState
	SpStart
	SpDuration
	SpStatusCode
	SpStatusMessage
	SpDroppedAttrs
	SpDroppedEvents
	SpDroppedLinks
	SpRounded15
	SpRounded60
	SpRounded300
	SpRounded3600
	SpFirstEvent
	SpFirstLink
)

const (
	EvTime = iota + 1
	EvName
	EvDropped
)

const (
	LnTraceID = iota + 1
	LnSpanID
	LnTraceState
	LnDropped
)

// Point-copy attribute columns ({level}_attrs).
const (
	AtOwner = iota
	AtKey
	AtVType
	AtStr
	AtStrRaw
	AtInt
	AtDbl
	AtBool
	AtUnsupported
)

// Element columns ({level}_attr_elems).
const (
	ElAttrRow = iota
	ElStr
	ElStrRaw
	ElInt
	ElDbl
	ElBool
)

// Value type ids ({level}_attrs.vtype and run vtype).
const (
	VTStr = iota
	VTInt
	VTDouble
	VTBool
	VTStrArr
	VTIntArr
	VTDoubleArr
	VTBoolArr
	VTEmptyArr
	VTUnsupported
	VTEmpty
)

// Run value columns (vcol), relative to the key-major value columns.
const (
	VColStr    = 0
	VColStrRaw = 1
	VColInt    = 2
	VColDbl    = 3
	VColBool   = 4
	VColNone   = 0xff
)

// VColPhys is the physical type of each run value column.
var VColPhys = [5]Phys{PhysBytes, PhysBytes, PhysInt64, PhysDouble, PhysBool}

// ColSpec describes one column of a table.
type ColSpec struct {
	Name   string
	Phys   Phys
	Opt    bool // nullable: buffers may carry def levels
	Stored bool // false: parent pointers and key-major run metadata
	Zone   bool // per-page min/max in the footer
}

func col(name string, p Phys) ColSpec  { return ColSpec{Name: name, Phys: p, Stored: true} }
func opt(name string, p Phys) ColSpec  { return ColSpec{Name: name, Phys: p, Opt: true, Stored: true} }
func zone(name string, p Phys) ColSpec { return ColSpec{Name: name, Phys: p, Stored: true, Zone: true} }

var parentRow = ColSpec{Name: "parent_row", Phys: PhysInt32}

var (
	tracesCols = []ColSpec{
		zone("trace_id", PhysBytes), zone("start_ns", PhysInt64), zone("end_ns", PhysInt64),
		zone("duration_ns", PhysInt64), col("root_service", PhysBytes), col("root_span", PhysBytes),
		col("first_stat", PhysInt32), col("first_resource", PhysInt32), col("first_span", PhysInt32),
	}
	serviceStatsCols = []ColSpec{
		parentRow, col("service_name", PhysBytes), col("span_count", PhysInt32), col("error_count", PhysInt32),
	}
	resourcesCols = []ColSpec{
		parentRow, col("service_name", PhysBytes), col("dropped_attrs", PhysInt32),
		col("first_scope", PhysInt32), col("first_span", PhysInt32),
	}
	scopesCols = []ColSpec{
		parentRow, col("name", PhysBytes), col("version", PhysBytes), col("dropped_attrs", PhysInt32),
		col("first_span", PhysInt32),
	}
	spansCols = []ColSpec{
		parentRow, col("span_id", PhysBytes), opt("parent_span_id", PhysBytes), col("parent_id", PhysInt32),
		col("nested_left", PhysInt32), col("nested_right", PhysInt32), col("child_count", PhysInt32),
		col("name", PhysBytes), col("kind", PhysInt64), col("trace_state", PhysBytes),
		zone("start_ns", PhysInt64), zone("duration_ns", PhysInt64), zone("status_code", PhysInt64),
		col("status_message", PhysBytes), col("dropped_attrs", PhysInt32), col("dropped_events", PhysInt32),
		col("dropped_links", PhysInt32), col("rounded_15", PhysInt32), col("rounded_60", PhysInt32),
		col("rounded_300", PhysInt32), col("rounded_3600", PhysInt32), col("first_event", PhysInt32),
		col("first_link", PhysInt32),
	}
	eventsCols = []ColSpec{
		parentRow, col("time_since_start_ns", PhysInt64), col("name", PhysBytes), col("dropped_attrs", PhysInt32),
	}
	linksCols = []ColSpec{
		parentRow, col("trace_id", PhysBytes), col("span_id", PhysBytes), col("trace_state", PhysBytes),
		col("dropped_attrs", PhysInt32),
	}
	attrsCols = []ColSpec{
		zone("owner_row", PhysInt32), col("key", PhysBytes), col("vtype", PhysInt32),
		opt("str", PhysBytes), opt("str_raw", PhysBytes), opt("int", PhysInt64), opt("dbl", PhysDouble),
		opt("bool", PhysBool), opt("unsupported", PhysBytes),
	}
	elemsCols = []ColSpec{
		zone("attr_row", PhysInt32), opt("str", PhysBytes), opt("str_raw", PhysBytes), opt("int", PhysInt64),
		opt("dbl", PhysDouble), opt("bool", PhysBool),
	}
	kmCols = []ColSpec{
		{Name: "key", Phys: PhysBytes}, {Name: "vtype", Phys: PhysInt32}, col("owner_row", PhysInt32),
		opt("str", PhysBytes), opt("str_raw", PhysBytes), opt("int", PhysInt64), opt("dbl", PhysDouble),
		opt("bool", PhysBool),
	}
)

var levelNames = [NumLevels]string{"resource", "scope", "span", "event", "link"}

// Cols returns the schema of table t.
func Cols(t int) []ColSpec {
	switch t {
	case TTraces:
		return tracesCols
	case TServiceStats:
		return serviceStatsCols
	case TResources:
		return resourcesCols
	case TScopes:
		return scopesCols
	case TSpans:
		return spansCols
	case TEvents:
		return eventsCols
	case TLinks:
		return linksCols
	}
	switch (t - 7) % 3 {
	case 0:
		return attrsCols
	case 1:
		return elemsCols
	}
	return kmCols
}

// TableName returns the name of table t.
func TableName(t int) string {
	if t < 7 {
		return [...]string{"traces", "service_stats", "resources", "scopes", "spans", "events", "links"}[t]
	}
	return levelNames[(t-7)/3] + [...]string{"_attrs", "_attr_elems", "_attrs_km"}[(t-7)%3]
}

var storedCols = func() (out [NumTables][]int) {
	for t := range out {
		for i, c := range Cols(t) {
			if c.Stored {
				out[t] = append(out[t], i)
			}
		}
	}
	return out
}()

// StoredCols returns the stored column ids of table t (shared; do not modify).
func StoredCols(t int) []int { return storedCols[t] }

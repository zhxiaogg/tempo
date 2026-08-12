package traceql

import (
	math_bits "math/bits"

	"github.com/grafana/tempo/pkg/tempopb"
)

var _ SpanWatcher = (*engineBytesWatcher)(nil)

type engineBytesWatcher struct {
	bytes uint64
}

// NewEngineBytesWatcher returns a watcher that estimates encoded attribute bytes on matched spans.
// For each watched span it adds Span.AttributesEncodedSize plus the span start time. The running
// total is reported under tempopb.AdditionalMetricEngineBytes.
func NewEngineBytesWatcher() SpanWatcher {
	return &engineBytesWatcher{}
}

func (e *engineBytesWatcher) Conditions() []Condition {
	return nil
}

func (e *engineBytesWatcher) WatchSpan(span Span) bool {
	e.bytes += span.AttributesEncodedSize()
	if st := span.StartTimeUnixNanos(); st != 0 {
		e.bytes += 1 + uint64(VarIntSize(st))
	}
	return true // keep watching every span
}

// AttributeNameEncodedSize returns the encoded size of an attribute name: the name bytes plus the
// varint holding their length.
func AttributeNameEncodedSize(a *Attribute) int {
	l := len(a.Name)
	return l + VarIntSize(uint64(l))
}

// StaticEncodedSize returns the encoded size of a Static value.
// Scalars: 1 type byte + payload.
// Arrays: 1 type byte + length varint + element payloads (no per-element type byte).
// Unknown types size as a bare type byte rather than panicking; this runs on every matched span
// and a usage metric must never take a query down.
func StaticEncodedSize(v *Static) int {
	switch v.Type {
	case TypeNil:
		return 1
	case TypeString:
		l := len(v.valBytes)
		return 1 + l + VarIntSize(uint64(l))
	case TypeInt, TypeStatus, TypeKind, TypeDuration:
		return 1 + VarIntSize(v.valScalar)
	case TypeFloat:
		return 1 + 8
	case TypeBoolean:
		return 1 + 1
	case TypeIntArray:
		ints, _ := v.IntArray()
		n := 1 + VarIntSize(uint64(len(ints)))
		for _, i := range ints {
			n += VarIntSize(uint64(i))
		}
		return n
	case TypeFloatArray:
		floats, _ := v.FloatArray()
		return 1 + VarIntSize(uint64(len(floats))) + 8*len(floats)
	case TypeStringArray:
		strs, _ := v.StringArray()
		n := 1 + VarIntSize(uint64(len(strs)))
		for _, s := range strs {
			l := len(s)
			n += l + VarIntSize(uint64(l))
		}
		return n
	case TypeBooleanArray:
		bools, _ := v.BooleanArray()
		return 1 + VarIntSize(uint64(len(bools))) + len(bools)
	default:
		return 1
	}
}

// VarIntSize returns the number of bytes a protobuf varint encoding of v occupies.
func VarIntSize(v uint64) int {
	return (math_bits.Len64(v|1) + 6) / 7
}

func (e *engineBytesWatcher) Active() bool {
	return true
}

func (e *engineBytesWatcher) Stats() map[string]int64 {
	return map[string]int64{tempopb.AdditionalMetricEngineBytes: int64(e.bytes)}
}

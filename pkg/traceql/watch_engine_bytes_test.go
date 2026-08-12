package traceql

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/grafana/tempo/pkg/tempopb"
)

func spanWithAttr(key string, val Static) *mockSpan {
	s := newMockSpan(nil)
	s.attributes[NewScopedAttribute(AttributeScopeSpan, false, key)] = val
	return s
}

func TestEngineBytesWatcher(t *testing.T) {
	tests := []struct {
		name string
		span Span
		want int64
	}{
		{
			name: "start time 1 is 2 bytes",
			span: newMockSpan(nil).WithStartTime(1),
			want: 2,
		},
		// Each case below is name + value. The name costs len(name) plus the varint holding
		// that length; the value is sized per StaticEncodedSize.
		{
			// name "missing" 7+1 → 8; value: type byte only → 1
			name: "nil attribute is 9 bytes",
			span: spanWithAttr("missing", NewStaticNil()),
			want: 9,
		},
		{
			// name 16+1 → 17; value: type(1) + varint(200)=2 → 3
			name: "http.status_code=200 is 20 bytes",
			span: newMockSpan(nil).WithSpanInt("http.status_code", 200),
			want: 20,
		},
		{
			// name 12+1 → 13; value: type(1) + varint(4)=1 + 4 → 6
			name: `service.name="test" is 19 bytes`,
			span: newMockSpan(nil).WithSpanString("service.name", "test"),
			want: 19,
		},
		{
			// name "codes" 5+1 → 6; value: type(1) + len(1) + varint(1)=1 + varint(200)=2 → 5
			name: "int array [1, 200] is 11 bytes",
			span: spanWithAttr("codes", NewStaticIntArray([]int{1, 200})),
			want: 11,
		},
		{
			// name "tags" 4+1 → 5; value: type(1) + len(1) + varint(len("test"))=1 + 4 → 7
			name: `string array ["test"] is 12 bytes`,
			span: spanWithAttr("tags", NewStaticStringArray([]string{"test"})),
			want: 12,
		},
		{
			// name "values" 6+1 → 7; value: type(1) + len(1) + 8 → 10
			name: "float array [1.5] is 17 bytes",
			span: spanWithAttr("values", NewStaticFloatArray([]float64{1.5})),
			want: 17,
		},
		{
			// name "flags" 5+1 → 6; value: type(1) + len(1) + bool + bool → 4
			name: "bool array [true, false] is 10 bytes",
			span: spanWithAttr("flags", NewStaticBooleanArray([]bool{true, false})),
			want: 10,
		},
		{
			// name "empty" 5+1 → 6; value: type(1) + len(0)=1 → 2
			name: "empty int array is 8 bytes",
			span: spanWithAttr("empty", NewStaticIntArray([]int{})),
			want: 8,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := NewEngineBytesWatcher()
			require.True(t, o.WatchSpan(tt.span))
			require.Equal(t, tt.want, o.Stats()[tempopb.AdditionalMetricEngineBytes])
		})
	}
}

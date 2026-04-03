package codex

import (
	"encoding/json"
	"testing"
)

func TestToFloat64(t *testing.T) {
	t.Parallel()
	tests := []struct {
		v    any
		name string
		want float64
		ok   bool
	}{
		{float64(42), "float64", 42, true},
		{int(7), "int", 7, true},
		{int64(99), "int64", 99, true},
		{json.Number("123"), "json.Number", 123, true},
		{"nope", "string", 0, false},
		{nil, "nil", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := toFloat64(tt.v)
			if ok != tt.ok {
				t.Errorf("ok = %v, want %v", ok, tt.ok)
			}
			if ok && got != tt.want {
				t.Errorf("got = %v, want %v", got, tt.want)
			}
		})
	}
}

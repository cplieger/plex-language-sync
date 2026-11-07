package alertrules

import (
	"slices"
	"testing"
)

func TestLineFilters(t *testing.T) {
	t.Parallel()
	expr := "absent_over_time({container=\"x\"} |= `deep analysis completed` |= \"complete=true\" != `a\"b` [50h])"
	got := LineFilters(t, expr)
	want := []LineFilter{
		{Text: "deep analysis completed", Contains: true},
		{Text: "complete=true", Contains: true},
		{Text: `a"b`, Contains: false},
	}
	if !slices.Equal(got, want) {
		t.Errorf("LineFilters = %+v, want %+v", got, want)
	}
}

func TestMatch(t *testing.T) {
	t.Parallel()
	filters := []LineFilter{{Text: "done", Contains: true}, {Text: "complete=false", Contains: false}}
	tests := []struct {
		line string
		want bool
	}{
		{"msg=done complete=true", true},
		{"msg=done complete=false", false},
		{"msg=started complete=true", false},
	}
	for _, tt := range tests {
		if got := Match(tt.line, filters); got != tt.want {
			t.Errorf("Match(%q) = %v, want %v", tt.line, got, tt.want)
		}
	}
}

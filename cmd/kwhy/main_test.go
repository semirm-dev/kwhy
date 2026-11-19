package main

import (
	"io"
	"testing"
)

func TestRunExitCodes(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{"no command", nil, 2},
		{"unknown command", []string{"nope"}, 2},
		{"help", []string{"help"}, 0},
		{"help flag", []string{"value", "--help"}, 0},
		{"unknown flag", []string{"value", "--nope"}, 2},
		{"value missing chart", []string{"value", "deploy/api", ".spec.replicas"}, 2},
		{"value missing path", []string{"value", "deploy/api", "--chart", "c"}, 2},
		{"value path and all", []string{"value", "deploy/api", ".spec.replicas", "--all", "--chart", "c"}, 2},
		{"value bad output", []string{"value", "deploy/api", "--all", "--chart", "c", "-o", "yaml"}, 2},
		{"value flags after args", []string{"value", "deploy/api", ".spec.replicas", "--chart", "c", "-f", "a.yaml", "-f", "b.yaml"}, 1},
		{"change", []string{"change"}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := run(tt.args, io.Discard, io.Discard); got != tt.want {
				t.Errorf("run(%q) = %d, want %d", tt.args, got, tt.want)
			}
		})
	}
}

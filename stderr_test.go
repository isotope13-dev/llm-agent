package llmagent

import "testing"

func TestIsStderrNoise(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"YOLO mode enabled", true},
		{"Loaded cached credentials from disk", true},
		{"Using credentials for project abc", true},
		{"Welcome to Gemini CLI", true},
		{"actual error: connection refused", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsStderrNoise(tt.in); got != tt.want {
			t.Errorf("IsStderrNoise(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

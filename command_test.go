package llmagent

import (
	"context"
	"strings"
	"testing"
)

func TestDefaultCommand(t *testing.T) {
	tests := []struct {
		provider    string
		wantBin     string
		wantInArgv  []string
		wantNotArgv []string
	}{
		{"claude", "claude", []string{"--verbose", "--output-format", "stream-json"}, nil},
		{"gemini", "gemini", []string{"--yolo", "--output-format", "stream-json"}, []string{"--model"}},
		{"gemini:gemini-2.5-pro", "gemini", []string{"--model", "gemini-2.5-pro"}, nil},
		{"codex", "codex", []string{"exec", "--json", "--full-auto"}, nil},
		{"opencode", "opencode", []string{"run", "--format", "json"}, []string{"--model"}},
		{"opencode:kimi-k2", "opencode", []string{"--model", "kimi-k2"}, nil},
		{"cursor", "agent", []string{"--model", "auto", "Follow the instructions in PROMPT.md exactly"}, nil},
		{"cursor:sonnet-4", "agent", []string{"--model", "sonnet-4"}, nil},
		{"pi", "pi", []string{"--mode", "rpc"}, []string{"--model", "--no-session"}},
		{"pi:anthropic/claude-sonnet-4-20250514", "pi", []string{"--mode", "rpc", "--model", "anthropic/claude-sonnet-4-20250514"}, []string{"--no-session"}},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			a := &Agent{Provider: tt.provider}
			cmd, err := DefaultCommand(context.Background(), a)
			if err != nil {
				t.Fatalf("DefaultCommand: %v", err)
			}
			if cmd.Args[0] != tt.wantBin {
				t.Fatalf("argv[0] = %q, want %q (%v)", cmd.Args[0], tt.wantBin, cmd.Args)
			}
			argv := strings.Join(cmd.Args, " ")
			for _, want := range tt.wantInArgv {
				if !strings.Contains(argv, want) {
					t.Errorf("argv missing %q: %s", want, argv)
				}
			}
			for _, no := range tt.wantNotArgv {
				if strings.Contains(argv, no) {
					t.Errorf("argv unexpectedly contains %q: %s", no, argv)
				}
			}
		})
	}
}

func TestDefaultCommandUnknown(t *testing.T) {
	a := &Agent{Provider: "made-up-thing"}
	if _, err := DefaultCommand(context.Background(), a); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestDefaultCommandIncludeDirs(t *testing.T) {
	a := &Agent{Provider: "gemini", IncludeDirs: []string{"/a", "/b"}}
	cmd, err := DefaultCommand(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Join(cmd.Args, " ")
	if !strings.Contains(argv, "--include-directories /a") || !strings.Contains(argv, "--include-directories /b") {
		t.Errorf("gemini argv missing include dirs: %s", argv)
	}
}

func TestDefaultCommandTmpDir(t *testing.T) {
	a := &Agent{Provider: "claude", TmpDir: "/scratch/x"}
	cmd, err := DefaultCommand(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, e := range cmd.Env {
		if e == "TMPDIR=/scratch/x" {
			seen = true
		}
	}
	if !seen {
		t.Errorf("env missing TMPDIR override")
	}
}

func TestResumeArgs(t *testing.T) {
	tests := []struct {
		provider, sid string
		want          []string
	}{
		{"claude", "abc", []string{"-r", "abc"}},
		{"opencode:kimi-k2", "abc", []string{"-s", "abc"}},
		{"cursor", "abc", []string{"--resume", "abc"}},
		{"gemini", "abc", nil},
		{"codex", "abc", nil},
		{"claude", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.provider+"/"+tt.sid, func(t *testing.T) {
			got := resumeArgs(tt.provider, tt.sid)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("[%d] %q want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

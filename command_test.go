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
		{"agy", "agy", []string{"--output-format", "stream-json", "--input-format", "stream-json", "--dangerously-skip-permissions"}, []string{"--model", "--effort"}},
		{"agy:gemini-3.1-pro@high", "agy", []string{"--model", "gemini-3.1-pro", "--effort", "high"}, nil},
		// "gemini" is a legacy alias for the same CLI; the standalone gemini
		// binary is no longer invoked.
		{"gemini", "agy", []string{"--input-format", "stream-json"}, []string{"--yolo", "--include-directories"}},
		// --full-auto and --sandbox are mutually exclusive in codex >=0.130 and
		// together leave the sandbox on; bwrap then fails under a unit with
		// RestrictNamespaces=true. Neither may come back.
		{"codex", "codex", []string{"exec", "--json", "--dangerously-bypass-approvals-and-sandbox"}, []string{"--full-auto", "--sandbox"}},
		{"opencode", "opencode", []string{"run", "--format", "json", "--auto"}, []string{"--model", "--variant"}},
		{"opencode:opencode/deepseek-v4-flash", "opencode", []string{"--model", "opencode/deepseek-v4-flash"}, nil},
		{"opencode:opencode/deepseek-v4-pro@high", "opencode", []string{"--model", "opencode/deepseek-v4-pro", "--variant", "high"}, nil},
		{"cursor", "agent", []string{"--model", "auto", "Follow the instructions in PROMPT.md exactly"}, nil},
		{"cursor:sonnet-4", "agent", []string{"--model", "sonnet-4"}, nil},
		{"pi", "pi", []string{"--mode", "rpc"}, []string{"--model", "--no-session", "--thinking", "--no-tools", "--no-skills"}},
		{"pi:anthropic/claude-sonnet-4-20250514", "pi", []string{"--mode", "rpc", "--model", "anthropic/claude-sonnet-4-20250514"}, []string{"--no-session", "--thinking", "--no-tools", "--no-skills"}},
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
	a := &Agent{Provider: "agy", IncludeDirs: []string{"/a", "/b"}}
	cmd, err := DefaultCommand(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Join(cmd.Args, " ")
	if !strings.Contains(argv, "--add-dir /a") || !strings.Contains(argv, "--add-dir /b") {
		t.Errorf("agy argv missing include dirs: %s", argv)
	}
}

func TestBuildCmdAddsTrustedWorkdir(t *testing.T) {
	tests := []struct {
		provider string
		want     string
	}{
		{"codex", "--add-dir . -"},
		{"agy", "--add-dir ."},
		{"gemini", "--add-dir ."},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			a := &Agent{Provider: tt.provider}
			cmd, err := a.buildCmd(context.Background(), t.TempDir(), "prompt", "")
			if err != nil {
				t.Fatal(err)
			}
			argv := strings.Join(cmd.Args, " ")
			if !strings.Contains(argv, tt.want) {
				t.Fatalf("argv missing trusted workdir args %q: %s", tt.want, argv)
			}
		})
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
		{"opencode:opencode/deepseek-v4-flash", "abc", []string{"-s", "abc"}},
		{"cursor", "abc", []string{"--resume", "abc"}},
		{"agy", "abc", []string{"--conversation", "abc"}},
		{"gemini:gemini-3.1-pro", "abc", []string{"--conversation", "abc"}},
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

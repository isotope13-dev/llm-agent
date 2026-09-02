// Command spec-probe verifies that "<provider>[:<model>][@<effort>]" specs are
// actually usable — the credentials work AND the model name resolves.
//
// It runs a real, bounded invocation rather than Agent.Probe. Probe waits for
// the CLI's first stdout byte and kills it, which is enough to prove the binary
// exists and nothing more: every provider emits session or init JSON before it
// authenticates or validates a model. In practice that made Probe report
// success for a nonexistent codex model, and — more expensively — it logged
// "llmagent probe ok" for claude immediately before every invocation failed with
// authentication_failed, so an expired login looked healthy while the premium
// tier silently degraded to its fallback for hours.
//
// A one-word completion costs a few cents and answers the question Probe cannot.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	llmagent "github.com/isotope13-dev/llm-agent"
)

// probePrompt is deliberately trivial: the goal is a completed round trip, not
// a useful answer. Asking for one token keeps the cost near the floor.
const probePrompt = "Reply with exactly one word: ok"

func main() {
	timeout := flag.Duration("timeout", 3*time.Minute, "per-spec timeout")
	verbose := flag.Bool("v", false, "log probe argv and provider events")
	flag.Parse()
	// main does nothing but map the exit code, so the deferred cleanup in run
	// still happens — an os.Exit down there would skip the workdir removal.
	os.Exit(run(flag.Args(), *timeout, *verbose))
}

func run(specs []string, timeout time.Duration, verbose bool) int {
	if len(specs) == 0 {
		fmt.Fprintln(os.Stderr, "usage: spec-probe [-timeout d] <provider[:model][@effort]>...")
		fmt.Fprintln(os.Stderr, "runs a real one-word completion per spec; verifies auth AND model name")
		return 2
	}

	level := slog.LevelError
	if verbose {
		level = slog.LevelInfo
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	workdir, err := os.MkdirTemp("", "spec-probe-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "tempdir:", err)
		return 1
	}
	defer os.RemoveAll(workdir) //nolint:errcheck // best-effort cleanup

	failed := false
	for _, spec := range specs {
		ok, detail, elapsed := runSpec(spec, workdir, timeout, logger)
		if ok {
			fmt.Printf("OK    %-36s base=%-7s model=%-22s effort=%-6s (%s)\n",
				spec, llmagent.Base(spec), llmagent.Model(spec), llmagent.Effort(spec), elapsed)
			continue
		}
		failed = true
		fmt.Printf("FAIL  %-36s %s (%s)\n", spec, detail, elapsed)
	}
	if failed {
		return 1
	}
	return 0
}

// runSpec reports whether the spec completed a round trip, the reason it did
// not, and how long the attempt took.
func runSpec(spec, workdir string, timeout time.Duration, logger *slog.Logger) (ok bool, detail string, elapsed time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	a := &llmagent.Agent{Provider: spec, Logger: logger}
	start := time.Now()
	res, err := a.Run(ctx, probePrompt, workdir, llmagent.RunOptions{Logger: logger})
	elapsed = time.Since(start).Round(time.Millisecond)

	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return false, fmt.Sprintf("timed out after %s", timeout), elapsed
	case err != nil:
		return false, firstLine(err.Error()), elapsed
	case res == nil || strings.TrimSpace(res.Output) == "":
		// A clean exit with no output is not success: the CLI can report a bad
		// model or a dead credential on stderr and still exit 0.
		return false, "completed with empty output (check credentials and model name)", elapsed
	}
	return true, "", elapsed
}

// firstLine trims a provider's error to something readable; the CLIs emit
// multi-kilobyte JSON blobs on failure.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

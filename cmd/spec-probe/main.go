// Command spec-probe verifies that "<provider>[:<model>][@<effort>]" specs are
// actually accepted by the provider CLIs. A bad model name parses fine and only
// fails at invocation, so the chains in cyclotron's config are unverifiable by
// inspection — this exercises each one for real.
//
// Probe starts the CLI, waits for its first stdout byte, and kills it, so the
// cost is a process start rather than a completion.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	llmagent "github.com/isotope13-dev/llm-agent"
)

func main() {
	timeout := flag.Duration("timeout", 90*time.Second, "per-spec probe timeout")
	verbose := flag.Bool("v", false, "log probe argv")
	flag.Parse()

	specs := flag.Args()
	if len(specs) == 0 {
		fmt.Fprintln(os.Stderr, "usage: spec-probe [-timeout d] <provider[:model][@effort]>...")
		os.Exit(2)
	}

	level := slog.LevelError
	if *verbose {
		level = slog.LevelInfo
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	worst := 0
	for _, spec := range specs {
		a := &llmagent.Agent{Provider: spec, Logger: logger, ProbeTimeout: *timeout}
		start := time.Now()
		err := a.Probe(context.Background())
		el := time.Since(start).Round(time.Millisecond)
		switch {
		case err == nil:
			fmt.Printf("OK    %-34s base=%-7s model=%-18s effort=%-6s (%s)\n",
				spec, llmagent.Base(spec), llmagent.Model(spec), llmagent.Effort(spec), el)
		default:
			worst = 1
			msg := err.Error()
			if len(msg) > 150 {
				msg = msg[:150] + "…"
			}
			fmt.Printf("FAIL  %-34s %s (%s)\n", spec, msg, el)
		}
	}
	os.Exit(worst)
}

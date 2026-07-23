// Command llm-diag probes local LLM CLIs and prints their availability.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"text/tabwriter"
	"time"

	llmagent "github.com/atomdrift-project/llm-agent"
)

type localProvider struct {
	name string
	bin  string
}

var localProviders = []localProvider{
	{name: "codex", bin: "codex"},
	{name: "opencode", bin: "opencode"},
	{name: "pi", bin: "pi"},
}

func main() {
	showAll := flag.Bool("all", false, "show supported local providers even when their executable is missing")
	verbose := flag.Bool("v", false, "log probe argv, pid, and HOME/XDG/TMPDIR context to stderr")
	timeout := flag.Duration("timeout", 0, "per-provider probe timeout; 0 uses llm-agent defaults")
	flag.Parse()

	logger := slog.New(slog.DiscardHandler)
	if *verbose {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}

	rows := probeLocalProviders(context.Background(), localProviders, *showAll, *timeout, logger)
	if err := printRows(os.Stdout, rows); err != nil {
		fmt.Fprintf(os.Stderr, "write diag output: %v\n", err)
		os.Exit(1)
	}
}

type row struct {
	provider string
	binary   string
	path     string
	status   string
	detail   string
}

func probeLocalProviders(ctx context.Context, providers []localProvider, showAll bool, timeout time.Duration, logger *slog.Logger) []row {
	rows := make([]row, 0, len(providers))
	for _, p := range providers {
		path, err := exec.LookPath(p.bin)
		if err != nil {
			if showAll {
				rows = append(rows, row{provider: p.name, binary: p.bin, status: "missing", detail: err.Error()})
			}
			continue
		}

		a := &llmagent.Agent{Provider: p.name, Logger: logger, ProbeTimeout: timeout}
		start := time.Now()
		err = a.Probe(ctx)
		elapsed := time.Since(start).Truncate(time.Millisecond)
		if err != nil {
			rows = append(rows, row{
				provider: p.name,
				binary:   p.bin,
				path:     path,
				status:   "fail",
				detail:   fmt.Sprintf("%s after %s", err, elapsed),
			})
			continue
		}
		rows = append(rows, row{provider: p.name, binary: p.bin, path: path, status: "ok", detail: elapsed.String()})
	}
	return rows
}

func printRows(w io.Writer, rows []row) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "PROVIDER\tBINARY\tSTATUS\tPATH\tDETAIL"); err != nil {
		return err
	}
	for _, r := range rows {
		_, err := fmt.Fprintf( //nolint:gosec // CLI table output, not HTML.
			tw, "%s\t%s\t%s\t%s\t%s\n",
			r.provider, r.binary, r.status, r.path, r.detail,
		)
		if err != nil {
			return err
		}
	}
	return tw.Flush()
}

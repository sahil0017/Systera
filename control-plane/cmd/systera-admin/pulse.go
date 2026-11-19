package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	ep "github.com/systera/control-plane/internal/engineeringpulse"
	"github.com/systera/control-plane/internal/knowledge"
	"github.com/systera/control-plane/internal/models"
	"github.com/systera/control-plane/internal/platform/config"
	"github.com/systera/control-plane/internal/platform/postgres"
)

// Engineering Pulse source-registry administration:
//
//	systera-admin pulse-sync     -file ../knowledge/source-registry/sources.json
//	systera-admin pulse-sources
//	systera-admin pulse-approve  -source pg-news -by "reviewer name"   (after reading the source's terms)
//	systera-admin pulse-suspend  -source pg-news [-retire] [-reason "..."]
//	systera-admin pulse-run      (runs one pipeline cycle and prints the report)
func pulseCommand(cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	file := fs.String("file", "../knowledge/source-registry/sources.json", "registry definition file (pulse-sync)")
	source := fs.String("source", "", "source_key")
	by := fs.String("by", "", "who reviewed the source's terms of use (pulse-approve)")
	retire := fs.Bool("retire", false, "retire instead of suspend (pulse-suspend)")
	reason := fs.String("reason", "", "reason (pulse-suspend)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	pool, err := postgres.Open(ctx, cfg.PostgresDSN())
	if err != nil {
		return err
	}
	defer pool.Close()
	ks := knowledge.NewStore(pool, models.NewDeterministicHashEmbedder(models.DefaultEmbeddingDimensions), "")
	store := ep.NewStore(pool, ks)
	print := func(v any) error {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}

	switch cmd {
	case "pulse-sync":
		defs, err := ep.LoadRegistryFile(*file)
		if err != nil {
			return err
		}
		res, err := store.SyncSources(ctx, defs)
		if err != nil {
			return err
		}
		return print(res)
	case "pulse-sources":
		srcs, err := store.ListSources(ctx, "")
		if err != nil {
			return err
		}
		for _, s := range srcs {
			fmt.Printf("%-28s %-15s %-15s rate=%d/h every>=%ds content=%s approved_by=%q\n  terms: %s\n  %s\n",
				s.Key, s.Status, s.Connector, s.RateLimitPerHour, s.MinIntervalSeconds, s.AllowedContent, s.ApprovedBy, s.TermsURL, s.TermsSummary)
		}
		return nil
	case "pulse-approve":
		if *source == "" || *by == "" {
			return fmt.Errorf("-source and -by are required")
		}
		src, err := store.ApproveSource(ctx, *source, *by)
		if err != nil {
			return err
		}
		return print(src)
	case "pulse-suspend":
		if *source == "" {
			return fmt.Errorf("-source is required")
		}
		status := ep.SourceSuspended
		if *retire {
			status = ep.SourceRetired
		}
		return store.SetSourceStatus(ctx, *source, status, *reason)
	case "pulse-run":
		p := &ep.Pipeline{Store: store, Fetcher: ep.NewFetcher(cfg.PulseUserAgent, cfg.PulseGitHubToken)}
		rep, err := p.RunCycle(ctx)
		if err != nil {
			return err
		}
		return print(rep)
	}
	return fmt.Errorf("unknown command %q", cmd)
}

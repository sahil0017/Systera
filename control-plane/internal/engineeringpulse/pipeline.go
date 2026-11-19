package engineeringpulse

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// PipelineConfig tunes one Engineering Pulse cycle.
type PipelineConfig struct {
	FetchBatch    int // sources claimed per cycle (default 10)
	ClassifyBatch int // items classified per cycle (default 500)
	Score         ScoreConfig
	Research      ResearchOptions
}

// Pipeline runs the Engineering Pulse stages in order (§2.2).
type Pipeline struct {
	Store      *Store
	Fetcher    *Fetcher
	Classifier Classifier // nil: rule classifier over the stored taxonomy
	Config     PipelineConfig
	Logger     *slog.Logger
}

// CycleReport summarizes one cycle.
type CycleReport struct {
	Fetches    []FetchOutcome    `json:"fetches"`
	Classified int               `json:"classified"`
	Scores     []TrendScore      `json:"scores"`
	Reports    []ReportOutcome   `json:"reports"`
	Published  []KnowledgeUpdate `json:"published"`
	Stale      int               `json:"stale"`
}

// ReportOutcome is one research result.
type ReportOutcome struct {
	ReportID     string   `json:"report_id"`
	TopicKey     string   `json:"topic_key"`
	Status       string   `json:"status"`
	FailedChecks []string `json:"failed_checks,omitempty"`
}

// RunCycle: connectors -> dedup -> classify -> observe -> score -> research -> validate ->
// publish & distribute -> stale sweep. A failing source does not stop the cycle.
func (p *Pipeline) RunCycle(ctx context.Context) (CycleReport, error) {
	log := p.Logger
	if log == nil {
		log = slog.Default()
	}
	rep := CycleReport{Fetches: []FetchOutcome{}, Scores: []TrendScore{}, Reports: []ReportOutcome{}, Published: []KnowledgeUpdate{}}
	claims, err := p.Store.ClaimDueSources(ctx, p.Config.FetchBatch)
	if err != nil {
		return rep, fmt.Errorf("claim sources: %w", err)
	}
	for _, c := range claims {
		res, ferr := p.Fetcher.Fetch(ctx, c.Source)
		out, err := p.Store.RecordFetch(ctx, c, res, ferr)
		if err != nil {
			return rep, fmt.Errorf("record fetch %s: %w", c.Source.Key, err)
		}
		if ferr != nil {
			log.Warn("pulse fetch failed", "source", c.Source.Key, "err", ferr)
		}
		rep.Fetches = append(rep.Fetches, out)
	}
	if rep.Classified, err = p.Store.ClassifyPending(ctx, p.Classifier, p.Config.ClassifyBatch); err != nil {
		return rep, fmt.Errorf("classify: %w", err)
	}
	if err := p.Store.RefreshObservations(ctx, p.Config.Score.ObservationHorizon(p.Store.now().UTC())); err != nil {
		return rep, fmt.Errorf("observations: %w", err)
	}
	if rep.Scores, err = p.Store.ComputeScores(ctx, p.Config.Score); err != nil {
		return rep, fmt.Errorf("score: %w", err)
	}
	reports, err := p.Store.Research(ctx, p.Config.Research)
	if err != nil {
		return rep, fmt.Errorf("research: %w", err)
	}
	for _, r := range reports {
		rep.Reports = append(rep.Reports, ReportOutcome{ReportID: r.ID, TopicKey: r.TopicKey, Status: r.Status, FailedChecks: r.Validation.Failed()})
		if r.Status != ReportValidated {
			continue
		}
		ku, err := p.Store.Publish(ctx, r.ID)
		if errors.Is(err, ErrNotPublishable) {
			log.Info("pulse report not published", "report", r.ID, "reason", err)
			continue
		}
		if err != nil {
			return rep, fmt.Errorf("publish %s: %w", r.TopicKey, err)
		}
		rep.Published = append(rep.Published, ku)
	}
	if rep.Stale, err = p.Store.SweepStale(ctx); err != nil {
		return rep, fmt.Errorf("stale sweep: %w", err)
	}
	return rep, nil
}

// Run executes cycles every interval until ctx ends.
func (p *Pipeline) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	log := p.Logger
	if log == nil {
		log = slog.Default()
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		rep, err := p.RunCycle(ctx)
		if err != nil && ctx.Err() == nil {
			log.Error("pulse cycle failed", "err", err)
		} else if err == nil {
			log.Info("pulse cycle", "fetches", len(rep.Fetches), "classified", rep.Classified, "scored", len(rep.Scores),
				"reports", len(rep.Reports), "published", len(rep.Published), "stale", rep.Stale)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

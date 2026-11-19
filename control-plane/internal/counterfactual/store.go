package counterfactual

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/systera/control-plane/internal/decisions"
	"github.com/systera/control-plane/internal/graph"
	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/postgres"
)

// Store persists counterfactual alternatives, materializes each alternative as a separate
// graph version, records ADRs with rejected alternatives, and computes alternative diffs.
type Store struct {
	pool      *pgxpool.Pool
	graphSvc  *graph.Service
	decisions *decisions.Store
}

func NewStore(pool *pgxpool.Pool, graphSvc *graph.Service, decStore *decisions.Store) *Store {
	return &Store{pool: pool, graphSvc: graphSvc, decisions: decStore}
}

func (s *Store) appTx(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	return postgres.WithTenantRoleTx(ctx, s.pool, tenantID, postgres.AppRole, fn)
}

// GenerateForModel generates >= 2 viable architectural alternatives for a canonical model,
// materializes each as a separate graph version (7.1), records an ADR with "when appropriate"
// and rejected alternatives (7.3), and persists the 8-constraint decision matrix (7.2).
func (s *Store) GenerateForModel(ctx context.Context, tenantID, modelID string, in GenerateInput, actorID string) (CounterfactualSet, error) {
	if !ids.Valid(modelID) {
		return CounterfactualSet{}, graph.ErrModelNotFound
	}
	return s.generate(ctx, tenantID, modelID, "", in, actorID)
}

// GenerateForWebsite generates >= 2 viable architectural alternatives for a website investigation
// (and materializes graph versions + ADR when the website is linked to a model).
func (s *Store) GenerateForWebsite(ctx context.Context, tenantID, websiteID, modelID, domain string, in GenerateInput, actorID string) (CounterfactualSet, error) {
	if !ids.Valid(websiteID) {
		return CounterfactualSet{}, ErrNotFound
	}
	if strings.TrimSpace(in.Subject) == "" {
		in.Subject = domain
	}
	return s.generate(ctx, tenantID, modelID, websiteID, in, actorID)
}

func (s *Store) generate(ctx context.Context, tenantID, modelID, websiteID string, in GenerateInput, actorID string) (CounterfactualSet, error) {
	set, err := GenerateCounterfactualSet(in.Subject, in.Styles, in.Constraints)
	if err != nil {
		return CounterfactualSet{}, err
	}
	set.ModelID = modelID
	set.WebsiteID = websiteID

	// 1. Materialize each alternative as its own graph version when linked to a model (7.1).
	if ids.Valid(modelID) && s.graphSvc != nil {
		for i := range set.Alternatives {
			cur, gErr := s.graphSvc.GraphAt(ctx, tenantID, modelID, -1)
			if gErr != nil {
				return CounterfactualSet{}, gErr
			}
			patch := BuildAlternativePatch(
				cur.Version.Number,
				set.Subject,
				set.Alternatives[i],
				fmt.Sprintf("cf-%s-%s", set.SetID[:8], set.Alternatives[i].Style),
			)
			res, pErr := s.graphSvc.ApplyPatch(ctx, tenantID, modelID, patch, graph.Author{Type: "USER", ID: actorID}, ids.New())
			if pErr == nil && res.Accepted && res.NewGraphVersion != nil {
				set.Alternatives[i].GraphVersion = *res.NewGraphVersion
			} else {
				set.Alternatives[i].GraphVersion = cur.Version.Number
			}
		}
	}

	// 2. Record an ADR capturing "when each option is appropriate" + rejected alternatives (7.3).
	recordADR := true
	if in.RecordADR != nil {
		recordADR = *in.RecordADR
	}
	if recordADR && ids.Valid(modelID) && s.decisions != nil && len(set.Alternatives) >= 2 {
		chosen := set.Alternatives[0]
		var rejected []decisions.Alternative
		var whenNotes []string
		whenNotes = append(whenNotes, fmt.Sprintf("- %s (recommended, %.2f/5): %s", chosen.Name, chosen.WeightedScore, chosen.WhenAppropriate))
		for _, alt := range set.Alternatives[1:] {
			whenNotes = append(whenNotes, fmt.Sprintf("- %s (%.2f/5): %s", alt.Name, alt.WeightedScore, alt.WhenAppropriate))
			rejected = append(rejected, decisions.Alternative{
				Option:          alt.Name,
				Pros:            alt.Pros,
				Cons:            alt.Cons,
				RejectionReason: fmt.Sprintf("Scored %.2f/5 vs %.2f/5 for %s. Appropriate when: %s", alt.WeightedScore, chosen.WeightedScore, chosen.Name, alt.WhenAppropriate),
			})
		}
		adr, dErr := s.decisions.Create(ctx, tenantID, modelID, decisions.CreateInput{
			ModelVersion:   chosen.GraphVersion,
			Title:          fmt.Sprintf("Architecture Style Selection: %s (%s)", set.Subject, chosen.Name),
			Context:        fmt.Sprintf("Counterfactual evaluation of %d architectural styles for %s across 8 constraints (scale, availability, latency, cost, team_size, compliance, operational_maturity, migration_complexity).", len(set.Alternatives), set.Subject),
			ChosenOption:   chosen.Name,
			Alternatives:   rejected,
			Rationale:      strings.Join(whenNotes, "\n"),
			TradeOffs:      fmt.Sprintf("Pros: %s | Cons: %s", chosen.Pros, chosen.Cons),
			Consequences:   fmt.Sprintf("Graph version %d represents the recommended %s baseline; rejected alternatives remain addressable as separate graph versions for diffing.", chosen.GraphVersion, chosen.Name),
			ConfidenceBand: "MEDIUM",
		}, decisions.Actor{Type: "USER", ID: actorID})
		if dErr == nil {
			set.DecisionID = adr.ID
			for i := range set.Alternatives {
				set.Alternatives[i].DecisionID = adr.ID
			}
		}
	}

	// 3. Persist all alternatives in architecture_alternatives.
	constraintsJSON, _ := json.Marshal(set.Constraints)
	err = s.appTx(ctx, tenantID, func(tx pgx.Tx) error {
		var modelPtr, websitePtr, decPtr *string
		if ids.Valid(modelID) {
			modelPtr = &modelID
		}
		if ids.Valid(websiteID) {
			websitePtr = &websiteID
		}
		if ids.Valid(set.DecisionID) {
			decPtr = &set.DecisionID
		}
		for i := range set.Alternatives {
			a := &set.Alternatives[i]
			a.TenantID = tenantID
			a.ModelID = modelID
			a.WebsiteID = websiteID
			scoresJSON, _ := json.Marshal(a.Scores)
			if err := tx.QueryRow(ctx, `
				INSERT INTO architecture_alternatives (
					id, tenant_id, model_id, website_id, set_id, style, name, summary,
					when_appropriate, pros, cons, graph_version, is_recommended,
					scores_json, weighted_score, constraints_json, decision_id
				) VALUES (
					$1, $2, $3, $4, $5, $6, $7, $8,
					$9, $10, $11, $12, $13,
					$14, $15, $16, $17
				)
				RETURNING created_at, updated_at`,
				a.ID, tenantID, modelPtr, websitePtr, set.SetID, string(a.Style), a.Name, a.Summary,
				a.WhenAppropriate, a.Pros, a.Cons, a.GraphVersion, a.IsRecommended,
				scoresJSON, a.WeightedScore, constraintsJSON, decPtr,
			).Scan(&a.CreatedAt, &a.UpdatedAt); err != nil {
				return err
			}
		}
		return nil
	})
	return set, err
}

// ListForModel returns the latest counterfactual set for an architecture model (7.5).
func (s *Store) ListForModel(ctx context.Context, tenantID, modelID string) (CounterfactualSet, error) {
	if !ids.Valid(modelID) {
		return CounterfactualSet{}, graph.ErrModelNotFound
	}
	return s.listLatest(ctx, tenantID, "model_id", modelID)
}

// ListForWebsite returns the latest counterfactual set for a website profile (7.5).
func (s *Store) ListForWebsite(ctx context.Context, tenantID, websiteID, domain string) (CounterfactualSet, error) {
	if !ids.Valid(websiteID) {
		return CounterfactualSet{}, ErrNotFound
	}
	set, err := s.listLatest(ctx, tenantID, "website_id", websiteID)
	if errors.Is(err, ErrNotFound) {
		// Derive a deterministic preview on the fly if none materialized yet.
		preview, pErr := GenerateCounterfactualSet(domain, DefaultStyles, ConstraintProfile{})
		if pErr != nil {
			return CounterfactualSet{}, pErr
		}
		preview.WebsiteID = websiteID
		return preview, nil
	}
	return set, err
}

func (s *Store) listLatest(ctx context.Context, tenantID, col, id string) (CounterfactualSet, error) {
	var alts []Alternative
	var profile ConstraintProfile
	err := s.appTx(ctx, tenantID, func(tx pgx.Tx) error {
		var latestSetID string
		err := tx.QueryRow(ctx, fmt.Sprintf(`
			SELECT set_id::text FROM architecture_alternatives
			WHERE tenant_id = $1 AND %s = $2
			ORDER BY created_at DESC LIMIT 1`, col), tenantID, id).Scan(&latestSetID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}

		rows, err := tx.Query(ctx, `
			SELECT id::text, tenant_id::text, COALESCE(model_id::text, ''), COALESCE(website_id::text, ''),
			       set_id::text, style, name, summary, when_appropriate, pros, cons,
			       graph_version, is_recommended, scores_json, weighted_score, constraints_json,
			       COALESCE(decision_id::text, ''), created_at, updated_at
			FROM architecture_alternatives
			WHERE tenant_id = $1 AND set_id = $2
			ORDER BY weighted_score DESC, style ASC`, tenantID, latestSetID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a Alternative
			var styleRaw string
			var scoresRaw, constraintsRaw []byte
			if err := rows.Scan(
				&a.ID, &a.TenantID, &a.ModelID, &a.WebsiteID,
				&a.SetID, &styleRaw, &a.Name, &a.Summary, &a.WhenAppropriate, &a.Pros, &a.Cons,
				&a.GraphVersion, &a.IsRecommended, &scoresRaw, &a.WeightedScore, &constraintsRaw,
				&a.DecisionID, &a.CreatedAt, &a.UpdatedAt,
			); err != nil {
				return err
			}
			a.Style = Style(styleRaw)
			_ = json.Unmarshal(scoresRaw, &a.Scores)
			_ = json.Unmarshal(constraintsRaw, &profile)
			alts = append(alts, a)
		}
		return rows.Err()
	})
	if err != nil {
		return CounterfactualSet{}, err
	}
	if len(alts) == 0 {
		return CounterfactualSet{}, ErrNotFound
	}
	return CounterfactualSet{
		SetID:          alts[0].SetID,
		ModelID:        alts[0].ModelID,
		WebsiteID:      alts[0].WebsiteID,
		Constraints:    profile,
		Alternatives:   alts,
		DecisionMatrix: BuildDecisionMatrix(alts, profile.NormalizedWeights()),
		DecisionID:     alts[0].DecisionID,
	}, nil
}

// DiffAlternatives diffs two materialized alternatives using the existing graph version diff (7.4).
func (s *Store) DiffAlternatives(ctx context.Context, tenantID, altAID, altBID string) (AlternativeDiff, error) {
	if !ids.Valid(altAID) || !ids.Valid(altBID) {
		return AlternativeDiff{}, ErrNotFound
	}
	var a, b Alternative
	err := s.appTx(ctx, tenantID, func(tx pgx.Tx) error {
		load := func(id string, dst *Alternative) error {
			var styleRaw string
			var scoresRaw []byte
			err := tx.QueryRow(ctx, `
				SELECT id::text, tenant_id::text, COALESCE(model_id::text, ''), COALESCE(website_id::text, ''),
				       set_id::text, style, name, summary, when_appropriate, pros, cons,
				       graph_version, is_recommended, scores_json, weighted_score,
				       COALESCE(decision_id::text, ''), created_at, updated_at
				FROM architecture_alternatives
				WHERE id = $1 AND tenant_id = $2`, id, tenantID).Scan(
				&dst.ID, &dst.TenantID, &dst.ModelID, &dst.WebsiteID,
				&dst.SetID, &styleRaw, &dst.Name, &dst.Summary, &dst.WhenAppropriate, &dst.Pros, &dst.Cons,
				&dst.GraphVersion, &dst.IsRecommended, &scoresRaw, &dst.WeightedScore,
				&dst.DecisionID, &dst.CreatedAt, &dst.UpdatedAt,
			)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
			dst.Style = Style(styleRaw)
			_ = json.Unmarshal(scoresRaw, &dst.Scores)
			return nil
		}
		if err := load(altAID, &a); err != nil {
			return err
		}
		return load(altBID, &b)
	})
	if err != nil {
		return AlternativeDiff{}, err
	}
	if !ids.Valid(a.ModelID) || a.ModelID != b.ModelID {
		return AlternativeDiff{}, ErrNoModel
	}
	gDiff, err := s.graphSvc.Diff(ctx, tenantID, a.ModelID, a.GraphVersion, b.GraphVersion)
	if err != nil {
		return AlternativeDiff{}, err
	}
	deltas := make(map[Constraint]float64, len(AllConstraints))
	for _, c := range AllConstraints {
		deltas[c] = b.Scores[c].Score - a.Scores[c].Score
	}
	return AlternativeDiff{
		ModelID:         a.ModelID,
		FromAlternative: a,
		ToAlternative:   b,
		ScoreDeltas:     deltas,
		WeightedDelta:   b.WeightedScore - a.WeightedScore,
		GraphDiff:       gDiff,
	}, nil
}

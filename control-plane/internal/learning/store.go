package learning

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	ep "github.com/systera/control-plane/internal/engineeringpulse"
	"github.com/systera/control-plane/internal/graph"
	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/postgres"
)

// GraphReader is the part of graph.Service Learning Mode reads.
type GraphReader interface {
	InspectComponent(ctx context.Context, tenantID, modelID string, number int, nodeID string) (graph.ComponentInspection, error)
	GraphAt(ctx context.Context, tenantID, modelID string, number int) (graph.Graph, error)
}

// PatternRetriever retrieves validated Engineering Pulse knowledge (implemented by
// *engineeringpulse.Retriever).
type PatternRetriever interface {
	Patterns(ctx context.Context, tenantID string, q ep.PatternQuery) ([]ep.PatternReference, error)
}

// Store serves lessons and records challenges.
type Store struct {
	pool     *pgxpool.Pool
	graph    GraphReader
	patterns PatternRetriever
}

// NewStore returns a Store. patterns may be nil (lessons then report research UNAVAILABLE).
func NewStore(pool *pgxpool.Pool, g GraphReader, patterns PatternRetriever) *Store {
	return &Store{pool: pool, graph: g, patterns: patterns}
}

// Overview returns the model-level learning overview for a graph version (-1 = current).
func (s *Store) Overview(ctx context.Context, tenantID, modelID string, version int) (Overview, error) {
	if !ids.Valid(modelID) {
		return Overview{}, graph.ErrModelNotFound
	}
	g, err := s.graph.GraphAt(ctx, tenantID, modelID, version)
	if err != nil {
		return Overview{}, err
	}
	return BuildOverview(g), nil
}

// Lesson returns the lesson for one component, with research links and past challenges.
func (s *Store) Lesson(ctx context.Context, tenantID, modelID string, version int, nodeID string) (Lesson, error) {
	insp, err := s.graph.InspectComponent(ctx, tenantID, modelID, version, nodeID)
	if err != nil {
		return Lesson{}, err
	}
	research, status := s.research(ctx, tenantID, insp)
	l := BuildLesson(insp, research, status)
	if l.Challenges, err = s.ListChallenges(ctx, tenantID, modelID, nodeID); err != nil {
		return Lesson{}, err
	}
	return l, nil
}

func (s *Store) research(ctx context.Context, tenantID string, insp graph.ComponentInspection) ([]ResearchLink, string) {
	if s.patterns == nil {
		return []ResearchLink{}, "UNAVAILABLE"
	}
	n := insp.Node.Node
	terms := []string{n.Type}
	for _, a := range insp.Alternatives {
		terms = append(terms, a.Option)
	}
	for _, it := range insp.Interfaces {
		terms = append(terms, it.Target.Type, it.Protocol)
	}
	refs, err := s.patterns.Patterns(ctx, tenantID, ep.PatternQuery{Query: strings.TrimSpace(n.Name + " " + n.Type), ContextTerms: dedupe(terms), Limit: 3})
	if err != nil {
		return []ResearchLink{}, "UNAVAILABLE"
	}
	out := ToResearchLinks(refs)
	if len(out) == 0 {
		return out, "NONE_FOUND"
	}
	return out, "FOUND"
}

// ToResearchLinks converts Pulse pattern references into lesson research links.
func ToResearchLinks(refs []ep.PatternReference) []ResearchLink {
	out := make([]ResearchLink, 0, len(refs))
	for _, r := range refs {
		srcs := make([]ResearchSource, 0, len(r.Explanation.SupportingSources))
		for _, ss := range r.Explanation.SupportingSources {
			srcs = append(srcs, ResearchSource{Title: ss.Title, URL: ss.URL, Source: ss.SourceName, IsPrimary: ss.IsPrimarySource})
		}
		out = append(out, ResearchLink{
			ProvenanceRef: r.ProvenanceRef, TopicKey: r.TopicKey, Title: r.Title, Why: r.Explanation.Why,
			ConfidenceBand: r.Explanation.ConfidenceBand, Maturity: r.Explanation.Maturity, Status: r.Explanation.Status,
			Limitations: r.Explanation.Limitations, Sources: srcs,
			AdoptionNote: "Popularity is shown for context only and is never a reason to choose a technology.",
		})
	}
	return out
}

func dedupe(xs []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, x := range xs {
		x = strings.TrimSpace(x)
		if x != "" && !seen[strings.ToLower(x)] {
			seen[strings.ToLower(x)] = true
			out = append(out, x)
		}
	}
	return out
}

// Challenge records a user challenge on a component and returns the revised explanation.
func (s *Store) Challenge(ctx context.Context, tenantID, modelID string, version int, nodeID, text, actorID string) (Challenge, error) {
	t, err := NormalizeChallenge(text)
	if err != nil {
		return Challenge{}, err
	}
	l, err := s.Lesson(ctx, tenantID, modelID, version, nodeID)
	if err != nil {
		return Challenge{}, err
	}
	c := Challenge{ID: ids.New(), ModelID: modelID, NodeID: nodeID, GraphVersion: l.GraphVersion, Text: t,
		Revised: ReviseExplanation(l, t), CreatedBy: actorID}
	resp, _ := json.Marshal(c.Revised)
	err = postgres.WithTenantRoleTx(ctx, s.pool, tenantID, postgres.AppRole, func(tx pgx.Tx) error {
		// Serialise revisions per component.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "learning:"+modelID+":"+nodeID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(revision), 0) + 1 FROM learning_challenges WHERE model_id = $1 AND node_id = $2`,
			modelID, nodeID).Scan(&c.Revision); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO learning_challenges (id, tenant_id, model_id, node_id, graph_version, revision, challenge, concern, response_json, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING created_at`,
			c.ID, tenantID, modelID, nodeID, c.GraphVersion, c.Revision, t, c.Revised.Concern, resp, actorID).Scan(&c.CreatedAt)
	})
	if err != nil {
		return Challenge{}, fmt.Errorf("record challenge: %w", err)
	}
	return c, nil
}

// ListChallenges returns a component's challenges, newest first.
func (s *Store) ListChallenges(ctx context.Context, tenantID, modelID, nodeID string) ([]Challenge, error) {
	out := []Challenge{}
	if !ids.Valid(modelID) || !ids.Valid(nodeID) {
		return out, nil
	}
	err := postgres.WithTenantRoleTx(ctx, s.pool, tenantID, postgres.AppRole, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id::text, model_id::text, node_id::text, graph_version, revision, challenge, response_json, created_by, created_at
			FROM learning_challenges WHERE model_id = $1 AND node_id = $2 ORDER BY revision DESC LIMIT 50`, modelID, nodeID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c Challenge
			var raw []byte
			if err := rows.Scan(&c.ID, &c.ModelID, &c.NodeID, &c.GraphVersion, &c.Revision, &c.Text, &raw, &c.CreatedBy, &c.CreatedAt); err != nil {
				return err
			}
			_ = json.Unmarshal(raw, &c.Revised)
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

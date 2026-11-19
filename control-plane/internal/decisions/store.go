package decisions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/outbox"
	"github.com/systera/control-plane/internal/platform/postgres"
)

// Store persists and manages ADRs in PostgreSQL with tenant RLS and outbox events.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) appTx(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	return postgres.WithTenantRoleTx(ctx, s.pool, tenantID, postgres.AppRole, fn)
}

// Create proposes a new ADR (initial status PROPOSED) on the model.
func (s *Store) Create(ctx context.Context, tenantID, modelID string, in CreateInput, actor Actor) (Decision, error) {
	if !ids.Valid(modelID) {
		return Decision{}, ErrModelNotFound
	}
	if err := ValidateCreate(&in); err != nil {
		return Decision{}, err
	}
	if actor.Type == "" {
		actor.Type = "USER"
	}
	if actor.ID == "" {
		return Decision{}, fmt.Errorf("%w: owner_id is required", ErrInvalidInput)
	}

	var out Decision
	err := s.appTx(ctx, tenantID, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM architecture_models WHERE id = $1 AND tenant_id = $2)`, modelID, tenantID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrModelNotFound
		}

		var nextNum int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(adr_number), 0) + 1 FROM decisions WHERE model_id = $1`, modelID).Scan(&nextNum); err != nil {
			return err
		}

		altJSON, err := json.Marshal(in.Alternatives)
		if err != nil {
			return err
		}
		evJSON, err := json.Marshal(in.EvidenceRefs)
		if err != nil {
			return err
		}

		var runPtr *string
		if ids.Valid(actor.RunID) {
			runPtr = &actor.RunID
		}

		id := ids.New()
		err = tx.QueryRow(ctx, `
			INSERT INTO decisions (
				id, tenant_id, model_id, model_version, adr_number, title, context,
				chosen_option, alternatives_json, rationale, trade_offs, consequences,
				evidence_refs, confidence_band, owner_type, owner_id, affected_nodes,
				status, run_id
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7,
				$8, $9, $10, $11, $12,
				$13, $14, $15, $16, $17::uuid[],
				'PROPOSED', $18
			)
			RETURNING created_at, updated_at`,
			id, tenantID, modelID, in.ModelVersion, nextNum, in.Title, in.Context,
			in.ChosenOption, altJSON, in.Rationale, in.TradeOffs, in.Consequences,
			evJSON, in.ConfidenceBand, actor.Type, actor.ID, in.AffectedNodes,
			runPtr,
		).Scan(&out.CreatedAt, &out.UpdatedAt)
		if err != nil {
			return err
		}

		out.ID = id
		out.TenantID = tenantID
		out.ModelID = modelID
		out.ModelVersion = in.ModelVersion
		out.ADRNumber = nextNum
		out.Title = in.Title
		out.Context = in.Context
		out.ChosenOption = in.ChosenOption
		out.Alternatives = in.Alternatives
		out.Rationale = in.Rationale
		out.TradeOffs = in.TradeOffs
		out.Consequences = in.Consequences
		out.EvidenceRefs = in.EvidenceRefs
		out.ConfidenceBand = in.ConfidenceBand
		out.OwnerType = actor.Type
		out.OwnerID = actor.ID
		out.AffectedNodes = in.AffectedNodes
		out.Status = StatusProposed
		out.RunID = actor.RunID

		_, err = outbox.InsertTx(ctx, tx, outbox.EventEnvelope{
			EventType:     EventDecisionCreated,
			TenantID:      tenantID,
			RunID:         actor.RunID,
			CorrelationID: ids.New(),
			Payload: map[string]any{
				"decision_id": out.ID,
				"model_id":    modelID,
				"adr_number":  out.ADRNumber,
				"title":       out.Title,
				"status":      out.Status,
			},
		})
		return err
	})
	return out, err
}

// Get retrieves a single ADR by ID within the tenant and model.
func (s *Store) Get(ctx context.Context, tenantID, modelID, id string) (Decision, error) {
	if !ids.Valid(modelID) || !ids.Valid(id) {
		return Decision{}, ErrNotFound
	}
	var d Decision
	err := s.appTx(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, decisionSelectSQL+` WHERE id = $1 AND model_id = $2 AND tenant_id = $3`, id, modelID, tenantID)
		var err error
		d, err = scanDecision(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return d, err
}

// List returns ADRs for a model, optionally filtered by status.
func (s *Store) List(ctx context.Context, tenantID, modelID string, status Status) ([]Decision, error) {
	if !ids.Valid(modelID) {
		return nil, ErrModelNotFound
	}
	var out []Decision
	err := s.appTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, decisionSelectSQL+`
			WHERE model_id = $1 AND tenant_id = $2 AND ($3 = '' OR status = $3)
			ORDER BY adr_number ASC`, modelID, tenantID, string(status))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			d, err := scanDecision(rows)
			if err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	if out == nil {
		out = []Decision{}
	}
	return out, err
}

// Transition moves an ADR to a new state if permitted by CanTransition.
func (s *Store) Transition(ctx context.Context, tenantID, modelID, id string, in TransitionInput, actor Actor) (Decision, error) {
	if !ids.Valid(modelID) || !ids.Valid(id) {
		return Decision{}, ErrNotFound
	}
	if !ValidStatus(in.Status) {
		return Decision{}, fmt.Errorf("%w: unknown status %q", ErrInvalidInput, in.Status)
	}

	var out Decision
	err := s.appTx(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, decisionSelectSQL+` WHERE id = $1 AND model_id = $2 AND tenant_id = $3 FOR UPDATE`, id, modelID, tenantID)
		cur, err := scanDecision(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if !CanTransition(cur.Status, in.Status) {
			return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, cur.Status, in.Status)
		}

		err = tx.QueryRow(ctx, `
			UPDATE decisions
			SET status = $1, review_reason = $2, updated_at = NOW()
			WHERE id = $3 AND tenant_id = $4
			RETURNING updated_at`, string(in.Status), in.Reason, id, tenantID).Scan(&cur.UpdatedAt)
		if err != nil {
			return err
		}
		cur.Status = in.Status
		cur.ReviewReason = in.Reason
		out = cur

		_, err = outbox.InsertTx(ctx, tx, outbox.EventEnvelope{
			EventType:     EventDecisionStatusChanged,
			TenantID:      tenantID,
			RunID:         actor.RunID,
			CorrelationID: ids.New(),
			Payload: map[string]any{
				"decision_id": out.ID,
				"model_id":    modelID,
				"status":      out.Status,
				"reason":      in.Reason,
			},
		})
		return err
	})
	return out, err
}

// Supersede atomically creates a replacement ADR linked via supersedes/superseded_by
// and marks the old ADR SUPERSEDED.
func (s *Store) Supersede(ctx context.Context, tenantID, modelID, oldID string, in CreateInput, actor Actor) (oldDec Decision, newDec Decision, err error) {
	if !ids.Valid(modelID) || !ids.Valid(oldID) {
		return Decision{}, Decision{}, ErrNotFound
	}
	if err := ValidateCreate(&in); err != nil {
		return Decision{}, Decision{}, err
	}
	if actor.Type == "" {
		actor.Type = "USER"
	}
	if actor.ID == "" {
		return Decision{}, Decision{}, fmt.Errorf("%w: owner_id is required", ErrInvalidInput)
	}

	err = s.appTx(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, decisionSelectSQL+` WHERE id = $1 AND model_id = $2 AND tenant_id = $3 FOR UPDATE`, oldID, modelID, tenantID)
		cur, err := scanDecision(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if !CanTransition(cur.Status, StatusSuperseded) {
			return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, cur.Status, StatusSuperseded)
		}

		var nextNum int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(adr_number), 0) + 1 FROM decisions WHERE model_id = $1`, modelID).Scan(&nextNum); err != nil {
			return err
		}

		altJSON, err := json.Marshal(in.Alternatives)
		if err != nil {
			return err
		}
		evJSON, err := json.Marshal(in.EvidenceRefs)
		if err != nil {
			return err
		}
		var runPtr *string
		if ids.Valid(actor.RunID) {
			runPtr = &actor.RunID
		}

		newID := ids.New()
		var createdAt, updatedAt time.Time
		err = tx.QueryRow(ctx, `
			INSERT INTO decisions (
				id, tenant_id, model_id, model_version, adr_number, title, context,
				chosen_option, alternatives_json, rationale, trade_offs, consequences,
				evidence_refs, confidence_band, owner_type, owner_id, affected_nodes,
				status, supersedes, run_id
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7,
				$8, $9, $10, $11, $12,
				$13, $14, $15, $16, $17::uuid[],
				'PROPOSED', $18, $19
			)
			RETURNING created_at, updated_at`,
			newID, tenantID, modelID, in.ModelVersion, nextNum, in.Title, in.Context,
			in.ChosenOption, altJSON, in.Rationale, in.TradeOffs, in.Consequences,
			evJSON, in.ConfidenceBand, actor.Type, actor.ID, in.AffectedNodes,
			oldID, runPtr,
		).Scan(&createdAt, &updatedAt)
		if err != nil {
			return err
		}

		err = tx.QueryRow(ctx, `
			UPDATE decisions
			SET status = 'SUPERSEDED', superseded_by = $1, updated_at = NOW()
			WHERE id = $2 AND tenant_id = $3
			RETURNING updated_at`, newID, oldID, tenantID).Scan(&cur.UpdatedAt)
		if err != nil {
			return err
		}
		cur.Status = StatusSuperseded
		cur.SupersededBy = newID
		oldDec = cur

		newDec = Decision{
			ID:             newID,
			TenantID:       tenantID,
			ModelID:        modelID,
			ModelVersion:   in.ModelVersion,
			ADRNumber:      nextNum,
			Title:          in.Title,
			Context:        in.Context,
			ChosenOption:   in.ChosenOption,
			Alternatives:   in.Alternatives,
			Rationale:      in.Rationale,
			TradeOffs:      in.TradeOffs,
			Consequences:   in.Consequences,
			EvidenceRefs:   in.EvidenceRefs,
			ConfidenceBand: in.ConfidenceBand,
			OwnerType:      actor.Type,
			OwnerID:        actor.ID,
			AffectedNodes:  in.AffectedNodes,
			Status:         StatusProposed,
			Supersedes:     oldID,
			RunID:          actor.RunID,
			CreatedAt:      createdAt,
			UpdatedAt:      updatedAt,
		}

		_, err = outbox.InsertTx(ctx, tx, outbox.EventEnvelope{
			EventType:     EventDecisionSuperseded,
			TenantID:      tenantID,
			RunID:         actor.RunID,
			CorrelationID: ids.New(),
			Payload: map[string]any{
				"old_decision_id": oldID,
				"new_decision_id": newID,
				"model_id":        modelID,
			},
		})
		return err
	})
	return oldDec, newDec, err
}

// MarkNeedsReviewForNodes marks any PROPOSED or ACCEPTED ADR whose affected_nodes
// intersects changedNodeIDs as NEEDS_REVIEW (Checklist 5.3).
func (s *Store) MarkNeedsReviewForNodes(ctx context.Context, tenantID, modelID string, changedNodeIDs []string, reason string) ([]string, error) {
	if !ids.Valid(modelID) || len(changedNodeIDs) == 0 {
		return nil, nil
	}
	var flagged []string
	err := s.appTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			UPDATE decisions
			SET status = 'NEEDS_REVIEW', review_reason = $1, updated_at = NOW()
			WHERE model_id = $2
			  AND tenant_id = $3
			  AND status IN ('PROPOSED', 'ACCEPTED')
			  AND affected_nodes && $4::uuid[]
			RETURNING id::text`, reason, modelID, tenantID, changedNodeIDs)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			flagged = append(flagged, id)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(flagged) > 0 {
			_, err = outbox.InsertTx(ctx, tx, outbox.EventEnvelope{
				EventType:     EventDecisionNeedsReview,
				TenantID:      tenantID,
				CorrelationID: ids.New(),
				Payload: map[string]any{
					"model_id":         modelID,
					"decision_ids":     flagged,
					"changed_node_ids": changedNodeIDs,
					"reason":           reason,
				},
			})
			return err
		}
		return nil
	})
	return flagged, err
}

const decisionSelectSQL = `
	SELECT id::text, tenant_id::text, model_id::text, model_version, adr_number,
	       title, context, chosen_option, alternatives_json, rationale, trade_offs,
	       consequences, evidence_refs, confidence_band, owner_type, owner_id,
	       COALESCE(affected_nodes::text[], '{}'), status,
	       COALESCE(supersedes::text, ''), COALESCE(superseded_by::text, ''),
	       review_reason, COALESCE(run_id::text, ''), created_at, updated_at
	FROM decisions`

type scanner interface {
	Scan(dest ...any) error
}

func scanDecision(row scanner) (Decision, error) {
	var d Decision
	var altRaw, evRaw []byte
	var statusStr string
	err := row.Scan(
		&d.ID, &d.TenantID, &d.ModelID, &d.ModelVersion, &d.ADRNumber,
		&d.Title, &d.Context, &d.ChosenOption, &altRaw, &d.Rationale, &d.TradeOffs,
		&d.Consequences, &evRaw, &d.ConfidenceBand, &d.OwnerType, &d.OwnerID,
		&d.AffectedNodes, &statusStr,
		&d.Supersedes, &d.SupersededBy,
		&d.ReviewReason, &d.RunID, &d.CreatedAt, &d.UpdatedAt,
	)
	if err != nil {
		return Decision{}, err
	}
	d.Status = Status(statusStr)
	_ = json.Unmarshal(altRaw, &d.Alternatives)
	_ = json.Unmarshal(evRaw, &d.EvidenceRefs)
	if d.Alternatives == nil {
		d.Alternatives = []Alternative{}
	}
	if d.EvidenceRefs == nil {
		d.EvidenceRefs = []string{}
	}
	if d.AffectedNodes == nil {
		d.AffectedNodes = []string{}
	}
	return d, nil
}

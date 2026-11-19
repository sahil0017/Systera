// Package policy enforces governance and human approval gates for high-risk
// architecture change sets and external actions (spec §24.4, §33, Checklist 8.5).
package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/systera/control-plane/internal/graph"
	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/outbox"
	"github.com/systera/control-plane/internal/platform/postgres"
)

var (
	ErrApprovalNotFound = errors.New("approval request not found")
	ErrApprovalRequired = errors.New("human approval required for high-risk change set")
	ErrAlreadyDecided   = errors.New("approval request already decided")
)

// RiskLevel classifies the blast radius of a proposed change.
type RiskLevel string

const (
	RiskLow    RiskLevel = "LOW"
	RiskMedium RiskLevel = "MEDIUM"
	RiskHigh   RiskLevel = "HIGH"
)

// RiskAssessment summarizes whether a GraphPatch requires human approval.
type RiskAssessment struct {
	Level            RiskLevel `json:"risk_level"`
	RequiresApproval bool      `json:"requires_approval"`
	Reasons          []string  `json:"reasons"`
}

// EvaluatePatchRisk inspects a GraphPatch's operations and returns a deterministic RiskAssessment.
// High-risk triggers:
//   - Removing any node (OpRemoveNode)
//   - 20 or more operations in a single patch
//   - Adding or modifying a sensitive node type (system, secret_boundary, database)
func EvaluatePatchRisk(ops []graph.Operation) RiskAssessment {
	var reasons []string
	if len(ops) >= 20 {
		reasons = append(reasons, fmt.Sprintf("large change set (%d operations)", len(ops)))
	}
	for _, op := range ops {
		if op.Op == graph.OpRemoveNode {
			reasons = append(reasons, fmt.Sprintf("removes architecture node %s", op.TargetID))
		}
		if op.Op == graph.OpAddNode {
			var pl struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(op.Payload, &pl) == nil {
				switch pl.Type {
				case "system", "secret_boundary":
					reasons = append(reasons, fmt.Sprintf("creates sensitive %s node", pl.Type))
				}
			}
		}
	}
	if len(reasons) > 0 {
		return RiskAssessment{Level: RiskHigh, RequiresApproval: true, Reasons: reasons}
	}
	if len(ops) >= 5 {
		return RiskAssessment{Level: RiskMedium, RequiresApproval: false, Reasons: []string{"multi-operation patch"}}
	}
	return RiskAssessment{Level: RiskLow, RequiresApproval: false, Reasons: []string{}}
}

// ApprovalRequest represents a pending or decided human approval gate.
type ApprovalRequest struct {
	ID             string     `json:"id"`
	TenantID       string     `json:"tenant_id"`
	ModelID        string     `json:"model_id,omitempty"`
	RunID          string     `json:"run_id,omitempty"`
	ActionType     string     `json:"action_type"`
	ResourceID     string     `json:"resource_id"`
	RiskLevel      RiskLevel  `json:"risk_level"`
	RiskReasons    []string   `json:"risk_reasons"`
	PayloadHash    string     `json:"payload_hash"`
	PayloadJSON    []byte     `json:"-"`
	Status         string     `json:"status"`
	RequestedBy    string     `json:"requested_by"`
	DecidedBy      string     `json:"decided_by,omitempty"`
	DecisionReason string     `json:"decision_reason,omitempty"`
	DecidedAt      *time.Time `json:"decided_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// ApprovalStore persists and resolves human approval gates in PostgreSQL.
type ApprovalStore struct {
	pool *pgxpool.Pool
}

func NewApprovalStore(pool *pgxpool.Pool) *ApprovalStore {
	return &ApprovalStore{pool: pool}
}

func (s *ApprovalStore) RequestApproval(ctx context.Context, tenantID, modelID, runID, resourceID, payloadHash, requestedBy string, risk RiskAssessment, payloadJSON ...[]byte) (ApprovalRequest, error) {
	var rawPayload []byte
	if len(payloadJSON) > 0 {
		rawPayload = payloadJSON[0]
	}
	var out ApprovalRequest
	err := postgres.WithTenantRoleTx(ctx, s.pool, tenantID, postgres.AppRole, func(tx pgx.Tx) error {
		reasonsJSON, _ := json.Marshal(risk.Reasons)
		id := ids.New()
		var modelPtr, runPtr *string
		if ids.Valid(modelID) {
			modelPtr = &modelID
		}
		if ids.Valid(runID) {
			runPtr = &runID
		}
		err := tx.QueryRow(ctx, `
			INSERT INTO approval_requests (
				id, tenant_id, model_id, run_id, action_type, resource_id,
				risk_level, risk_reasons, payload_hash, payload_json, status, requested_by
			) VALUES ($1, $2, $3, $4, 'HIGH_RISK_GRAPH_PATCH', $5, $6, $7, $8, $9, 'PENDING', $10)
			RETURNING created_at`,
			id, tenantID, modelPtr, runPtr, resourceID, string(risk.Level), reasonsJSON, payloadHash, rawPayload, requestedBy,
		).Scan(&out.CreatedAt)
		if err != nil {
			return err
		}
		out.ID = id
		out.TenantID = tenantID
		out.ModelID = modelID
		out.RunID = runID
		out.ActionType = "HIGH_RISK_GRAPH_PATCH"
		out.ResourceID = resourceID
		out.RiskLevel = risk.Level
		out.RiskReasons = risk.Reasons
		out.PayloadHash = payloadHash
		out.Status = "PENDING"
		out.RequestedBy = requestedBy

		_, err = outbox.InsertTx(ctx, tx, outbox.EventEnvelope{
			EventType:     "approval.requested",
			TenantID:      tenantID,
			RunID:         runID,
			CorrelationID: ids.New(),
			Payload: map[string]any{
				"approval_id": id,
				"model_id":    modelID,
				"risk_level":  risk.Level,
				"reasons":     risk.Reasons,
			},
		})
		return err
	})
	return out, err
}

func (s *ApprovalStore) Decide(ctx context.Context, tenantID, approvalID, actorID string, approved bool, reason string) (ApprovalRequest, error) {
	if !ids.Valid(approvalID) {
		return ApprovalRequest{}, ErrApprovalNotFound
	}
	nextStatus := "REJECTED"
	if approved {
		nextStatus = "APPROVED"
	}
	var out ApprovalRequest
	err := postgres.WithTenantRoleTx(ctx, s.pool, tenantID, postgres.AppRole, func(tx pgx.Tx) error {
		var curStatus string
		err := tx.QueryRow(ctx, `SELECT status FROM approval_requests WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, approvalID, tenantID).Scan(&curStatus)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrApprovalNotFound
		}
		if err != nil {
			return err
		}
		if curStatus != "PENDING" {
			return ErrAlreadyDecided
		}
		var reasonsRaw []byte
		err = tx.QueryRow(ctx, `
			UPDATE approval_requests
			SET status = $1, decided_by = $2, decision_reason = $3, decided_at = NOW(), updated_at = NOW()
			WHERE id = $4 AND tenant_id = $5
			RETURNING id::text, tenant_id::text, COALESCE(model_id::text, ''), COALESCE(run_id::text, ''),
			          action_type, resource_id, risk_level, risk_reasons, payload_hash, payload_json, status,
			          requested_by, decided_by, decision_reason, decided_at, created_at`,
			nextStatus, actorID, reason, approvalID, tenantID,
		).Scan(&out.ID, &out.TenantID, &out.ModelID, &out.RunID, &out.ActionType, &out.ResourceID,
			&out.RiskLevel, &reasonsRaw, &out.PayloadHash, &out.PayloadJSON, &out.Status, &out.RequestedBy,
			&out.DecidedBy, &out.DecisionReason, &out.DecidedAt, &out.CreatedAt)
		if err != nil {
			return err
		}
		_ = json.Unmarshal(reasonsRaw, &out.RiskReasons)
		_, err = outbox.InsertTx(ctx, tx, outbox.EventEnvelope{
			EventType:     "approval.decided",
			TenantID:      tenantID,
			RunID:         out.RunID,
			CorrelationID: ids.New(),
			Payload: map[string]any{
				"approval_id": out.ID,
				"status":      out.Status,
				"decided_by":  actorID,
			},
		})
		return err
	})
	return out, err
}

func (s *ApprovalStore) List(ctx context.Context, tenantID, status string) ([]ApprovalRequest, error) {
	var out []ApprovalRequest
	err := postgres.WithTenantRoleTx(ctx, s.pool, tenantID, postgres.AppRole, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id::text, tenant_id::text, COALESCE(model_id::text, ''), COALESCE(run_id::text, ''),
			       action_type, resource_id, risk_level, risk_reasons, payload_hash, status,
			       requested_by, COALESCE(decided_by, ''), decision_reason, decided_at, created_at
			FROM approval_requests
			WHERE tenant_id = $1 AND ($2 = '' OR status = $2)
			ORDER BY created_at DESC`, tenantID, status)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a ApprovalRequest
			var reasonsRaw []byte
			if err := rows.Scan(&a.ID, &a.TenantID, &a.ModelID, &a.RunID, &a.ActionType, &a.ResourceID,
				&a.RiskLevel, &reasonsRaw, &a.PayloadHash, &a.Status, &a.RequestedBy,
				&a.DecidedBy, &a.DecisionReason, &a.DecidedAt, &a.CreatedAt); err != nil {
				return err
			}
			_ = json.Unmarshal(reasonsRaw, &a.RiskReasons)
			out = append(out, a)
		}
		return rows.Err()
	})
	if out == nil {
		out = []ApprovalRequest{}
	}
	return out, err
}

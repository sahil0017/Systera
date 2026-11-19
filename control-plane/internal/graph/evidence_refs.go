package graph

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/systera/control-plane/internal/platform/ids"
)

// EvidenceRefPrefix marks a GraphPatch evidence_ref that points into the evidence ledger
// ("evidence:<uuid>"). Other refs (e.g. "req:FR-1", URLs) are free-form and not checked.
const EvidenceRefPrefix = "evidence:"

// ledgerRefFindings validates ledger references in a patch: each must name evidence of
// this model that is ACTIVE and unexpired at the time the patch is applied.
func ledgerRefFindings(ctx context.Context, tx pgx.Tx, modelID string, ops []Operation) ([]Finding, error) {
	type ref struct {
		op int
		id string
	}
	var refs []ref
	var lookup []string
	var fs []Finding
	for i, op := range ops {
		for _, r := range op.EvidenceRefs {
			if !strings.HasPrefix(r, EvidenceRefPrefix) {
				continue
			}
			id := strings.TrimPrefix(r, EvidenceRefPrefix)
			if !ids.Valid(id) {
				fs = append(fs, Finding{OpIndex: i, TargetID: op.TargetID, Code: "invalid_evidence_ref", Severity: "ERROR",
					Message: fmt.Sprintf("%q is not evidence:<uuid>", r)})
				continue
			}
			refs = append(refs, ref{i, id})
			lookup = append(lookup, id)
		}
	}
	if len(lookup) == 0 {
		return fs, nil
	}
	rows, err := tx.Query(ctx, `SELECT id::text, status, (expires_at IS NOT NULL AND expires_at <= NOW())
		FROM evidence_records WHERE model_id = $1 AND id = ANY($2::uuid[])`, modelID, lookup)
	if err != nil {
		return nil, err
	}
	type state struct {
		status  string
		expired bool
	}
	found := map[string]state{}
	for rows.Next() {
		var id string
		var st state
		if err := rows.Scan(&id, &st.status, &st.expired); err != nil {
			rows.Close()
			return nil, err
		}
		found[id] = st
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, r := range refs {
		st, ok := found[r.id]
		target := ops[r.op].TargetID
		switch {
		case !ok:
			fs = append(fs, Finding{OpIndex: r.op, TargetID: target, Code: "unknown_evidence", Severity: "ERROR",
				Invariant: "claim_evidence_requirements", Message: fmt.Sprintf("evidence %s does not exist in this model", r.id)})
		case st.status != "ACTIVE" || st.expired:
			status := st.status
			if st.expired && status == "ACTIVE" {
				status = "EXPIRED"
			}
			fs = append(fs, Finding{OpIndex: r.op, TargetID: target, Code: "evidence_not_usable", Severity: "ERROR",
				Invariant: "claim_evidence_requirements", Message: fmt.Sprintf("evidence %s is %s", r.id, status)})
		}
	}
	return fs, nil
}

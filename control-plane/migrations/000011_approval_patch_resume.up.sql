-- Stage 2 (Seam 2): Store paused high-risk GraphPatch payload on approval_requests
-- so approving an approval request can automatically resume and apply the patch.

ALTER TABLE approval_requests
    ADD COLUMN IF NOT EXISTS payload_json JSONB;

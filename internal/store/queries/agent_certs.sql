-- name: LookupAgentCertByFingerprint :one
-- Backs the agentMTLS middleware's identity-binding step. Resolves a
-- SHA-256 fingerprint (32 raw bytes) to its bound node and revocation
-- status. The middleware enforces revocation policy in Go; this query
-- returns the row as recorded.
select node_id, revoked_at
from agent_certs
where fingerprint_sha256 = @fingerprint_sha256;

-- name: CreateAgentCert :one
-- Inserts agent cert metadata after Step 2 redemption signs а CSR.
-- The cert PEM itself is NOT stored — returned к the agent в the
-- redemption response, agent persists locally on disk. CP keeps only
-- the metadata (serial, fingerprint, subject DN, validity) for
-- AgentMTLS fingerprint lookup и future revocation tracking. The
-- caller passes serial as DER big-endian bytes (big.Int.Bytes() of
-- the cert's SerialNumber).
insert into agent_certs (
    id, node_id, serial, fingerprint_sha256,
    subject_dn, not_before, not_after
)
values (
    @id, @node_id, @serial, @fingerprint_sha256,
    @subject_dn, @not_before, @not_after
)
returning *;

-- name: NodeHasActiveCert :one
-- Used by the Step 2 redemption handler к detect node-name conflicts
-- before signing а fresh cert. "Active" means revoked_at IS NULL —
-- а revoked cert does NOT block reuse of the node row (the operator
-- already invalidated it, fresh bootstrap is the recovery path).
-- Returns false когда either no cert row exists for the node OR all
-- existing certs have been revoked.
select exists(
    select 1
    from agent_certs
    where node_id = @node_id
      and revoked_at is null
)::boolean;

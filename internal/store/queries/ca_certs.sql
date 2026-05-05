-- name: GetActiveCACert :one
-- Returns the single active cluster CA row. /v1/ca handler uses this
-- to project к the public ClusterCA view (cert_pem + fingerprint
-- only — key_pem stays server-side). Step 2 CSR signing path uses
-- this к load the signer keypair into memory at request time.
select *
from ca_certs
where active = true;

-- name: CreateCACert :one
-- Idempotent at the row level via the uq_ca_certs_active partial
-- unique index — а second active row insert returns 23505, which the
-- CA bootstrap hook traps к fetch the winner race-
-- safety semantics.
insert into ca_certs (
    id, cert_pem, key_pem, fingerprint_sha256,
    not_before, not_after, active
)
values (
    @id, @cert_pem, @key_pem, @fingerprint_sha256,
    @not_before, @not_after, true
)
returning *;

-- name: DeactivateCACerts :exec
-- Future rotation step (not invoked в Step 1). Marks every active row
-- inactive so а new active row can be inserted under the partial
-- unique index. Held в Step 1 как Step 2/3 dependency.
update ca_certs set active = false where active = true;

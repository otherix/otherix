-- name: GetActiveCACert :one
-- Returns the single active cluster CA row. /v1/ca handler uses this
-- to project to the public ClusterCA view (cert_pem + fingerprint
-- only — key_pem stays server-side). Step 2 CSR signing path uses
-- this to load the signer keypair into memory at request time.
select *
from ca_certs
where active = true;

-- name: CreateCACert :one
-- Idempotent at the row level via the uq_ca_certs_active partial
-- unique index — a second active row insert returns 23505, which the
-- CA bootstrap hook traps to fetch the winner race-
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
-- Future rotation step (not invoked in Step 1). Marks every active row
-- inactive so a new active row can be inserted under the partial
-- unique index. Held in Step 1 as Step 2/3 dependency.
update ca_certs set active = false where active = true;

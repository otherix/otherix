-- Cluster-wide advisory locks. Keys are dispensed by internal/store/locks.go
-- so callers reference а typed constant rather than а raw integer. All
-- locks below are transaction-scoped (pg_advisory_xact_lock) — callers
-- MUST acquire them inside an InTxWithTx callback и MUST NOT use them
-- outside а transaction (autocommit acquires + releases on the same
-- statement, which serializes nothing). See locks.go for the namespace
-- contract.

-- name: AcquirePlacementLock :exec
-- Serializes VM placement decisions cluster-wide. Acquired inside the
-- vm.create transaction before SchedulePlacement reads candidate state;
-- released automatically on transaction commit/rollback. Caller passes
-- store.LockKeyPlacement as @lock_key.
select pg_advisory_xact_lock(@lock_key::bigint);

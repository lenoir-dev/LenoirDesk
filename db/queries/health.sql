-- name: CountPendingJobs :one
SELECT count(*) FROM planning_jobs WHERE tenant_id = sqlc.arg(tenant_id) AND status='pending';

-- name: ActiveRevision :one
SELECT id FROM automation_revisions
WHERE tenant_id=sqlc.arg(tenant_id) AND connection_id=sqlc.arg(connection_id)
  AND status='active' AND activated_at<=now();

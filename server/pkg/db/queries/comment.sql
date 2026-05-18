-- name: ListCommentsForIssue :many
-- All comments for an issue in chronological order, capped at $3 (DB safety
-- net). Issue p99 is ~30 comments, max ever observed in prod is ~1.1k, so
-- the handler-side cap of 2000 is purely defensive.
SELECT * FROM comment
WHERE issue_id = $1 AND workspace_id = $2
ORDER BY created_at ASC, id ASC
LIMIT $3;

-- name: ListCommentsSinceForIssue :many
-- Comments created strictly after $3 in chronological order, capped at $4.
-- Powers the CLI's `--since` agent-polling flow.
SELECT * FROM comment
WHERE issue_id = $1 AND workspace_id = $2 AND created_at > $3
ORDER BY created_at ASC, id ASC
LIMIT $4;

-- name: ListThreadCommentsForIssue :many
-- Returns the root of the thread containing @anchor_id plus every descendant
-- (recursive — defends against any future deeper nesting; today's data is two
-- layers because the CreateComment path collapses replies to root, but the
-- schema does not enforce that). @anchor_id may itself be a root or a reply.
-- Output is chronological so it can be fed straight to the agent.
WITH RECURSIVE root_of AS (
    -- Walk up from the anchor until parent_id IS NULL.
    SELECT c.id, c.parent_id
    FROM comment c
    WHERE c.id = @anchor_id AND c.issue_id = @issue_id AND c.workspace_id = @workspace_id
    UNION ALL
    SELECT p.id, p.parent_id
    FROM comment p
    JOIN root_of r ON p.id = r.parent_id
),
thread_root AS (
    SELECT id FROM root_of WHERE parent_id IS NULL LIMIT 1
),
descendants AS (
    -- Start from the root, then keep adding any comment whose parent is
    -- already in the set. Cycle-safe under PK constraint (a comment cannot
    -- be its own ancestor).
    SELECT c.id, c.issue_id, c.author_type, c.author_id, c.content, c.type,
           c.created_at, c.updated_at, c.parent_id, c.workspace_id,
           c.resolved_at, c.resolved_by_type, c.resolved_by_id
    FROM comment c
    JOIN thread_root tr ON c.id = tr.id
    UNION
    SELECT c.id, c.issue_id, c.author_type, c.author_id, c.content, c.type,
           c.created_at, c.updated_at, c.parent_id, c.workspace_id,
           c.resolved_at, c.resolved_by_type, c.resolved_by_id
    FROM comment c
    JOIN descendants d ON c.parent_id = d.id
    WHERE c.issue_id = @issue_id AND c.workspace_id = @workspace_id
)
SELECT id, issue_id, author_type, author_id, content, type,
       created_at, updated_at, parent_id, workspace_id,
       resolved_at, resolved_by_type, resolved_by_id
FROM descendants
ORDER BY created_at ASC, id ASC
LIMIT @row_limit;

-- name: ListRecentCommentsForIssue :many
-- Returns the most recent N comments for an issue, optionally bounded above
-- by a (created_at, id) cursor. The composite cursor avoids the
-- same-timestamp duplicate/skip risk that plain `created_at < $x` has under
-- the existing (created_at ASC, id ASC) ordering. Pass @has_cursor = FALSE
-- and the cursor params are ignored (returns the absolute newest N).
SELECT id, issue_id, author_type, author_id, content, type,
       created_at, updated_at, parent_id, workspace_id,
       resolved_at, resolved_by_type, resolved_by_id
FROM comment
WHERE issue_id = @issue_id
  AND workspace_id = @workspace_id
  AND (
      @has_cursor::boolean = FALSE
      OR (created_at, id) < (@before_created_at::timestamptz, @before_id::uuid)
  )
ORDER BY created_at DESC, id DESC
LIMIT @row_limit;

-- name: CountComments :one
SELECT count(*) FROM comment
WHERE issue_id = $1 AND workspace_id = $2;

-- name: GetComment :one
SELECT * FROM comment
WHERE id = $1;

-- name: GetCommentInWorkspace :one
SELECT * FROM comment
WHERE id = $1 AND workspace_id = $2;

-- name: CreateComment :one
INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type, parent_id)
VALUES ($1, $2, $3, $4, $5, $6, sqlc.narg(parent_id))
RETURNING *;

-- name: UpdateComment :one
UPDATE comment SET
    content = $2,
    updated_at = now()
WHERE id = $1
RETURNING *;

-- name: HasAgentCommentedSince :one
SELECT EXISTS (
    SELECT 1 FROM comment
    WHERE issue_id = @issue_id
      AND author_type = 'agent'
      AND author_id = @author_id
      AND created_at >= @since
) AS commented;

-- name: HasAgentRepliedInThread :one
-- Returns true if the given agent has posted a reply in the thread rooted at
-- the specified parent comment. Used to detect agent participation in a
-- member-started thread so that follow-up member replies still trigger the agent.
SELECT count(*) > 0 AS has_replied FROM comment
WHERE parent_id = @parent_id AND author_type = 'agent' AND author_id = @agent_id;

-- name: DeleteComment :exec
DELETE FROM comment WHERE id = $1;

-- name: ResolveComment :one
-- Idempotent: re-resolving keeps the original resolved_at + resolver. Always
-- returns the row so the handler can surface the canonical state.
UPDATE comment SET
    resolved_at = COALESCE(resolved_at, now()),
    resolved_by_type = COALESCE(resolved_by_type, $2),
    resolved_by_id = COALESCE(resolved_by_id, $3),
    updated_at = CASE WHEN resolved_at IS NULL THEN now() ELSE updated_at END
WHERE id = $1
RETURNING *;

-- name: UnresolveComment :one
-- Idempotent: a no-op clear (already unresolved) just returns the row.
UPDATE comment SET
    resolved_at = NULL,
    resolved_by_type = NULL,
    resolved_by_id = NULL,
    updated_at = CASE WHEN resolved_at IS NOT NULL THEN now() ELSE updated_at END
WHERE id = $1
RETURNING *;

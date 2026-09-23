-- name: CreateUser :exec
INSERT INTO users(id, email) VALUES ($1, $2);

-- name: CreateAccount :exec
INSERT INTO accounts(id, name) VALUES ($1, $2);

-- name: AddAccountOwner :exec
INSERT INTO account_members(account_id, user_id, role) VALUES ($1, $2, 'owner');

-- name: FindAccountOwner :one
SELECT user_id FROM account_members
WHERE account_id=$1 AND role='owner'
ORDER BY user_id LIMIT 1;

-- name: CreateAPIKey :exec
INSERT INTO api_keys(id, account_id, created_by_user_id, name, token_hash)
VALUES ($1, $2, $3, $4, $5);

-- name: RevokeAPIKey :execrows
UPDATE api_keys SET revoked_at=now()
WHERE id=$1 AND revoked_at IS NULL;

-- name: GetActiveAPIKey :one
SELECT account_id, id, token_hash FROM api_keys
WHERE id=$1 AND revoked_at IS NULL;

-- name: BeginUsage :exec
INSERT INTO usage_events(request_id, account_id, key_id, model, price_id)
VALUES ($1, $2, $3, $4,
    (SELECT id FROM model_prices WHERE model=$4
     ORDER BY created_at DESC, id DESC LIMIT 1));

-- name: FinishUsage :execrows
UPDATE usage_events AS e SET
    backend=sqlc.arg(backend),
    http_status=sqlc.arg(http_status),
    state=sqlc.arg(state),
    prompt_tokens=sqlc.narg(prompt_tokens)::bigint,
    completion_tokens=sqlc.narg(completion_tokens)::bigint,
    finished_at=now(),
    estimated_cost_micro=CASE WHEN sqlc.arg(state)='complete' THEN (
        SELECT round((sqlc.narg(prompt_tokens)::numeric * p.input_rate_micro_per_million +
                      sqlc.narg(completion_tokens)::numeric * p.output_rate_micro_per_million) / 1000000)::bigint
        FROM model_prices AS p WHERE p.id=e.price_id
    ) ELSE NULL END
WHERE e.request_id=sqlc.arg(request_id) AND e.state='pending';

-- name: SetModelPrice :exec
INSERT INTO model_prices(id, model, input_rate_micro_per_million, output_rate_micro_per_million)
VALUES ($1, $2, $3, $4);

-- name: GetDailyUsage :many
SELECT day, model, key_id, request_count, complete_count, incomplete_count,
       failed_count, prompt_tokens, completion_tokens, estimated_cost_micro, priced_count
FROM usage_daily WHERE account_id=$1 AND day >= $2
ORDER BY day, model, key_id;

-- name: ExpirePendingUsage :exec
UPDATE usage_events SET state='incomplete', finished_at=now()
WHERE state='pending' AND started_at < now() - interval '7 days';

-- name: RollupUsageBatch :execrows
WITH claimed AS (
    UPDATE usage_events SET rolled_up_at=now()
    WHERE request_id IN (
        SELECT request_id FROM usage_events
        WHERE rolled_up_at IS NULL AND state <> 'pending'
        ORDER BY started_at LIMIT $1 FOR UPDATE SKIP LOCKED
    )
    RETURNING started_at::date AS day, account_id, key_id, model,
              state, prompt_tokens, completion_tokens, estimated_cost_micro
), totals AS (
    SELECT day, account_id, key_id, model,
           count(*) AS request_count,
           count(*) FILTER (WHERE state='complete') AS complete_count,
           count(*) FILTER (WHERE state='incomplete') AS incomplete_count,
           count(*) FILTER (WHERE state='failed') AS failed_count,
           COALESCE(sum(prompt_tokens), 0) AS prompt_tokens,
           COALESCE(sum(completion_tokens), 0) AS completion_tokens,
           COALESCE(sum(estimated_cost_micro), 0) AS estimated_cost_micro,
           count(*) FILTER (WHERE estimated_cost_micro IS NOT NULL) AS priced_count
    FROM claimed GROUP BY day, account_id, key_id, model
)
INSERT INTO usage_daily(day, account_id, key_id, model, request_count,
                        complete_count, incomplete_count, failed_count,
                        prompt_tokens, completion_tokens, estimated_cost_micro,
                        priced_count)
SELECT day, account_id, key_id, model, request_count, complete_count,
       incomplete_count, failed_count, prompt_tokens, completion_tokens,
       estimated_cost_micro, priced_count FROM totals
ON CONFLICT (day, account_id, key_id, model) DO UPDATE SET
    request_count=usage_daily.request_count + EXCLUDED.request_count,
    complete_count=usage_daily.complete_count + EXCLUDED.complete_count,
    incomplete_count=usage_daily.incomplete_count + EXCLUDED.incomplete_count,
    failed_count=usage_daily.failed_count + EXCLUDED.failed_count,
    prompt_tokens=usage_daily.prompt_tokens + EXCLUDED.prompt_tokens,
    completion_tokens=usage_daily.completion_tokens + EXCLUDED.completion_tokens,
    estimated_cost_micro=usage_daily.estimated_cost_micro + EXCLUDED.estimated_cost_micro,
    priced_count=usage_daily.priced_count + EXCLUDED.priced_count;

-- name: DeleteOldUsageBatch :execrows
DELETE FROM usage_events WHERE request_id IN (
    SELECT old.request_id FROM usage_events AS old
    WHERE old.rolled_up_at IS NOT NULL AND old.started_at < $1
    ORDER BY old.started_at LIMIT $2
);

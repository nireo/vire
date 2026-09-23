package accountdb

import (
	"context"
	"time"
)

const (
	usageRetention  = 7 * 24 * time.Hour
	rollupBatchSize = 1000
	deleteBatchSize = 1000
)

// Maintain rolls terminal requests into durable daily totals and removes old
// detailed rows. Claiming rows and incrementing totals happen in one SQL
// transaction: concurrent gateway replicas cannot count a request twice.
func (s *Store) Maintain(ctx context.Context) error {
	// A gateway that crashes mid-request leaves a pending row. A request older
	// than the raw-event retention window cannot still be metered reliably.
	if _, err := s.pool.Exec(ctx, `
        UPDATE usage_events SET state='incomplete', finished_at=now()
        WHERE state='pending' AND started_at < now() - interval '7 days'`); err != nil {
		return err
	}
	for {
		result, err := s.pool.Exec(ctx, `
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
				priced_count=usage_daily.priced_count + EXCLUDED.priced_count`,
			rollupBatchSize)
		if err != nil {
			return err
		}
		if result.RowsAffected() == 0 {
			break
		}
	}
	for {
		result, err := s.pool.Exec(ctx, `
            DELETE FROM usage_events WHERE request_id IN (
                SELECT request_id FROM usage_events
                WHERE rolled_up_at IS NOT NULL AND started_at < $1
                ORDER BY started_at LIMIT $2
            )`, time.Now().Add(-usageRetention), deleteBatchSize)
		if err != nil {
			return err
		}
		if result.RowsAffected() == 0 {
			return nil
		}
	}
}

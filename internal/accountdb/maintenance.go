package accountdb

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nireo/vire/internal/accountdb/db"
)

const (
	usageRetention  = 7 * 24 * time.Hour
	rollupBatchSize = 1000
	deleteBatchSize = 1000
)

// Maintain rolls terminal requests into durable daily totals and removes old
// detailed rows. Claiming rows and incrementing totals happen in one SQL
// statement: concurrent gateway replicas cannot count a request twice.
func (s *Store) Maintain(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM portal_sessions WHERE expires_at < now()`); err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM login_attempts WHERE window_started < now() - interval '15 minutes'`); err != nil {
		return err
	}
	// A gateway that crashes mid-request leaves a pending row. A request older
	// than the raw-event retention window cannot still be metered reliably.
	if err := s.queries.ExpirePendingUsage(ctx); err != nil {
		return err
	}
	for {
		rows, err := s.queries.RollupUsageBatch(ctx, rollupBatchSize)
		if err != nil {
			return err
		}
		if rows == 0 {
			break
		}
	}
	for {
		rows, err := s.queries.DeleteOldUsageBatch(ctx, db.DeleteOldUsageBatchParams{
			StartedAt: pgtype.Timestamptz{Time: time.Now().Add(-usageRetention), Valid: true},
			Limit:     deleteBatchSize,
		})
		if err != nil {
			return err
		}
		if rows == 0 {
			return nil
		}
	}
}

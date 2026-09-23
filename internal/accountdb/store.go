package accountdb

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nireo/vire/gateway"
)

//go:embed schema.sql schema_v2.sql
var schemaFS embed.FS

type Store struct {
	pool *pgxpool.Pool
}

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}
	config.MaxConns = 10
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect database: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(824587105215)`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)`); err != nil {
		return err
	}
	var version int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return err
	}
	if version > 2 {
		return fmt.Errorf("database schema version %d is newer than this gateway", version)
	}
	if version == 0 {
		schema, err := schemaFS.ReadFile("schema.sql")
		if err != nil {
			return err
		}
		for _, statement := range strings.Split(string(schema), ";") {
			if strings.TrimSpace(statement) == "" {
				continue
			}
			if _, err := tx.Exec(ctx, statement); err != nil {
				return fmt.Errorf("apply schema: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES (1)`); err != nil {
			return err
		}
		version = 1
	}
	if version == 1 {
		schema, err := schemaFS.ReadFile("schema_v2.sql")
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(schema)); err != nil {
			return fmt.Errorf("apply schema v2: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES (2)`); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// CreateAccount creates the first user as the account owner. A UI can later use
// an external identity provider and attach its subject to this user record.
func (s *Store) CreateAccount(ctx context.Context, email, name string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	name = strings.TrimSpace(name)
	if email == "" || !strings.Contains(email, "@") || name == "" {
		return "", errors.New("a valid email and nonempty account name are required")
	}
	userID, accountID := rand.Text(), rand.Text()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO users(id, email) VALUES ($1, $2)`, userID, email); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO accounts(id, name) VALUES ($1, $2)`, accountID, name); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO account_members(account_id, user_id, role) VALUES ($1, $2, 'owner')`, accountID, userID); err != nil {
		return "", err
	}
	return accountID, tx.Commit(ctx)
}

// IssueKey returns the secret once. Only its SHA-256 digest is retained; the
// random 256-bit secret makes a fast digest safe against offline guessing.
func (s *Store) IssueKey(ctx context.Context, accountID, name string) (string, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", errors.New("key name is required")
	}
	var ownerID string
	if err := s.pool.QueryRow(ctx, `SELECT user_id FROM account_members WHERE account_id=$1 AND role='owner' ORDER BY user_id LIMIT 1`, accountID).Scan(&ownerID); err != nil {
		return "", "", fmt.Errorf("find account owner: %w", err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", "", err
	}
	keyID := rand.Text()
	token := "vire_" + keyID + "_" + base64.RawURLEncoding.EncodeToString(secret)
	digest := sha256.Sum256([]byte(token))
	_, err := s.pool.Exec(ctx, `INSERT INTO api_keys(id, account_id, created_by_user_id, name, token_hash) VALUES ($1, $2, $3, $4, $5)`, keyID, accountID, ownerID, name, digest[:])
	if err != nil {
		return "", "", err
	}
	return keyID, token, nil
}

func (s *Store) RevokeKey(ctx context.Context, keyID string) error {
	result, err := s.pool.Exec(ctx, `UPDATE api_keys SET revoked_at=now() WHERE id=$1 AND revoked_at IS NULL`, keyID)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return errors.New("active key not found")
	}
	return nil
}

func (s *Store) Authenticate(ctx context.Context, token string) (gateway.KeyIdentity, error) {
	if len(token) > 128 || !strings.HasPrefix(token, "vire_") {
		return gateway.KeyIdentity{}, gateway.ErrInvalidKey
	}
	keyID, secret, ok := strings.Cut(strings.TrimPrefix(token, "vire_"), "_")
	decoded, err := base64.RawURLEncoding.DecodeString(secret)
	if !ok || keyID == "" || err != nil || len(decoded) != 32 {
		return gateway.KeyIdentity{}, gateway.ErrInvalidKey
	}
	var identity gateway.KeyIdentity
	var stored []byte
	err = s.pool.QueryRow(ctx, `SELECT account_id, id, token_hash FROM api_keys WHERE id=$1 AND revoked_at IS NULL`, keyID).Scan(&identity.AccountID, &identity.KeyID, &stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return gateway.KeyIdentity{}, gateway.ErrInvalidKey
	}
	if err != nil {
		return gateway.KeyIdentity{}, err
	}
	digest := sha256.Sum256([]byte(token))
	if subtle.ConstantTimeCompare(digest[:], stored) != 1 {
		return gateway.KeyIdentity{}, gateway.ErrInvalidKey
	}
	return identity, nil
}

func (s *Store) BeginUsage(ctx context.Context, start gateway.UsageStart) error {
	_, err := s.pool.Exec(ctx, `
        INSERT INTO usage_events(request_id, account_id, key_id, model, price_id)
        VALUES ($1, $2, $3, $4,
            (SELECT id FROM model_prices WHERE model=$4 ORDER BY created_at DESC, id DESC LIMIT 1))`,
		start.RequestID, start.AccountID, start.KeyID, start.Model)
	return err
}

func (s *Store) FinishUsage(ctx context.Context, finish gateway.UsageFinish) error {
	if finish.Outcome != "complete" && finish.Outcome != "incomplete" && finish.Outcome != "failed" {
		return errors.New("invalid usage outcome")
	}
	if finish.Outcome == "complete" && (finish.PromptTokens == nil || finish.CompletionTokens == nil) {
		return errors.New("complete usage requires token counts")
	}
	result, err := s.pool.Exec(ctx, `
        UPDATE usage_events AS e SET
            backend=$2, http_status=$3, state=$4,
            prompt_tokens=$5::bigint, completion_tokens=$6::bigint, finished_at=now(),
            estimated_cost_micro=CASE WHEN $4='complete' THEN (
                SELECT round(($5::numeric * p.input_rate_micro_per_million +
                              $6::numeric * p.output_rate_micro_per_million) / 1000000)::bigint
                FROM model_prices AS p WHERE p.id=e.price_id
            ) ELSE NULL END
        WHERE e.request_id=$1 AND e.state='pending'`,
		finish.RequestID, finish.Backend, finish.Status, finish.Outcome,
		finish.PromptTokens, finish.CompletionTokens)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return errors.New("pending usage request not found")
	}
	return nil
}

func (s *Store) SetPrice(ctx context.Context, model string, inputRate, outputRate int64) (string, error) {
	model = strings.TrimSpace(model)
	if model == "" || inputRate < 0 || outputRate < 0 {
		return "", errors.New("model and nonnegative rates are required")
	}
	id := rand.Text()
	_, err := s.pool.Exec(ctx, `INSERT INTO model_prices(id, model, input_rate_micro_per_million, output_rate_micro_per_million) VALUES ($1, $2, $3, $4)`, id, model, inputRate, outputRate)
	return id, err
}

type DailyUsage struct {
	Day                time.Time `json:"day"`
	Model              string    `json:"model"`
	KeyID              string    `json:"key_id"`
	Requests           int64     `json:"requests"`
	Complete           int64     `json:"complete"`
	Incomplete         int64     `json:"incomplete"`
	Failed             int64     `json:"failed"`
	PromptTokens       int64     `json:"prompt_tokens"`
	CompletionTokens   int64     `json:"completion_tokens"`
	EstimatedCostMicro int64     `json:"estimated_cost_micro"`
	Priced             int64     `json:"priced"`
}

func (s *Store) DailyUsage(ctx context.Context, accountID string, since time.Time) ([]DailyUsage, error) {
	rows, err := s.pool.Query(ctx, `
        SELECT day, model, key_id, request_count, complete_count, incomplete_count,
               failed_count, prompt_tokens, completion_tokens, estimated_cost_micro, priced_count
        FROM usage_daily WHERE account_id=$1 AND day >= $2
        ORDER BY day, model, key_id`, accountID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	usage := make([]DailyUsage, 0)
	for rows.Next() {
		var row DailyUsage
		if err := rows.Scan(&row.Day, &row.Model, &row.KeyID, &row.Requests,
			&row.Complete, &row.Incomplete, &row.Failed, &row.PromptTokens,
			&row.CompletionTokens, &row.EstimatedCostMicro, &row.Priced); err != nil {
			return nil, err
		}
		usage = append(usage, row)
	}
	return usage, rows.Err()
}

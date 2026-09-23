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
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nireo/vire/gateway"
	"github.com/nireo/vire/internal/accountdb/db"
)

//go:embed schema.sql schema_v2.sql schema_v3.sql
var schemaFS embed.FS

type Store struct {
	pool    *pgxpool.Pool
	queries *db.Queries
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
	return &Store{pool: pool, queries: db.New(pool)}, nil
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
	if version > 3 {
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
		version = 2
	}
	if version == 2 {
		schema, err := schemaFS.ReadFile("schema_v3.sql")
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(schema)); err != nil {
			return fmt.Errorf("apply schema v3: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES (3)`); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Signup creates the account and its first API key in one transaction. The
// plaintext key is returned once and never stored.
func (s *Store) Signup(ctx context.Context, email, name, password string) (gateway.SignupResult, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	name = strings.TrimSpace(name)
	if email == "" || !strings.Contains(email, "@") || name == "" {
		return gateway.SignupResult{}, errors.New("a valid email and nonempty account name are required")
	}
	passwordHash, err := hashPassword(password)
	if err != nil {
		return gateway.SignupResult{}, err
	}
	sessionToken, sessionHash, err := newSessionToken()
	if err != nil {
		return gateway.SignupResult{}, err
	}
	userID, accountID, keyID := rand.Text(), rand.Text(), rand.Text()
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return gateway.SignupResult{}, err
	}
	token := "vire_" + keyID + "_" + base64.RawURLEncoding.EncodeToString(secret)
	digest := sha256.Sum256([]byte(token))
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return gateway.SignupResult{}, err
	}
	defer tx.Rollback(ctx)
	queries := s.queries.WithTx(tx)
	if err := queries.CreateUser(ctx, db.CreateUserParams{ID: userID, Email: email}); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "users_email_key" {
			return gateway.SignupResult{}, gateway.ErrEmailTaken
		}
		return gateway.SignupResult{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO user_passwords(user_id, password_hash) VALUES ($1, $2)`, userID, passwordHash); err != nil {
		return gateway.SignupResult{}, err
	}
	if err := queries.CreateAccount(ctx, db.CreateAccountParams{ID: accountID, Name: name}); err != nil {
		return gateway.SignupResult{}, err
	}
	if err := queries.AddAccountOwner(ctx, db.AddAccountOwnerParams{AccountID: accountID, UserID: userID}); err != nil {
		return gateway.SignupResult{}, err
	}
	if err := queries.CreateAPIKey(ctx, db.CreateAPIKeyParams{
		ID: keyID, AccountID: accountID, CreatedByUserID: userID, Name: "Default", TokenHash: digest[:],
	}); err != nil {
		return gateway.SignupResult{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO portal_sessions(token_hash, user_id, account_id, expires_at) VALUES ($1,$2,$3,now() + interval '7 days')`, sessionHash, userID, accountID); err != nil {
		return gateway.SignupResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return gateway.SignupResult{}, err
	}
	return gateway.SignupResult{AccountID: accountID, KeyID: keyID, APIKey: token, SessionToken: sessionToken}, nil
}

// IssueKey returns the secret once. Only its SHA-256 digest is retained; the
// random 256-bit secret makes a fast digest safe against offline guessing.
func (s *Store) IssueKey(ctx context.Context, accountID, name string) (string, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", errors.New("key name is required")
	}
	ownerID, err := s.queries.FindAccountOwner(ctx, accountID)
	if err != nil {
		return "", "", fmt.Errorf("find account owner: %w", err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", "", err
	}
	keyID := rand.Text()
	token := "vire_" + keyID + "_" + base64.RawURLEncoding.EncodeToString(secret)
	digest := sha256.Sum256([]byte(token))
	err = s.queries.CreateAPIKey(ctx, db.CreateAPIKeyParams{
		ID: keyID, AccountID: accountID, CreatedByUserID: ownerID, Name: name, TokenHash: digest[:],
	})
	if err != nil {
		return "", "", err
	}
	return keyID, token, nil
}

func (s *Store) RevokeKey(ctx context.Context, keyID string) error {
	rows, err := s.queries.RevokeAPIKey(ctx, keyID)
	if err != nil {
		return err
	}
	if rows == 0 {
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
	key, err := s.queries.GetActiveAPIKey(ctx, keyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return gateway.KeyIdentity{}, gateway.ErrInvalidKey
	}
	if err != nil {
		return gateway.KeyIdentity{}, err
	}
	digest := sha256.Sum256([]byte(token))
	if subtle.ConstantTimeCompare(digest[:], key.TokenHash) != 1 {
		return gateway.KeyIdentity{}, gateway.ErrInvalidKey
	}
	return gateway.KeyIdentity{AccountID: key.AccountID, KeyID: key.ID}, nil
}

func (s *Store) BeginUsage(ctx context.Context, start gateway.UsageStart) error {
	return s.queries.BeginUsage(ctx, db.BeginUsageParams{
		RequestID: start.RequestID, AccountID: start.AccountID, KeyID: start.KeyID, Model: start.Model,
	})
}

func (s *Store) FinishUsage(ctx context.Context, finish gateway.UsageFinish) error {
	if finish.Outcome != "complete" && finish.Outcome != "incomplete" && finish.Outcome != "failed" {
		return errors.New("invalid usage outcome")
	}
	if finish.Outcome == "complete" && (finish.PromptTokens == nil || finish.CompletionTokens == nil) {
		return errors.New("complete usage requires token counts")
	}
	status := int32(finish.Status)
	rows, err := s.queries.FinishUsage(ctx, db.FinishUsageParams{
		RequestID: finish.RequestID, Backend: finish.Backend, HttpStatus: &status,
		State: finish.Outcome, PromptTokens: finish.PromptTokens, CompletionTokens: finish.CompletionTokens,
	})
	if err != nil {
		return err
	}
	if rows != 1 {
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
	err := s.queries.SetModelPrice(ctx, db.SetModelPriceParams{
		ID: id, Model: model, InputRateMicroPerMillion: inputRate, OutputRateMicroPerMillion: outputRate,
	})
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
	rows, err := s.queries.GetDailyUsage(ctx, db.GetDailyUsageParams{
		AccountID: accountID, Day: pgtype.Date{Time: since, Valid: true},
	})
	if err != nil {
		return nil, err
	}
	usage := make([]DailyUsage, 0, len(rows))
	for _, row := range rows {
		usage = append(usage, DailyUsage{
			Day: row.Day.Time, Model: row.Model, KeyID: row.KeyID,
			Requests: row.RequestCount, Complete: row.CompleteCount,
			Incomplete: row.IncompleteCount, Failed: row.FailedCount,
			PromptTokens: row.PromptTokens, CompletionTokens: row.CompletionTokens,
			EstimatedCostMicro: row.EstimatedCostMicro, Priced: row.PricedCount,
		})
	}
	return usage, nil
}

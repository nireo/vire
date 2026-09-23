package accountdb

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alexedwards/argon2id"
	"github.com/jackc/pgx/v5"
	"github.com/nireo/vire/gateway"
)

var passwordParams = &argon2id.Params{
	Memory: 19 * 1024, Iterations: 2, Parallelism: 1, SaltLength: 16, KeyLength: 32,
}

func hashPassword(password string) (string, error) {
	if !utf8.ValidString(password) || utf8.RuneCountInString(password) < 10 || len(password) > 256 {
		return "", errors.New("password must contain at least 10 characters and at most 256 bytes")
	}
	return argon2id.CreateHash(password, passwordParams)
}

func verifyPassword(password, encoded string) bool {
	match, err := argon2id.ComparePasswordAndHash(password, encoded)
	return err == nil && match
}

func newSessionToken() (string, []byte, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", nil, err
	}
	token := base64.RawURLEncoding.EncodeToString(random)
	digest := sha256.Sum256([]byte(token))
	return token, digest[:], nil
}

func sessionDigest(token string) ([]byte, bool) {
	if len(token) != 43 {
		return nil, false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) != 32 {
		return nil, false
	}
	digest := sha256.Sum256([]byte(token))
	return digest[:], true
}

func (s *Store) Login(ctx context.Context, email, password string) (gateway.PortalSession, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	// A shared database counter limits guessing across gateway replicas.
	var attempts int
	err := s.pool.QueryRow(ctx, `INSERT INTO login_attempts(email, attempts, window_started)
        VALUES ($1, 1, now()) ON CONFLICT (email) DO UPDATE SET
        attempts = CASE WHEN login_attempts.window_started < now() - interval '15 minutes' THEN 1 ELSE login_attempts.attempts + 1 END,
        window_started = CASE WHEN login_attempts.window_started < now() - interval '15 minutes' THEN now() ELSE login_attempts.window_started END
        RETURNING attempts`, email).Scan(&attempts)
	if err != nil {
		return gateway.PortalSession{}, err
	}
	if attempts > 5 {
		return gateway.PortalSession{}, gateway.ErrLoginLimited
	}
	var account gateway.PortalAccount
	var userID, encoded string
	err = s.pool.QueryRow(ctx, `SELECT u.id, u.email, a.id, a.name, p.password_hash
        FROM users u JOIN user_passwords p ON p.user_id = u.id
        JOIN account_members m ON m.user_id = u.id AND m.role = 'owner'
        JOIN accounts a ON a.id = m.account_id WHERE u.email = $1 LIMIT 1`, email).
		Scan(&userID, &account.Email, &account.AccountID, &account.Name, &encoded)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return gateway.PortalSession{}, err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		// Burn comparable Argon2 work for unknown accounts.
		_, _ = argon2id.CreateHash(password, passwordParams)
		return gateway.PortalSession{}, gateway.ErrInvalidCredentials
	}
	if !verifyPassword(password, encoded) {
		return gateway.PortalSession{}, gateway.ErrInvalidCredentials
	}
	token, digest, err := newSessionToken()
	if err != nil {
		return gateway.PortalSession{}, err
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO portal_sessions(token_hash,user_id,account_id,expires_at) VALUES ($1,$2,$3,now() + interval '7 days')`, digest, userID, account.AccountID); err != nil {
		return gateway.PortalSession{}, err
	}
	_, _ = s.pool.Exec(ctx, `DELETE FROM login_attempts WHERE email=$1`, email)
	return gateway.PortalSession{Token: token, Account: account}, nil
}

func (s *Store) Session(ctx context.Context, token string) (gateway.PortalAccount, error) {
	digest, ok := sessionDigest(token)
	if !ok {
		return gateway.PortalAccount{}, gateway.ErrInvalidSession
	}
	var account gateway.PortalAccount
	err := s.pool.QueryRow(ctx, `SELECT p.account_id,a.name,u.email FROM portal_sessions p
        JOIN users u ON u.id=p.user_id JOIN accounts a ON a.id=p.account_id
        WHERE p.token_hash=$1 AND p.expires_at > now()`, digest).
		Scan(&account.AccountID, &account.Name, &account.Email)
	if errors.Is(err, pgx.ErrNoRows) {
		return gateway.PortalAccount{}, gateway.ErrInvalidSession
	}
	return account, err
}

func (s *Store) ChatKeyID(ctx context.Context, accountID string) (string, error) {
	var keyID string
	err := s.pool.QueryRow(ctx, `SELECT id FROM api_keys WHERE account_id=$1 AND revoked_at IS NULL ORDER BY created_at, id LIMIT 1`, accountID).Scan(&keyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return keyID, err
}

func (s *Store) Logout(ctx context.Context, token string) error {
	digest, ok := sessionDigest(token)
	if !ok {
		return gateway.ErrInvalidSession
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM portal_sessions WHERE token_hash=$1`, digest)
	return err
}

func (s *Store) Usage(ctx context.Context, accountID string, since time.Time) ([]gateway.PortalUsage, error) {
	rows, err := s.pool.Query(ctx, `SELECT day, model, SUM(requests), SUM(complete), SUM(incomplete), SUM(failed),
        SUM(prompt_tokens), SUM(completion_tokens), SUM(estimated_cost_micro), SUM(priced)
        FROM (
          SELECT day,model,request_count AS requests,complete_count AS complete,incomplete_count AS incomplete,
            failed_count AS failed,prompt_tokens,completion_tokens,estimated_cost_micro,priced_count AS priced
          FROM usage_daily WHERE account_id=$1 AND day >= $2::date
          UNION ALL
          SELECT started_at::date AS day,model,COUNT(*) AS requests,
            COUNT(*) FILTER (WHERE state='complete') AS complete,
            COUNT(*) FILTER (WHERE state='incomplete') AS incomplete,
            COUNT(*) FILTER (WHERE state='failed') AS failed,
            COALESCE(SUM(prompt_tokens),0),COALESCE(SUM(completion_tokens),0),
			COALESCE(SUM(estimated_cost_micro),0),COUNT(*) FILTER (WHERE estimated_cost_micro IS NOT NULL)
          FROM usage_events WHERE account_id=$1 AND started_at >= $2::date
            AND rolled_up_at IS NULL AND state <> 'pending' GROUP BY started_at::date,model
        ) u GROUP BY day,model ORDER BY day DESC,model`, accountID, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	usage := make([]gateway.PortalUsage, 0)
	for rows.Next() {
		var item gateway.PortalUsage
		if err := rows.Scan(&item.Day, &item.Model, &item.Requests, &item.Complete, &item.Incomplete, &item.Failed, &item.PromptTokens, &item.CompletionTokens, &item.EstimatedCostMicro, &item.Priced); err != nil {
			return nil, err
		}
		usage = append(usage, item)
	}
	return usage, rows.Err()
}

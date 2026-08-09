package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// The chat platforms a panel account can be linked to.
const (
	ProviderDiscord  = "discord"
	ProviderTelegram = "telegram"
)

// Integration errors.
var (
	// ErrLinkTaken means the chat account is already linked to someone else.
	ErrLinkTaken = errors.New("that account is already linked")
	// ErrCodeInvalid means the code is unknown, expired or already used.
	ErrCodeInvalid = errors.New("the code is invalid or has expired")
)

// CodeTTL is how long a linking code stays valid.
//
// Long enough to switch windows and retype it, short enough that a code left
// on screen is not a standing key to the account. The code's real defence is
// being single-use; this bounds the window in which guessing is worth trying.
const CodeTTL = 10 * time.Minute

// codeAlphabet leaves out the characters people confuse when copying from a
// screen: no O or 0, no I, l or 1. A code someone mistypes is a support
// question, and this is the cheapest place to prevent it.
const codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// codeLength gives 31^8, about 40 bits. With a ten-minute life, a single use
// and the panel's rate limiting in front of it, guessing is not a threat that
// buys shortening the code any further.
const codeLength = 8

// IntegrationLink ties a chat account to a panel account.
type IntegrationLink struct {
	ID         string
	Provider   string
	ExternalID string
	UserID     string
	CreatedAt  time.Time
}

// IntegrationRepo stores links and the one-time codes that create them.
type IntegrationRepo struct{ db *sql.DB }

// GenerateCode returns a new linking code in the form ABCD-EFGH.
//
// The dash is presentational: it is stripped before hashing, so a person who
// types it either way is understood.
func GenerateCode() (string, error) {
	limit := big.NewInt(int64(len(codeAlphabet)))

	var b strings.Builder
	for i := 0; i < codeLength; i++ {
		n, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", fmt.Errorf("generating a linking code: %w", err)
		}
		b.WriteByte(codeAlphabet[n.Int64()])
	}

	code := b.String()
	return code[:4] + "-" + code[4:], nil
}

// NormalizeCode puts a typed code into the form that is hashed: upper case,
// no dashes, no spaces.
func NormalizeCode(code string) string {
	replacer := strings.NewReplacer("-", "", " ", "", "\t", "")
	return strings.ToUpper(replacer.Replace(strings.TrimSpace(code)))
}

// hashCode returns the storage form of a code. The same hash as tokens use;
// the plaintext is never written down.
func hashCode(code string) string { return HashToken(NormalizeCode(code)) }

// IssueCode replaces any outstanding code for this account and provider with
// a new one, returning the plaintext exactly once.
//
// Replaced rather than added to: a person who asks for a second code has
// almost certainly lost the first, and leaving both alive doubles the number
// of live keys for no benefit.
func (r *IntegrationRepo) IssueCode(ctx context.Context, provider, userID string) (string, time.Time, error) {
	code, err := GenerateCode()
	if err != nil {
		return "", time.Time{}, err
	}

	now := time.Now().UTC()
	expires := now.Add(CodeTTL)

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("issuing a linking code: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM integration_codes WHERE provider = ? AND user_id = ?`,
		provider, userID); err != nil {
		return "", time.Time{}, fmt.Errorf("clearing previous codes: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO integration_codes (hash, provider, user_id, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		hashCode(code), provider, userID, formatTime(expires), formatTime(now)); err != nil {
		return "", time.Time{}, fmt.Errorf("storing the linking code: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", time.Time{}, fmt.Errorf("issuing a linking code: %w", err)
	}
	return code, expires, nil
}

// Redeem exchanges a code for a link between the account that asked for it
// and the given chat account.
//
// Consuming the code and creating the link are two transactions on purpose.
// One transaction would look tidier and would be wrong: every failure after
// the code was deleted rolls that deletion back, so a refused attempt hands
// the code back to whoever made it. A code that survives being refused is a
// code an attacker can sit and retry.
func (r *IntegrationRepo) Redeem(ctx context.Context, provider, code, externalID string) (*IntegrationLink, error) {
	if strings.TrimSpace(externalID) == "" {
		return nil, errors.New("the external account id is empty")
	}

	userID, err := r.consumeCode(ctx, provider, code)
	if err != nil {
		return nil, err
	}

	link := &IntegrationLink{
		ID: NewID(), Provider: provider, ExternalID: externalID,
		UserID: userID, CreatedAt: time.Now().UTC(),
	}
	if err := r.createLink(ctx, link); err != nil {
		return nil, err
	}
	return link, nil
}

// consumeCode deletes a code and returns the account it belonged to, refusing
// one that is unknown or expired.
func (r *IntegrationRepo) consumeCode(ctx context.Context, provider, code string) (string, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("redeeming a linking code: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var userID, rawExpires string
	err = tx.QueryRowContext(ctx,
		`SELECT user_id, expires_at FROM integration_codes WHERE hash = ? AND provider = ?`,
		hashCode(code), provider).Scan(&userID, &rawExpires)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrCodeInvalid
	}
	if err != nil {
		return "", fmt.Errorf("reading the linking code: %w", err)
	}

	// Deleted before the expiry is judged, so an expired code is spent rather
	// than left for a second attempt.
	if _, err := tx.ExecContext(ctx, `DELETE FROM integration_codes WHERE hash = ?`, hashCode(code)); err != nil {
		return "", fmt.Errorf("consuming the linking code: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("consuming the linking code: %w", err)
	}

	expires, err := parseTime(rawExpires)
	if err != nil {
		return "", fmt.Errorf("reading the code's expiry: %w", err)
	}
	if time.Now().UTC().After(expires) {
		return "", ErrCodeInvalid
	}
	return userID, nil
}

// createLink replaces the account's link for this platform with a new one.
func (r *IntegrationRepo) createLink(ctx context.Context, link *IntegrationLink) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("creating the link: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// A person relinking after changing accounts should not have to unlink
	// first, so their previous link for this platform is replaced.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM integration_links WHERE provider = ? AND user_id = ?`,
		link.Provider, link.UserID); err != nil {
		return fmt.Errorf("clearing the previous link: %w", err)
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO integration_links (id, provider, external_id, user_id, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		link.ID, link.Provider, link.ExternalID, link.UserID, formatTime(link.CreatedAt))
	if err != nil {
		if isUniqueViolation(err, "idx_integration_links_external", "integration_links.external_id") {
			// The chat account belongs to a different panel account. Said
			// plainly, because the alternative reading — "your code was
			// wrong" — sends the person to re-issue codes forever.
			return ErrLinkTaken
		}
		return fmt.Errorf("creating the link: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("creating the link: %w", err)
	}
	return nil
}

// ByExternalID returns the link for a chat account, which is how a delegated
// request finds the person it is acting for.
func (r *IntegrationRepo) ByExternalID(ctx context.Context, provider, externalID string) (*IntegrationLink, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT id, provider, external_id, user_id, created_at
		 FROM integration_links WHERE provider = ? AND external_id = ?`,
		provider, externalID)
	return scanLink(row)
}

// ListForUser returns every link a panel account has.
func (r *IntegrationRepo) ListForUser(ctx context.Context, userID string) ([]*IntegrationLink, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, provider, external_id, user_id, created_at
		 FROM integration_links WHERE user_id = ? ORDER BY provider`, userID)
	if err != nil {
		return nil, fmt.Errorf("listing links for user %s: %w", userID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []*IntegrationLink
	for rows.Next() {
		link, err := scanLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, link)
	}
	return out, rows.Err()
}

// Unlink removes a panel account's link for one platform.
func (r *IntegrationRepo) Unlink(ctx context.Context, provider, userID string) error {
	result, err := r.db.ExecContext(ctx,
		`DELETE FROM integration_links WHERE provider = ? AND user_id = ?`, provider, userID)
	if err != nil {
		return fmt.Errorf("unlinking %s for user %s: %w", provider, userID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("unlinking %s for user %s: %w", provider, userID, err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// SweepCodes deletes expired codes. Called on a timer; an expired code is
// already refused, so this is housekeeping rather than enforcement.
func (r *IntegrationRepo) SweepCodes(ctx context.Context) (int64, error) {
	result, err := r.db.ExecContext(ctx,
		`DELETE FROM integration_codes WHERE expires_at < ?`, formatTime(time.Now().UTC()))
	if err != nil {
		return 0, fmt.Errorf("sweeping linking codes: %w", err)
	}
	return result.RowsAffected()
}

// scanner is what both QueryRow and Rows satisfy.
type scanner interface{ Scan(dest ...any) error }

func scanLink(row scanner) (*IntegrationLink, error) {
	var link IntegrationLink
	var created string

	if err := row.Scan(&link.ID, &link.Provider, &link.ExternalID, &link.UserID, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("reading a link: %w", err)
	}

	parsed, err := parseTime(created)
	if err != nil {
		return nil, fmt.Errorf("reading a link's creation time: %w", err)
	}
	link.CreatedAt = parsed
	return &link, nil
}

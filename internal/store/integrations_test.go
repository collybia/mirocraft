package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func newIntegrationStore(t *testing.T) (*Store, *User, *User) {
	t.Helper()

	db, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	first := &User{Email: "first@example.com", PasswordHash: "x", Role: RoleUser}
	if err := db.Users.Create(ctx, first); err != nil {
		t.Fatalf("creating the first user: %v", err)
	}
	second := &User{Email: "second@example.com", PasswordHash: "x", Role: RoleUser}
	if err := db.Users.Create(ctx, second); err != nil {
		t.Fatalf("creating the second user: %v", err)
	}
	return db, first, second
}

func TestCodeRoundTrip(t *testing.T) {
	db, user, _ := newIntegrationStore(t)
	ctx := context.Background()

	code, expires, err := db.Integrations.IssueCode(ctx, ProviderDiscord, user.ID)
	if err != nil {
		t.Fatalf("IssueCode: %v", err)
	}
	if !expires.After(time.Now()) {
		t.Errorf("expires at %s, which is not in the future", expires)
	}
	// Readable off a screen: a dash in the middle and no ambiguous letters.
	if len(code) != codeLength+1 || code[4] != '-' {
		t.Errorf("code = %q, want the ABCD-EFGH shape", code)
	}
	for _, c := range strings.ReplaceAll(code, "-", "") {
		if !strings.ContainsRune(codeAlphabet, c) {
			t.Errorf("code %q contains %q, which is outside the alphabet", code, c)
		}
	}

	link, err := db.Integrations.Redeem(ctx, ProviderDiscord, code, "31337")
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if link.UserID != user.ID {
		t.Errorf("linked user = %q, want %q", link.UserID, user.ID)
	}

	found, err := db.Integrations.ByExternalID(ctx, ProviderDiscord, "31337")
	if err != nil {
		t.Fatalf("ByExternalID: %v", err)
	}
	if found.UserID != user.ID {
		t.Errorf("lookup returned user %q, want %q", found.UserID, user.ID)
	}
}

// A code that survives its use is a code someone can replay.
func TestACodeWorksOnlyOnce(t *testing.T) {
	db, user, _ := newIntegrationStore(t)
	ctx := context.Background()

	code, _, err := db.Integrations.IssueCode(ctx, ProviderDiscord, user.ID)
	if err != nil {
		t.Fatalf("IssueCode: %v", err)
	}
	if _, err := db.Integrations.Redeem(ctx, ProviderDiscord, code, "31337"); err != nil {
		t.Fatalf("first Redeem: %v", err)
	}

	_, err = db.Integrations.Redeem(ctx, ProviderDiscord, code, "42")
	if !errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("second Redeem: %v, want ErrCodeInvalid", err)
	}
}

// And one that is refused must be consumed too, or it can be brute-forced by
// retrying against it.
func TestAFailedRedemptionStillConsumesTheCode(t *testing.T) {
	db, user, other := newIntegrationStore(t)
	ctx := context.Background()

	// The chat account already belongs to someone else.
	taken, _, err := db.Integrations.IssueCode(ctx, ProviderDiscord, other.ID)
	if err != nil {
		t.Fatalf("IssueCode for the other user: %v", err)
	}
	if _, err := db.Integrations.Redeem(ctx, ProviderDiscord, taken, "31337"); err != nil {
		t.Fatalf("linking the other user: %v", err)
	}

	code, _, err := db.Integrations.IssueCode(ctx, ProviderDiscord, user.ID)
	if err != nil {
		t.Fatalf("IssueCode: %v", err)
	}
	if _, err := db.Integrations.Redeem(ctx, ProviderDiscord, code, "31337"); !errors.Is(err, ErrLinkTaken) {
		t.Fatalf("Redeem: %v, want ErrLinkTaken", err)
	}

	// Retrying the same code against a free account must not work either.
	if _, err := db.Integrations.Redeem(ctx, ProviderDiscord, code, "99999"); !errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("retry: %v, want ErrCodeInvalid — the code survived a failed attempt", err)
	}
}

func TestAnExpiredCodeIsRefused(t *testing.T) {
	db, user, _ := newIntegrationStore(t)
	ctx := context.Background()

	code, _, err := db.Integrations.IssueCode(ctx, ProviderDiscord, user.ID)
	if err != nil {
		t.Fatalf("IssueCode: %v", err)
	}
	// Aged past its life by rewriting the stored expiry, which is the only
	// part of the mechanism that depends on the clock.
	if _, err := db.db.ExecContext(ctx,
		`UPDATE integration_codes SET expires_at = ?`,
		formatTime(time.Now().UTC().Add(-time.Minute))); err != nil {
		t.Fatalf("ageing the code: %v", err)
	}

	if _, err := db.Integrations.Redeem(ctx, ProviderDiscord, code, "31337"); !errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("Redeem: %v, want ErrCodeInvalid", err)
	}
}

// A typed code is a typed code, dashes or not.
func TestCodesAreAcceptedHoweverTheyAreTyped(t *testing.T) {
	db, user, _ := newIntegrationStore(t)
	ctx := context.Background()

	code, _, err := db.Integrations.IssueCode(ctx, ProviderDiscord, user.ID)
	if err != nil {
		t.Fatalf("IssueCode: %v", err)
	}

	typed := " " + strings.ToLower(strings.ReplaceAll(code, "-", "")) + " "
	if _, err := db.Integrations.Redeem(ctx, ProviderDiscord, typed, "31337"); err != nil {
		t.Fatalf("Redeem(%q): %v", typed, err)
	}
}

// Asking for a second code must retire the first: two live keys where one was
// meant is the sort of thing nobody notices until it matters.
func TestIssuingACodeReplacesThePrevious(t *testing.T) {
	db, user, _ := newIntegrationStore(t)
	ctx := context.Background()

	first, _, err := db.Integrations.IssueCode(ctx, ProviderDiscord, user.ID)
	if err != nil {
		t.Fatalf("first IssueCode: %v", err)
	}
	second, _, err := db.Integrations.IssueCode(ctx, ProviderDiscord, user.ID)
	if err != nil {
		t.Fatalf("second IssueCode: %v", err)
	}

	if _, err := db.Integrations.Redeem(ctx, ProviderDiscord, first, "31337"); !errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("the first code still works: %v", err)
	}
	if _, err := db.Integrations.Redeem(ctx, ProviderDiscord, second, "31337"); err != nil {
		t.Fatalf("the second code does not work: %v", err)
	}
}

// One chat account, one panel account. Without this two people could link the
// same Discord account and each would see the other's servers.
func TestAChatAccountLinksToOnePanelAccount(t *testing.T) {
	db, user, other := newIntegrationStore(t)
	ctx := context.Background()

	code, _, err := db.Integrations.IssueCode(ctx, ProviderDiscord, user.ID)
	if err != nil {
		t.Fatalf("IssueCode: %v", err)
	}
	if _, err := db.Integrations.Redeem(ctx, ProviderDiscord, code, "31337"); err != nil {
		t.Fatalf("Redeem: %v", err)
	}

	stolen, _, err := db.Integrations.IssueCode(ctx, ProviderDiscord, other.ID)
	if err != nil {
		t.Fatalf("IssueCode for the other user: %v", err)
	}
	if _, err := db.Integrations.Redeem(ctx, ProviderDiscord, stolen, "31337"); !errors.Is(err, ErrLinkTaken) {
		t.Fatalf("Redeem: %v, want ErrLinkTaken", err)
	}
}

// Relinking after changing chat accounts should not need an unlink first.
func TestRelinkingReplacesThePreviousLink(t *testing.T) {
	db, user, _ := newIntegrationStore(t)
	ctx := context.Background()

	for _, external := range []string{"31337", "42"} {
		code, _, err := db.Integrations.IssueCode(ctx, ProviderDiscord, user.ID)
		if err != nil {
			t.Fatalf("IssueCode: %v", err)
		}
		if _, err := db.Integrations.Redeem(ctx, ProviderDiscord, code, external); err != nil {
			t.Fatalf("Redeem(%s): %v", external, err)
		}
	}

	links, err := db.Integrations.ListForUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("ListForUser: %v", err)
	}
	if len(links) != 1 || links[0].ExternalID != "42" {
		t.Fatalf("links = %+v, want only the newest", links)
	}
	if _, err := db.Integrations.ByExternalID(ctx, ProviderDiscord, "31337"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the old link still resolves: %v", err)
	}
}

// The two platforms are independent: linking Telegram must not disturb Discord.
func TestProvidersAreIndependent(t *testing.T) {
	db, user, _ := newIntegrationStore(t)
	ctx := context.Background()

	for _, provider := range []string{ProviderDiscord, ProviderTelegram} {
		code, _, err := db.Integrations.IssueCode(ctx, provider, user.ID)
		if err != nil {
			t.Fatalf("IssueCode(%s): %v", provider, err)
		}
		if _, err := db.Integrations.Redeem(ctx, provider, code, "31337"); err != nil {
			t.Fatalf("Redeem(%s): %v", provider, err)
		}
	}

	links, err := db.Integrations.ListForUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("ListForUser: %v", err)
	}
	if len(links) != 2 {
		t.Fatalf("links = %+v, want one per platform", links)
	}
}

func TestUnlink(t *testing.T) {
	db, user, _ := newIntegrationStore(t)
	ctx := context.Background()

	code, _, err := db.Integrations.IssueCode(ctx, ProviderDiscord, user.ID)
	if err != nil {
		t.Fatalf("IssueCode: %v", err)
	}
	if _, err := db.Integrations.Redeem(ctx, ProviderDiscord, code, "31337"); err != nil {
		t.Fatalf("Redeem: %v", err)
	}

	if err := db.Integrations.Unlink(ctx, ProviderDiscord, user.ID); err != nil {
		t.Fatalf("Unlink: %v", err)
	}
	if err := db.Integrations.Unlink(ctx, ProviderDiscord, user.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Unlink: %v, want ErrNotFound", err)
	}
}

// Deleting an account must take its links with it, or a deleted user's Discord
// would keep resolving to a row pointing at nobody.
func TestDeletingAUserRemovesTheirLinks(t *testing.T) {
	db, user, _ := newIntegrationStore(t)
	ctx := context.Background()

	code, _, err := db.Integrations.IssueCode(ctx, ProviderDiscord, user.ID)
	if err != nil {
		t.Fatalf("IssueCode: %v", err)
	}
	if _, err := db.Integrations.Redeem(ctx, ProviderDiscord, code, "31337"); err != nil {
		t.Fatalf("Redeem: %v", err)
	}

	if err := db.Users.Delete(ctx, user.ID); err != nil {
		t.Fatalf("deleting the user: %v", err)
	}
	if _, err := db.Integrations.ByExternalID(ctx, ProviderDiscord, "31337"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the link outlived its user: %v", err)
	}
}

func TestSweepCodesRemovesOnlyExpiredOnes(t *testing.T) {
	db, user, other := newIntegrationStore(t)
	ctx := context.Background()

	if _, _, err := db.Integrations.IssueCode(ctx, ProviderDiscord, other.ID); err != nil {
		t.Fatalf("IssueCode for the other user: %v", err)
	}
	if _, _, err := db.Integrations.IssueCode(ctx, ProviderTelegram, user.ID); err != nil {
		t.Fatalf("IssueCode: %v", err)
	}
	if _, err := db.db.ExecContext(ctx,
		`UPDATE integration_codes SET expires_at = ? WHERE provider = ?`,
		formatTime(time.Now().UTC().Add(-time.Hour)), ProviderDiscord); err != nil {
		t.Fatalf("ageing a code: %v", err)
	}

	removed, err := db.Integrations.SweepCodes(ctx)
	if err != nil {
		t.Fatalf("SweepCodes: %v", err)
	}
	if removed != 1 {
		t.Fatalf("swept %d codes, want 1", removed)
	}
}

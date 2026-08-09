package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/collybia/mirocraft/internal/store"
)

// DelegationHeader names the chat account a bot is acting for.
//
// The value is "provider:id", for example "discord:31337". A bot authenticates
// as itself and says who it is acting for; the panel decides what that person
// may do. Authorization never moves into the bot, which is the project's rule
// about management logic applied to permissions.
const DelegationHeader = "X-Mirocraft-On-Behalf-Of"

// DelegatableScopes is the ceiling on what a delegated request may do,
// whatever the person it acts for is allowed.
//
// A chat bot exists to start a server and read its console. It has no business
// administering accounts, editing files or restoring backups over someone's
// world — and a bot token is a single credential sitting on a machine that
// also runs a chat library. Capping here means a stolen bot token cannot do
// those things even when the person who linked their account is an admin.
//
// The person's own permissions still apply on top: this is a ceiling, not a
// grant. Someone who cannot power their server through the panel cannot power
// it through the bot either.
var DelegatableScopes = []string{
	ScopeServersRead,
	ScopeServersPower,
	ScopeServersConsole,
	ScopeBackupsRead,
}

// Delegation errors, kept separate so the middleware can say which went wrong.
var (
	errDelegationMalformed = errors.New("the delegation header is malformed")
	errDelegationUnknown   = errors.New("that account is not linked to a panel user")
)

// parseDelegation splits a header value into a provider and an account id.
func parseDelegation(value string) (provider, externalID string, err error) {
	provider, externalID, found := strings.Cut(strings.TrimSpace(value), ":")
	if !found {
		return "", "", errDelegationMalformed
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	externalID = strings.TrimSpace(externalID)
	if provider == "" || externalID == "" {
		return "", "", errDelegationMalformed
	}
	switch provider {
	case store.ProviderDiscord, store.ProviderTelegram:
	default:
		return "", "", fmt.Errorf("%w: unknown provider %q", errDelegationMalformed, provider)
	}
	return provider, externalID, nil
}

// delegate rewrites a bot's principal into the principal of the person it is
// acting for.
//
// Returns the original principal untouched when no delegation was asked for,
// so the header being absent is the ordinary case rather than a branch every
// caller has to think about.
func (a *API) delegate(ctx context.Context, bot *Principal, header string) (*Principal, error) {
	if strings.TrimSpace(header) == "" {
		return bot, nil
	}
	// Checked literally rather than through HasScope, which honours admin:*
	// as a wildcard. Acting for another person is not something a broad grant
	// should sweep up: it has to be asked for by name.
	if !hasExactScope(bot, ScopeIntegrationsAct) {
		return nil, fmt.Errorf("token is missing the %s scope", ScopeIntegrationsAct)
	}

	provider, externalID, err := parseDelegation(header)
	if err != nil {
		return nil, err
	}

	link, err := a.store.Integrations.ByExternalID(ctx, provider, externalID)
	if err != nil {
		return nil, errDelegationUnknown
	}

	user, err := a.store.Users.GetByID(ctx, link.UserID)
	if err != nil {
		return nil, errDelegationUnknown
	}
	if user.Blocked {
		return nil, errors.New("that panel account is blocked")
	}

	return &Principal{
		UserID: user.ID,
		Email:  user.Email,
		// The role travels, because a server belongs to someone and an admin
		// acting through a bot should still see their own servers. What does
		// not travel is admin:*, which the scope set below refuses to include.
		Role:   user.Role,
		Scopes: delegatedScopes(user.Role),
		// The bot's token, not the person's: they have no token here, and the
		// audit trail should name the credential that was actually used.
		TokenID:  bot.TokenID,
		Delegate: &Delegation{Provider: provider, ExternalID: externalID, BotUserID: bot.UserID},
	}, nil
}

// hasExactScope reports whether the scope was granted by name, ignoring the
// admin:* wildcard.
func hasExactScope(p *Principal, scope string) bool {
	if p == nil {
		return false
	}
	for _, s := range p.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// delegatedScopes returns what a person can do through a bot: everything their
// role allows, intersected with the delegatable set.
func delegatedScopes(role string) []string {
	allowed := scopesForRole(role)

	out := make([]string, 0, len(DelegatableScopes))
	for _, scope := range DelegatableScopes {
		for _, granted := range allowed {
			// admin:* is deliberately not treated as a wildcard here. It is
			// the one grant a bot must not be able to borrow, and reading it
			// as "everything" is exactly how it would be borrowed.
			if granted == scope {
				out = append(out, scope)
				break
			}
		}
	}
	return out
}

// Delegation records that a request was made by a bot for someone else.
type Delegation struct {
	// Provider is "discord" or "telegram".
	Provider string
	// ExternalID is the chat account the bot named.
	ExternalID string
	// BotUserID is the panel account the bot's own token belongs to.
	BotUserID string
}

// String renders a delegation for the audit log.
func (d *Delegation) String() string {
	if d == nil {
		return ""
	}
	return d.Provider + ":" + d.ExternalID
}

// withDelegation is middleware that applies the delegation header after
// authentication.
//
// A separate step rather than part of authenticate, so the two questions stay
// apart: authenticate answers "which credential is this", and this answers
// "who is it acting for". A failure here is 403 rather than 401 — the token
// was fine, the delegation was not, and telling the caller to log in again
// would send them to fix the wrong thing.
func (a *API) withDelegation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get(DelegationHeader)
		if header == "" {
			next.ServeHTTP(w, r)
			return
		}

		bot, ok := principalFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, CodeUnauthorized, "not authenticated")
			return
		}

		principal, err := a.delegate(r.Context(), bot, header)
		if err != nil {
			writeError(w, http.StatusForbidden, CodeForbidden, err.Error())
			return
		}

		// Nil when the header held only whitespace, which delegate treats as
		// absent. Dereferencing it here would panic on a request an attacker
		// can send at will.
		if principal.Delegate != nil {
			a.log.Debug("acting on behalf of a linked account",
				"provider", principal.Delegate.Provider,
				"bot_user_id", principal.Delegate.BotUserID,
				"user_id", principal.UserID)
		}

		next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), principal)))
	})
}

// refuseDelegation is middleware for the endpoints a bot must never reach,
// whoever it claims to act for.
//
// A refusal rather than a silent ignore: quietly dropping the header would
// run the request as the bot's own account, which is a different and possibly
// more privileged caller than the one that was asked for.
func (a *API) refuseDelegation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(DelegationHeader) != "" {
			writeError(w, http.StatusForbidden, CodeForbidden,
				"this endpoint cannot be used on behalf of a linked account")
			return
		}
		next.ServeHTTP(w, r)
	})
}

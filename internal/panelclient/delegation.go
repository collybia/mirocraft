package panelclient

import (
	"context"
	"net/http"
	"strings"
)

// DelegationHeader names the chat account a request is made for. The panel
// resolves it to a linked account and applies that person's permissions.
const DelegationHeader = "X-Mirocraft-On-Behalf-Of"

// The platforms a chat account can belong to.
const (
	ProviderDiscord  = "discord"
	ProviderTelegram = "telegram"
)

// For returns a client that makes every request on behalf of one chat account.
//
// A separate client rather than a parameter on each call: a bot handles one
// person per command, and threading an identity through forty call sites is
// how one of them ends up missing it and answering with someone else's
// servers. The returned client shares the underlying HTTP client and token —
// only the identity differs.
func (c *Client) For(provider, externalID string) *Client {
	clone := *c
	clone.onBehalfOf = provider + ":" + externalID
	return &clone
}

// OnBehalfOf returns the chat account this client acts for, empty when it acts
// as itself.
func (c *Client) OnBehalfOf() string { return c.onBehalfOf }

// applyDelegation sets the header when the client is acting for someone.
func (c *Client) applyDelegation(req *http.Request) {
	if strings.TrimSpace(c.onBehalfOf) != "" {
		req.Header.Set(DelegationHeader, c.onBehalfOf)
	}
}

// Link exchanges a code a person typed in a chat for a link to their panel
// account.
//
// Called with the bot's own token, not a delegated one: there is nobody to
// delegate to until this succeeds.
func (c *Client) Link(ctx context.Context, provider, code, externalID string) (LinkedAccount, error) {
	var out LinkedAccount
	body := map[string]string{"code": code, "external_id": externalID}
	err := c.self().do(ctx, http.MethodPost, "/integrations/"+provider+"/link", nil, body, &out)
	return out, err
}

// Unlink forgets the link for the account this client acts for.
func (c *Client) Unlink(ctx context.Context, provider string) error {
	return c.do(ctx, http.MethodDelete, "/integrations/"+provider, nil, nil, nil)
}

// self returns a client that acts as itself, for the calls that must not be
// delegated.
func (c *Client) self() *Client {
	clone := *c
	clone.onBehalfOf = ""
	return &clone
}

// LinkedAccount is the panel account a chat account was linked to.
type LinkedAccount struct {
	Provider   string `json:"provider"`
	ExternalID string `json:"external_id"`
	UserID     string `json:"user_id"`
	Email      string `json:"email"`
}

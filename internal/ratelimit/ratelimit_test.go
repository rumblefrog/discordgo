package ratelimit

import (
	"testing"
)

func TestParseURL(t *testing.T) {
	tests := map[string]string{
		// Input		Expected Output
		"/gateway":       "/gateway",
		"/channels/5050": "/channels/5050",

		"/channels/5050/messages":             "/channels/5050/messages",
		"/channels/5050/messages/1337?asdasd": "/channels/5050/messages/",
		"/channels/5050/messages/bulk_delete": "/channels/5050/messages/bulk_delete",

		"/channels/5050/permissions/1337": "/channels/5050/permissions/",
		"/channels/5050/invites":          "/channels/5050/invites",

		"/channels/5050/pins":      "/channels/5050/pins",
		"/channels/5050/pins/1337": "/channels/5050/pins/",

		"/guilds/99":                        "/guilds/99",
		"/guilds/99/channels":               "/guilds/99/channels",
		"/guilds/99/members":                "/guilds/99/members",
		"/guilds/99/members/1337":           "/guilds/99/members/",
		"/guilds/99/bans":                   "/guilds/99/bans",
		"/guilds/99/bans/1337":              "/guilds/99/bans/",
		"/guilds/99/roles":                  "/guilds/99/roles",
		"/guilds/99/roles/1337":             "/guilds/99/roles/",
		"/guilds/99/prune":                  "/guilds/99/prune",
		"/guilds/99/regions":                "/guilds/99/regions",
		"/guilds/99/invites":                "/guilds/99/invites",
		"/guilds/99/integrations":           "/guilds/99/integrations",
		"/guilds/99/integrations/1337":      "/guilds/99/integrations/",
		"/guilds/99/integrations/1337/sync": "/guilds/99/integrations//sync",
		"/guilds/99/embed":                  "/guilds/99/embed",

		"/users/@me":             "/users/@me",
		"/users/@me/guilds":      "/users/@me/guilds",
		"/users/@me/guilds/99":   "/users/@me/guilds/99",
		"/users/@me/channels":    "/users/@me/channels",
		"/users/@me/connections": "/users/@me/connections",
		"/users/1337":            "/users/",

		"/invites/1337": "/invites/",
	}

	for input, correct := range tests {
		out := ParseURL(input)
		if out != correct {
			t.Errorf("Incorrect parsed url, input: %q, got: %q, expected: %q", input, out, correct)
		}
	}
}

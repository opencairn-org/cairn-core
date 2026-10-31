package oauthserver

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/url"
	"strings"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// Registration is an RFC 7591 client registration request.
type Registration struct {
	ClientName              string
	RedirectURIs            []string
	GrantTypes              []string
	Scope                   string
	TokenEndpointAuthMethod string
	// SoftwareID / SoftwareVersion (RFC 7591 §2) name the application.
	SoftwareID      string
	SoftwareVersion string
}

// Registered is the stored client and, for a confidential client, its
// one-time plaintext secret.
type Registered struct {
	Client domain.OAuthClient
	Secret string
}

// registrationFieldMax caps free-text registration metadata.
const registrationFieldMax = 200

// clipField trims s and caps it at registrationFieldMax runes.
func clipField(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > registrationFieldMax {
		return string(r[:registrationFieldMax])
	}
	return s
}

// Register stores a dynamically registered client; unset fields default to a
// public, read-only PKCE client.
func (s *Server) Register(ctx context.Context, req Registration) (Registered, error) {
	if len(req.RedirectURIs) == 0 {
		return Registered{}, protoErr("invalid_redirect_uri", "at least one redirect_uri is required")
	}
	for _, u := range req.RedirectURIs {
		if _, err := url.Parse(u); err != nil {
			return Registered{}, protoErr("invalid_redirect_uri", "redirect_uri is not a valid URL")
		}
	}
	authMethod := req.TokenEndpointAuthMethod
	if authMethod == "" {
		authMethod = "none"
	}
	grants := req.GrantTypes
	if len(grants) == 0 {
		grants = []string{"authorization_code", "refresh_token", domain.GrantTypeDeviceCode}
	}
	scopes := domain.ReadOnlyScopes
	if req.Scope != "" {
		scopes = domain.FilterRequestedScopes(domain.ParseScopes(req.Scope), domain.SupportedScopes)
	}
	name := clipField(req.ClientName)
	if name == "" {
		name = "Dynamic client"
	}
	out := Registered{Client: domain.OAuthClient{
		ClientID:                "cairn_dcr_" + randString(16),
		Name:                    name,
		RedirectURIs:            req.RedirectURIs,
		GrantTypes:              grants,
		Scopes:                  scopes,
		TokenEndpointAuthMethod: authMethod,
		IsDynamic:               true,
		SoftwareID:              clipField(req.SoftwareID),
		SoftwareVersion:         clipField(req.SoftwareVersion),
	}}
	if authMethod != "none" {
		out.Secret = randString(40)
		h := sha256.Sum256([]byte(out.Secret))
		out.Client.SecretHash = h[:]
	}
	if err := s.Repo.CreateClient(ctx, out.Client); err != nil {
		return Registered{}, serverErr("could not register client", fmt.Errorf("register client: %w", err))
	}
	return out, nil
}

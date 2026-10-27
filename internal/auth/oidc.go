// Package auth verifies external identities. Permissions come from PostgreSQL.
package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

type Identity struct{ Issuer, Subject string }
type Authenticator interface {
	Authenticate(context.Context, string) (Identity, error)
}

var ErrUnauthenticated = errors.New("unauthenticated")

type OIDC struct{ verifier *oidc.IDTokenVerifier }

func secureURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Fragment == ""
}

func NewOIDC(ctx context.Context, issuer, audience string, client *http.Client) (*OIDC, error) {
	if !secureURL(issuer) || audience == "" {
		return nil, errors.New("https issuer and resource audience are required")
	}
	secureClient := http.Client{}
	if client != nil {
		secureClient = *client
	}
	if secureClient.Timeout == 0 {
		secureClient.Timeout = 5 * time.Second
	}
	redirectPolicy := secureClient.CheckRedirect
	secureClient.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if !secureURL(request.URL.String()) {
			return errors.New("oidc redirect requires a secure HTTPS URL")
		}
		if redirectPolicy != nil {
			return redirectPolicy(request, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	ctx = oidc.ClientContext(ctx, &secureClient)
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	var metadata struct {
		JWKS string `json:"jwks_uri"`
	}
	if err := provider.Claims(&metadata); err != nil || !secureURL(metadata.JWKS) {
		return nil, errors.New("https jwks endpoint required")
	}
	return &OIDC{verifier: provider.VerifierContext(ctx, &oidc.Config{ClientID: audience, SupportedSigningAlgs: []string{oidc.RS256, oidc.ES256}})}, nil
}

func (o *OIDC) Authenticate(ctx context.Context, raw string) (Identity, error) {
	if ctx.Err() != nil {
		return Identity{}, ctx.Err()
	}
	if len(raw) == 0 || len(raw) > 16384 {
		return Identity{}, ErrUnauthenticated
	}
	token, err := o.verifier.Verify(ctx, raw)
	if err != nil {
		return Identity{}, ErrUnauthenticated
	}
	var claims struct {
		Type      string `json:"typ"`
		Use       string `json:"token_use"`
		NotBefore int64  `json:"nbf"`
	}
	if err := token.Claims(&claims); err != nil || token.Subject == "" || claims.NotBefore > time.Now().Unix() {
		return Identity{}, ErrUnauthenticated
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Identity{}, ErrUnauthenticated
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Identity{}, ErrUnauthenticated
	}
	var header struct {
		Type string `json:"typ"`
	}
	if json.Unmarshal(headerBytes, &header) != nil {
		return Identity{}, ErrUnauthenticated
	}
	// Access tokens only: Keycloak, token_use providers, or RFC 9068 at+jwt.
	if claims.Type != "Bearer" && claims.Use != "access" && header.Type != "at+jwt" {
		return Identity{}, ErrUnauthenticated
	}
	return Identity{Issuer: token.Issuer, Subject: token.Subject}, nil
}

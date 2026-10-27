package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOIDCVerifiesAccessTokens(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		issuer := "https://" + r.Host
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "jwks_uri": issuer + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}})
		case "/keys":
			json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "test", "alg": "RS256", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	issuer := provider.URL
	verifier, err := NewOIDC(context.Background(), issuer, "wms-api", provider.Client())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		edit  func(map[string]any)
		valid bool
	}{
		{name: "valid access token", valid: true},
		{name: "wrong audience", edit: func(c map[string]any) { c["aud"] = "another-api" }},
		{name: "wrong issuer", edit: func(c map[string]any) { c["iss"] = "https://attacker.invalid" }},
		{name: "expired", edit: func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }},
		{name: "not yet valid", edit: func(c map[string]any) { c["nbf"] = time.Now().Add(time.Hour).Unix() }},
		{name: "missing subject", edit: func(c map[string]any) { delete(c, "sub") }},
		{name: "id token rejected", edit: func(c map[string]any) { delete(c, "typ") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := map[string]any{"iss": issuer, "aud": "wms-api", "sub": "worker", "typ": "Bearer", "exp": time.Now().Add(time.Hour).Unix()}
			if tc.edit != nil {
				tc.edit(claims)
			}
			token := signTestToken(t, key, claims)
			identity, err := verifier.Authenticate(context.Background(), token)
			if (err == nil) != tc.valid {
				t.Fatalf("Authenticate = %+v, %v", identity, err)
			}
			if tc.valid && (identity.Issuer != issuer || identity.Subject != "worker") {
				t.Fatalf("identity=%+v", identity)
			}
		})
	}
	t.Run("wrong signature", func(t *testing.T) {
		other, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		token := signTestToken(t, other, map[string]any{"iss": issuer, "aud": "wms-api", "sub": "worker", "typ": "Bearer", "exp": time.Now().Add(time.Hour).Unix()})
		if _, err := verifier.Authenticate(context.Background(), token); err == nil {
			t.Fatal("invalid signature accepted")
		}
	})
	t.Run("malformed", func(t *testing.T) {
		if _, err := verifier.Authenticate(context.Background(), "not.a.jwt"); err == nil {
			t.Fatal("malformed token accepted")
		}
	})
}

func signTestToken(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "test"})
	body, _ := json.Marshal(claims)
	value := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(value))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return value + "." + base64.RawURLEncoding.EncodeToString(sig)
}

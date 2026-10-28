package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	mathrand "math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/quick"
	"time"
)

func TestOIDCAdversarialAlgorithmsAndLifetime(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var issuer string
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "jwks_uri": issuer + "/keys", "id_token_signing_alg_values_supported": []string{"RS256", "ES256"}})
		case "/keys":
			x, y := make([]byte, 32), make([]byte, 32)
			ecKey.X.FillBytes(x)
			ecKey.Y.FillBytes(y)
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{
				map[string]any{"kty": "RSA", "kid": "test", "alg": "RS256", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(rsaKey.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(rsaKey.E)).Bytes())},
				map[string]any{"kty": "EC", "kid": "ec", "alg": "ES256", "use": "sig", "crv": "P-256", "x": base64.RawURLEncoding.EncodeToString(x), "y": base64.RawURLEncoding.EncodeToString(y)},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(provider.Close)
	issuer = provider.URL
	startup, cancel := context.WithCancel(t.Context())
	verifier, err := NewOIDC(startup, issuer, "wms-api", provider.Client())
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	claims := func() map[string]any {
		return map[string]any{"iss": issuer, "aud": "wms-api", "sub": "worker", "token_use": "access", "exp": time.Now().Add(time.Hour).Unix()}
	}
	t.Run("uncached JWKS survives cancelled startup context", func(t *testing.T) {
		identity, err := verifier.Authenticate(t.Context(), signTestToken(t, rsaKey, claims()))
		if err != nil || identity.Issuer != issuer || identity.Subject != "worker" {
			t.Fatalf("valid RSA access token after startup cancellation: %+v %v", identity, err)
		}
	})
	t.Run("ES256 access token with RFC9068 header", func(t *testing.T) {
		body := claims()
		delete(body, "token_use")
		header, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": "ec", "typ": "at+jwt"})
		payload, _ := json.Marshal(body)
		unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
		digest := sha256.Sum256([]byte(unsigned))
		r, s, err := ecdsa.Sign(rand.Reader, ecKey, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		signature := make([]byte, 64)
		r.FillBytes(signature[:32])
		s.FillBytes(signature[32:])
		identity, err := verifier.Authenticate(t.Context(), unsigned+"."+base64.RawURLEncoding.EncodeToString(signature))
		if err != nil || identity.Subject != "worker" {
			t.Fatalf("valid ES256 access token rejected: %+v %v", identity, err)
		}
	})
	for _, tc := range []struct {
		name string
		edit func(map[string]any)
	}{
		{name: "near-future nbf is rejected without library clock-skew allowance", edit: func(c map[string]any) { c["nbf"] = time.Now().Add(30 * time.Second).Unix() }},
		{name: "missing expiry", edit: func(c map[string]any) { delete(c, "exp") }},
		{name: "empty subject", edit: func(c map[string]any) { c["sub"] = "" }},
		{name: "malformed not-before", edit: func(c map[string]any) { c["nbf"] = "tomorrow" }},
		{name: "identity token cannot be used as an access token", edit: func(c map[string]any) { c["token_use"] = "id" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := claims()
			tc.edit(body)
			if identity, err := verifier.Authenticate(t.Context(), signTestToken(t, rsaKey, body)); err == nil {
				t.Fatalf("invalid access claims accepted as %+v", identity)
			}
		})
	}
	t.Run("every changed signature bit fails authentication", func(t *testing.T) {
		original := signTestToken(t, rsaKey, claims())
		parts := strings.Split(original, ".")
		signature, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil {
			t.Fatal(err)
		}
		property := func(index uint16, bit uint8) bool {
			changed := append([]byte(nil), signature...)
			changed[int(index)%len(changed)] ^= 1 << (bit % 8)
			_, err := verifier.Authenticate(t.Context(), parts[0]+"."+parts[1]+"."+base64.RawURLEncoding.EncodeToString(changed))
			return err != nil
		}
		if err := quick.Check(property, &quick.Config{MaxCount: 64, Rand: mathrand.New(mathrand.NewSource(2026102803))}); err != nil {
			t.Fatal(err)
		}
	})
}

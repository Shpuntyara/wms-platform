package jobs

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSenderSignsExactEventAndRejectsRedirect(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	event := Event{ID: "event-1", Token: "token-1", Payload: []byte(`{"event_id":"event-1"}`)}
	var requests int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		body, _ := io.ReadAll(r.Body)
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write([]byte(r.Header.Get("X-WMS-Timestamp") + "\n" + event.ID + "\n" + string(body)))
		if r.Header.Get("X-WMS-Event-ID") != event.ID || r.Header.Get("X-WMS-Signature") != hex.EncodeToString(mac.Sum(nil)) {
			t.Error("invalid authenticated envelope")
		}
		w.Header().Set("Location", "https://example.invalid/stolen")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	sender, err := NewSender(server.URL, key, false)
	if err != nil {
		t.Fatal(err)
	}
	sender.client.Transport = server.Client().Transport
	if err := sender.Send(t.Context(), event); err == nil {
		t.Fatal("redirect counted as delivery")
	}
	if requests != 1 {
		t.Fatalf("requests=%d", requests)
	}
}
func TestSenderConfigurationAndNetworkBoundary(t *testing.T) {
	for _, url := range []string{"http://example.com", "https://user:pass@example.com", "https://example.com/#secret", "https://example.com/?token=secret", "file:///etc/passwd"} {
		if _, err := NewSender(url, []byte(strings.Repeat("k", 32)), false); err == nil {
			t.Fatalf("unsafe endpoint=%s", url)
		}
	}
	if _, err := NewSender("https://example.com", []byte("short"), false); err == nil {
		t.Fatal("weak key accepted")
	}
	for _, ip := range []string{"127.0.0.1", "::1", "169.254.169.254", "10.0.0.1", "100.100.100.200", "::ffff:127.0.0.1", "0.0.0.0"} {
		if allowedAddress(net.ParseIP(ip), false) {
			t.Fatal(ip)
		}
	}
	if !allowedAddress(net.ParseIP("10.0.0.1"), true) {
		t.Fatal("explicit private broker option rejected")
	}
	if allowedAddress(net.ParseIP("169.254.169.254"), true) {
		t.Fatal("metadata allowed with private broker option")
	}
}

type fakeStore struct {
	events  []Event
	acked   bool
	success bool
}

func (f *fakeStore) ExpireReservations(context.Context) (int, error) { return 1, nil }
func (f *fakeStore) ClaimEvent(context.Context) (Event, bool, error) {
	if len(f.events) == 0 {
		return Event{}, false, nil
	}
	e := f.events[0]
	f.events = f.events[1:]
	return e, true, nil
}
func (f *fakeStore) CompleteEvent(_ context.Context, _ Event, success bool) error {
	f.acked = true
	f.success = success
	return nil
}

type failSender struct{}

func (failSender) Send(context.Context, Event) error { return errors.New("receiver unavailable") }
func TestDeliveryFailureSchedulesRetry(t *testing.T) {
	store := &fakeStore{events: []Event{{ID: "event"}}}
	w := Worker{Store: store, Sender: failSender{}}
	if _, err := w.DeliverOne(t.Context()); err == nil {
		t.Fatal("failed delivery hidden")
	}
	if !store.acked || store.success {
		t.Fatal("failed delivery acknowledged as success")
	}
}

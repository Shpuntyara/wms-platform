package jobs

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type adversarialJobStore struct {
	claim func(context.Context, int) ([]Event, error)
	ack   func(context.Context, Event, bool) error
}

func (*adversarialJobStore) ExpireReservations(context.Context) (int, error) { return 0, nil }
func (*adversarialJobStore) QueueState(context.Context) (QueueState, error)  { return QueueState{}, nil }
func (s *adversarialJobStore) ClaimEvents(ctx context.Context, n int) ([]Event, error) {
	return s.claim(ctx, n)
}
func (s *adversarialJobStore) CompleteEvent(ctx context.Context, e Event, ok bool) error {
	return s.ack(ctx, e, ok)
}

type adversarialJobSender func(context.Context, Event) error

func (s adversarialJobSender) Send(ctx context.Context, e Event) error { return s(ctx, e) }

func TestWorkerRejectsPartialFailedClaim(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var claims, sends, acknowledgements atomic.Int64
		store := &adversarialJobStore{
			claim: func(context.Context, int) ([]Event, error) {
				switch claims.Add(1) {
				case 1:
					return []Event{{ID: "unconfirmed", Token: "rolled-back-lease"}}, errors.New("claim result stream failed")
				case 2:
					return []Event{{ID: "unconfirmed", Token: "confirmed-new-lease"}}, nil
				}
				return nil, nil
			},
			ack: func(context.Context, Event, bool) error { acknowledgements.Add(1); return nil },
		}
		worker := Worker{Store: store, Sender: adversarialJobSender(func(context.Context, Event) error { sends.Add(1); return nil }), Concurrency: 1}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { defer close(done); worker.Run(ctx) }()
		synctest.Wait()
		initialSends, initialAcks, initialClaims := sends.Load(), acknowledgements.Load(), claims.Load()
		time.Sleep(99 * time.Millisecond)
		synctest.Wait()
		claimsBeforeRetry := claims.Load()
		time.Sleep(time.Millisecond)
		synctest.Wait()
		cancel()
		<-done
		if initialSends != 0 || initialAcks != 0 {
			t.Fatalf("an unsuccessful claim published unconfirmed work: sends=%d acknowledgements=%d", initialSends, initialAcks)
		}
		if initialClaims != 1 || claimsBeforeRetry != 1 {
			t.Fatalf("claim failure busy-polled before backoff: initial=%d before_retry=%d", initialClaims, claimsBeforeRetry)
		}
		if sends.Load() != 1 || acknowledgements.Load() != 1 {
			t.Fatalf("a later confirmed lease was lost: sends=%d acknowledgements=%d", sends.Load(), acknowledgements.Load())
		}
		if worker.Snapshot().RelayErrors != 1 {
			t.Fatal("failed claim was not recorded")
		}
	})
}

func TestWorkerCancellationReleasesEveryActiveSender(t *testing.T) {
	for _, concurrency := range []int{1, 4, 64} {
		t.Run(strconv.Itoa(concurrency), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var started, active, peak, completed atomic.Int64
				store := &adversarialJobStore{
					claim: func(_ context.Context, n int) ([]Event, error) {
						events := make([]Event, n)
						for i := range events {
							events[i] = Event{ID: "leased"}
						}
						return events, nil
					},
					ack: func(_ context.Context, _ Event, success bool) error {
						if success {
							t.Error("cancelled send acknowledged as delivered")
						}
						completed.Add(1)
						return nil
					},
				}
				sender := adversarialJobSender(func(ctx context.Context, _ Event) error {
					n := active.Add(1)
					for previous := peak.Load(); n > previous && !peak.CompareAndSwap(previous, n); previous = peak.Load() {
					}
					started.Add(1)
					defer active.Add(-1)
					<-ctx.Done()
					return ctx.Err()
				})
				worker := Worker{Store: store, Sender: sender, Concurrency: concurrency}
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan struct{})
				go func() { defer close(done); worker.Run(ctx) }()
				synctest.Wait()
				cancel()
				<-done
				if started.Load() != int64(concurrency) || peak.Load() != int64(concurrency) || active.Load() != 0 || completed.Load() != int64(concurrency) {
					t.Fatalf("sender lifecycle: started=%d peak=%d active=%d completed=%d concurrency=%d", started.Load(), peak.Load(), active.Load(), completed.Load(), concurrency)
				}
			})
		})
	}
}

func TestWorkerAckFailureDoesNotCountAsDelivery(t *testing.T) {
	store := &adversarialJobStore{
		claim: func(context.Context, int) ([]Event, error) { return []Event{{ID: "event"}}, nil },
		ack:   func(context.Context, Event, bool) error { return errors.New("lease fenced") },
	}
	worker := Worker{Store: store, Sender: adversarialJobSender(func(context.Context, Event) error { return nil })}
	if claimed, err := worker.DeliverOne(t.Context()); !claimed || err == nil {
		t.Fatalf("claimed=%v error=%v", claimed, err)
	}
	if snapshot := worker.Snapshot(); snapshot.Delivered != 0 || snapshot.DeliveryErrors != 1 {
		t.Fatalf("unconfirmed delivery counted: %+v", snapshot)
	}
}

type adversarialRoundTripper func(*http.Request) (*http.Response, error)

func (f adversarialRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSenderCopiesSigningKeyAndChecksResponseStatus(t *testing.T) {
	for _, status := range []int{200, 204, 299, 300, 400, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			key := []byte(strings.Repeat("k", 32))
			sender, err := NewSender("https://broker.example.invalid/events", key, false)
			if err != nil {
				t.Fatal(err)
			}
			key[0] = 'x'
			event := Event{ID: "signed-event", Payload: []byte("{\"event_id\":\"signed-event\"}")}
			sender.client.Transport = adversarialRoundTripper(func(r *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				mac := hmac.New(sha256.New, []byte(strings.Repeat("k", 32)))
				_, _ = mac.Write([]byte(r.Header.Get("X-WMS-Timestamp") + "\n" + event.ID + "\n"))
				_, _ = mac.Write(body)
				if !hmac.Equal([]byte(r.Header.Get("X-WMS-Signature")), []byte(hex.EncodeToString(mac.Sum(nil)))) {
					t.Error("caller mutation changed private signing key")
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
			})
			if err = sender.Send(t.Context(), event); (err == nil) != (status >= 200 && status < 300) {
				t.Fatalf("status=%d error=%v", status, err)
			}
		})
	}
}

func TestSenderInvalidEnvelopeNeverMakesRequest(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	sender, err := NewSender("https://broker.example.invalid/events", key, false)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	sender.client.Transport = adversarialRoundTripper(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, errors.New("should not dial") })
	for _, event := range []Event{{ID: ""}, {ID: "bad\rheader"}, {ID: "bad\nheader"}, {ID: "large", Payload: make([]byte, 1<<20+1)}} {
		if err = sender.Send(t.Context(), event); err == nil {
			t.Fatal("invalid event accepted")
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid envelope reached transport %d times", calls.Load())
	}
}

func TestWorkerFreshHealthExpiresDuringMonitoringFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now()
		worker := Worker{state: Status{RelayAt: now, ExpiryAt: now, MonitorAt: now}}
		if worker.Ready() != nil {
			t.Fatal("fresh health rejected")
		}
		time.Sleep(21 * time.Second)
		if worker.Ready() == nil {
			t.Fatal("stale health remained ready")
		}
	})
}

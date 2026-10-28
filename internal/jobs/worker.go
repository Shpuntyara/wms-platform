// Package jobs runs bounded inventory maintenance and an authenticated outbox relay.
package jobs

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Event struct {
	ID, Token string
	Payload   []byte
}
type Store interface {
	ExpireReservations(context.Context) (int, error)
	ClaimEvents(context.Context, int) ([]Event, error)
	CompleteEvent(context.Context, Event, bool) error
}
type Sender interface {
	Send(context.Context, Event) error
}

// Worker claims at most Concurrency leases at a time. No leased event waits
// behind another delivery; every nonempty batch is followed immediately.
type Worker struct {
	Store       Store
	Sender      Sender
	Logger      *slog.Logger
	Concurrency int
	mu          sync.RWMutex
	state       Status
}

type Status struct {
	RelayAt        time.Time  `json:"relay_at"`
	ExpiryAt       time.Time  `json:"expiry_at"`
	MonitorAt      time.Time  `json:"monitor_at"`
	Delivered      uint64     `json:"delivered"`
	DeliveryErrors uint64     `json:"delivery_errors"`
	Expired        uint64     `json:"expired"`
	RelayErrors    uint64     `json:"relay_errors"`
	ExpiryErrors   uint64     `json:"expiry_errors"`
	Queue          QueueState `json:"queue"`
}
type QueueState struct {
	Pending              int64   `json:"pending"`
	Dead                 int64   `json:"dead"`
	OldestPendingSeconds float64 `json:"oldest_pending_seconds"`
	ExpiredReservations  int64   `json:"expired_reservations"`
	OldestExpiredSeconds float64 `json:"oldest_expired_seconds"`
}
type MonitorStore interface {
	QueueState(context.Context) (QueueState, error)
}

func (w *Worker) Snapshot() Status { w.mu.RLock(); defer w.mu.RUnlock(); return w.state }

// Ready fails on stalled loops, failed monitoring, undeliverable events or an
// ageing backlog. Workers keep draining even when HTTP readiness is withdrawn.
func (w *Worker) Ready() error {
	s := w.Snapshot()
	now := time.Now()
	if s.RelayAt.IsZero() || now.Sub(s.RelayAt) > 20*time.Second || s.ExpiryAt.IsZero() || now.Sub(s.ExpiryAt) > 20*time.Second || s.MonitorAt.IsZero() || now.Sub(s.MonitorAt) > 20*time.Second || s.Queue.Dead > 0 || s.Queue.OldestPendingSeconds > 60 || s.Queue.OldestExpiredSeconds > 60 {
		return errors.New("background processing is unhealthy")
	}
	return nil
}
func (w *Worker) DeliverOne(ctx context.Context) (bool, error) {
	n, err := w.deliver(ctx, 1)
	return n > 0, err
}
func (w *Worker) send(ctx context.Context, event Event) error {
	sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	sendErr := w.Sender.Send(sendCtx, event)
	cancel()
	ackErr := w.Store.CompleteEvent(ctx, event, sendErr == nil)
	w.mu.Lock()
	defer w.mu.Unlock()
	if sendErr == nil && ackErr == nil {
		w.state.Delivered++
	} else {
		w.state.DeliveryErrors++
	}
	return errors.Join(sendErr, ackErr)
}
func (w *Worker) claim(ctx context.Context, limit int) ([]Event, error) {
	events, err := w.Store.ClaimEvents(ctx, limit)
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		w.state.RelayErrors++
		// A partial result may belong to a rolled-back claim transaction.
		// Publish only a batch whose lease acquisition was confirmed.
		events = nil
	} else {
		w.state.RelayAt = time.Now()
	}
	return events, err
}
func (w *Worker) deliver(ctx context.Context, limit int) (int, error) {
	events, err := w.claim(ctx, limit)
	if err != nil {
		return 0, err
	}
	var result error
	for _, event := range events {
		result = errors.Join(result, w.send(ctx, event))
	}
	return len(events), result
}

// relay leases only free delivery slots. A slow event holds its own slot while
// completed senders immediately accept new work; batches have no tail barrier.
func (w *Worker) relay(ctx context.Context, concurrency int) {
	slots := make(chan struct{}, concurrency)
	events := make(chan Event, concurrency)
	for range concurrency {
		slots <- struct{}{}
	}
	var senders sync.WaitGroup
	for range concurrency {
		senders.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case event := <-events:
					step, cancel := context.WithTimeout(ctx, 10*time.Second)
					_ = w.send(step, event)
					cancel()
					slots <- struct{}{}
				}
			}
		})
	}
	defer senders.Wait()
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case <-slots:
		}
		free := 1 + len(slots)
		for i := 1; i < free; i++ {
			<-slots
		}
		step, cancel := context.WithTimeout(ctx, 10*time.Second)
		batch, err := w.claim(step, free)
		cancel()
		for i := len(batch); i < free; i++ {
			slots <- struct{}{}
		}
		if err != nil && ctx.Err() == nil {
			w.logFailure("outbox_claim")
		}
		for _, event := range batch {
			select {
			case <-ctx.Done():
				return
			case events <- event:
			}
		}
		if (err != nil || len(batch) == 0) && !pause(ctx, 100*time.Millisecond) {
			return
		}
	}
}
func pause(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
func (w *Worker) Run(ctx context.Context) {
	concurrency := w.Concurrency
	if concurrency == 0 {
		concurrency = 16
	}
	if concurrency < 1 || concurrency > 64 {
		w.logFailure("invalid_concurrency")
		return
	}
	var wg sync.WaitGroup
	wg.Go(func() { w.relay(ctx, concurrency) })
	wg.Go(func() {
		for ctx.Err() == nil {
			step, cancel := context.WithTimeout(ctx, 10*time.Second)
			n, err := w.Store.ExpireReservations(step)
			cancel()
			w.mu.Lock()
			if err == nil {
				w.state.ExpiryAt = time.Now()
				w.state.Expired += uint64(n)
			} else {
				w.state.ExpiryErrors++
			}
			w.mu.Unlock()
			if err != nil && ctx.Err() == nil {
				w.logFailure("reservation_expiry")
			}
			if (err != nil || n < 100) && !pause(ctx, time.Second) {
				return
			}
		}
	})
	wg.Go(func() {
		monitor, ok := w.Store.(MonitorStore)
		if !ok {
			return
		}
		for ctx.Err() == nil {
			step, cancel := context.WithTimeout(ctx, 5*time.Second)
			queue, err := monitor.QueueState(step)
			cancel()
			if err == nil {
				w.mu.Lock()
				w.state.Queue = queue
				w.state.MonitorAt = time.Now()
				w.mu.Unlock()
				if w.Logger != nil {
					snapshot := w.Snapshot()
					w.Logger.Info("background processing", "delivered", snapshot.Delivered, "delivery_errors", snapshot.DeliveryErrors, "claim_errors", snapshot.RelayErrors, "expiry_errors", snapshot.ExpiryErrors, "pending", queue.Pending, "dead", queue.Dead, "oldest_pending_seconds", queue.OldestPendingSeconds, "expired_reservations", queue.ExpiredReservations, "oldest_expired_seconds", queue.OldestExpiredSeconds)
				}
			} else if ctx.Err() == nil {
				w.logFailure("background_monitor")
			}
			if !pause(ctx, 5*time.Second) {
				return
			}
		}
	})
	wg.Wait()
}
func (w *Worker) logFailure(name string) {
	if w.Logger != nil {
		w.Logger.Error("background job failed", "job", name)
	}
}

type HTTPSender struct {
	endpoint string
	key      []byte
	client   *http.Client
}

func NewSender(endpoint string, key []byte, allowPrivate bool) (*HTTPSender, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(key) < 32 {
		return nil, errors.New("outbox requires a fixed HTTPS endpoint and at least 32 key bytes")
	}
	dialer := &net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, MaxIdleConns: 64, MaxIdleConnsPerHost: 64, MaxConnsPerHost: 64, IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: 4 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, errors.New("outbox DNS lookup failed")
		}
		// Validate the actual dial destination on every new connection, including DNS changes.
		for _, a := range addresses {
			if !allowedAddress(a.IP, allowPrivate) {
				return nil, errors.New("outbox destination is outside allowed networks")
			}
		}
		for _, a := range addresses {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(a.IP.String(), port))
			if err == nil {
				return conn, nil
			}
		}
		return nil, errors.New("outbox connection failed")
	}
	return &HTTPSender{endpoint: endpoint, key: bytes.Clone(key), client: &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

var excludedNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("::/96"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"), netip.MustParsePrefix("2002::/16"),
}

func allowedAddress(ip net.IP, allowPrivate bool) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsUnspecified() || addr.IsPrivate() && !allowPrivate {
		return false
	}
	for _, network := range excludedNetworks {
		if network.Contains(addr) {
			return false
		}
	}
	return true
}

func (s *HTTPSender) Send(ctx context.Context, event Event) error {
	if event.ID == "" || strings.ContainsAny(event.ID, "\r\n") || len(event.Payload) > 1<<20 {
		return errors.New("invalid outbox event")
	}
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, s.key)
	_, _ = io.WriteString(mac, stamp+"\n"+event.ID+"\n")
	_, _ = mac.Write(event.Payload)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(event.Payload))
	if err != nil {
		return errors.New("invalid relay request")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-WMS-Event-ID", event.ID)
	request.Header.Set("X-WMS-Timestamp", stamp)
	request.Header.Set("X-WMS-Signature", hex.EncodeToString(mac.Sum(nil)))
	response, err := s.client.Do(request)
	if err != nil {
		return errors.New("outbox delivery failed")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 8192))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("outbox receiver status %d", response.StatusCode)
	}
	return nil
}

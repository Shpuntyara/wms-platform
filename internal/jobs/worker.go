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
	ClaimEvent(context.Context) (Event, bool, error)
	CompleteEvent(context.Context, Event, bool) error
}
type Sender interface {
	Send(context.Context, Event) error
}
type Worker struct {
	Store  Store
	Sender Sender
	Logger *slog.Logger
}

func (w *Worker) DeliverOne(ctx context.Context) (bool, error) {
	event, found, err := w.Store.ClaimEvent(ctx)
	if err != nil || !found {
		return false, err
	}
	sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	sendErr := w.Sender.Send(sendCtx, event)
	cancel()
	// A cancelled process leaves its lease to expire; never acknowledge blindly.
	ackErr := w.Store.CompleteEvent(ctx, event, sendErr == nil)
	return true, errors.Join(sendErr, ackErr)
}

// Run keeps expiry independent of receiver latency. Transient failures remain
// durable in PostgreSQL and are logged without payloads, credentials or URLs.
func (w *Worker) Run(ctx context.Context) {
	var wg sync.WaitGroup
	loop := func(interval time.Duration, name string, step func(context.Context) error) {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := step(callCtx)
			cancel()
			if err != nil && ctx.Err() == nil && w.Logger != nil {
				w.Logger.Error("background job failed", "job", name)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}
	wg.Add(2)
	go loop(time.Second, "reservation_expiry", func(ctx context.Context) error { _, err := w.Store.ExpireReservations(ctx); return err })
	go loop(100*time.Millisecond, "outbox", func(ctx context.Context) error { _, err := w.DeliverOne(ctx); return err })
	wg.Wait()
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
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: 4 * time.Second}
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

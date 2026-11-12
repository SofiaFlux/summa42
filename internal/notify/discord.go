package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// WebhookSender posts a message to a Discord webhook. The webhook URL is a
// secret: it is read from a private file and never logged.
type WebhookSender struct {
	url    string
	client *http.Client
}

var discordHosts = map[string]bool{"discord.com": true, "discordapp.com": true, "ptb.discord.com": true, "canary.discord.com": true}

// NewDiscordSender reads the webhook URL from a regular file that only its
// owner can read, and accepts only an https Discord webhook address.
func NewDiscordSender(path string) (*WebhookSender, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("webhook file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("webhook file must be a regular file with mode 0600 (it holds a secret)")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("webhook file: %w", err)
	}
	parsed, err := url.Parse(strings.TrimSpace(string(raw)))
	if err != nil || parsed.Scheme != "https" || !discordHosts[parsed.Hostname()] || !strings.HasPrefix(parsed.Path, "/api/webhooks/") || parsed.User != nil {
		return nil, errors.New("webhook file must hold one https://discord.com/api/webhooks/... address")
	}
	return &WebhookSender{url: parsed.String(), client: &http.Client{Timeout: 20 * time.Second}}, nil
}

// Send posts the message. Mentions are switched off, so no text can ping
// anyone. The error never contains the webhook address.
func (s *WebhookSender) Send(ctx context.Context, message string) (Outcome, error) {
	body, err := json.Marshal(map[string]any{
		"content":          message,
		"allowed_mentions": map[string]any{"parse": []string{}},
	})
	if err != nil {
		return NotSent, err
	}
	var connected atomic.Bool
	trace := &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { connected.Store(true) }}
	request, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return NotSent, errors.New("build webhook request")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		// Until a connection is handed to the request (GotConn fires after
		// dial, proxy and TLS setup), nothing of the request was written, so
		// the failure carried nothing: a dial or DNS error, or a timeout or
		// cancel while still connecting. The trace belongs to the request
		// context, so a redirect inherits the flag: a failure after an earlier
		// hop reached a connection stays Unknown. Anything later may have
		// delivered the message before it failed.
		if !connected.Load() {
			return NotSent, errors.New("webhook: could not connect")
		}
		return Unknown, errors.New("webhook: the request failed after it was sent")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16))
	switch {
	case response.StatusCode >= 200 && response.StatusCode < 300:
		return Delivered, nil
	case response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500:
		// The receiver answered that it did not take the message.
		return NotSent, fmt.Errorf("webhook: receiver answered %d", response.StatusCode)
	default:
		// A 4xx will not get better by itself (a deleted webhook, a bad
		// request); it is not retried forever.
		return Unknown, fmt.Errorf("webhook: receiver refused with %d", response.StatusCode)
	}
}

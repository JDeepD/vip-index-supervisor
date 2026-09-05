// Package notify publishes best-effort ntfy alerts without blocking indexing.
package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type Config struct {
	Endpoint    string `json:"endpoint"`
	Token       string `json:"token,omitempty"`
	RetryAlerts bool   `json:"retry_alerts"`
}

func (c Config) Validate() error {
	if c.Endpoint == "" {
		return nil
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Hostname() == "" || u.Opaque != "" {
		return errors.New("enter an absolute ntfy topic URL")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || !(strings.EqualFold(u.Hostname(), "localhost") || (ip != nil && ip.IsLoopback())) {
			return errors.New("ntfy requires HTTPS (HTTP is allowed only for localhost testing)")
		}
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("use a topic URL without credentials, query parameters, or fragments; enter the token separately")
	}
	if strings.Trim(u.Path, "/") == "" || strings.HasSuffix(u.Path, "/") || strings.ContainsAny(u.Path, "\r\n\t") {
		return errors.New("the ntfy URL must include a topic, e.g. https://ntfy.sh/your-private-topic")
	}
	if strings.IndexFunc(c.Token, unicode.IsSpace) >= 0 || strings.IndexFunc(c.Token, unicode.IsControl) >= 0 {
		return errors.New("the access token cannot contain whitespace or control characters")
	}
	return nil
}

type Message struct {
	Title    string
	Body     string
	Priority int
	Tags     string
}

const requestTimeout = 4 * time.Second

type Publisher struct {
	cfg    Config
	client *http.Client
}

func NewPublisher(cfg Config) (*Publisher, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Publisher{cfg: cfg, client: &http.Client{
		Timeout: requestTimeout,
		// A redirect must never forward the secret topic or bearer token.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (p *Publisher) Publish(ctx context.Context, message Message) error {
	if p.cfg.Endpoint == "" {
		return nil
	}
	body := message.Body
	if len(body) > 3500 {
		body = body[:3500]
		for !utf8.ValidString(body) {
			body = body[:len(body)-1]
		}
		body += "\n(truncated)"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.Endpoint, strings.NewReader(body))
	if err != nil {
		return errors.New("could not create ntfy request")
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	req.Header.Set("Title", strings.NewReplacer("\r", " ", "\n", " ").Replace(message.Title))
	priority := message.Priority
	if priority < 1 || priority > 5 {
		priority = 3
	}
	req.Header.Set("Priority", fmt.Sprint(priority))
	if message.Tags != "" {
		req.Header.Set("Tags", message.Tags)
	}
	if p.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.cfg.Token)
	}
	res, err := p.client.Do(req)
	if err != nil {
		// net/http errors contain URLs; never put the secret topic into logs.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			return errors.New("ntfy request timed out")
		}
		return errors.New("ntfy request failed (check connectivity and TLS)")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("ntfy returned HTTP %d", res.StatusCode)
	}
	// Bound response draining, including a proxy returning an endless body.
	_, err = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	if err != nil {
		return errors.New("ntfy response could not be read")
	}
	return nil
}

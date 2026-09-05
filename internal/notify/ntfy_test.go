package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func TestEndpointValidation(t *testing.T) {
	for _, endpoint := range []string{"", "https://ntfy.sh/private-topic", "https://notify.example.com/prefix/topic", "http://127.0.0.1:8080/test", "http://[::1]:8080/test"} {
		if err := (Config{Endpoint: endpoint}).Validate(); err != nil {
			t.Errorf("%q: %v", endpoint, err)
		}
	}
	for _, endpoint := range []string{"topic", "https://ntfy.sh", "https://ntfy.sh/", "https://ntfy.sh/topic/", "http://ntfy.sh/topic", "ftp://example.com/topic", "https://user:pass@example.com/topic", "https://example.com/topic?auth=secret", "https://example.com/topic#fragment"} {
		if err := (Config{Endpoint: endpoint}).Validate(); err == nil {
			t.Errorf("unsafe endpoint accepted: %q", endpoint)
		}
	}
	if err := (Config{Endpoint: "https://example.com/topic", Token: "tk_secret\r\nInjected: yes"}).Validate(); err == nil {
		t.Fatal("header injection accepted")
	}
}

func TestPublishUsesNtfyProtocol(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != "POST" || r.URL.Path != "/private-topic" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		for key, want := range map[string]string{"Authorization": "Bearer tk_test", "Title": "Index supervisor: Run completed", "Priority": "3", "Tags": "white_check_mark"} {
			if got := r.Header.Get(key); got != want {
				t.Errorf("%s=%q, want %q", key, got, want)
			}
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "Target: @fake.local\nDone." {
			t.Errorf("body: %q", body)
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"event":"message","id":"local-test-only"}`)
	}))
	defer server.Close()
	p, err := NewPublisher(Config{Endpoint: server.URL + "/private-topic", Token: "tk_test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Publish(context.Background(), Message{Title: "Index supervisor: Run completed", Body: "Target: @fake.local\nDone.", Priority: 3, Tags: "white_check_mark"}); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatal("wrong request count")
	}
}

func TestPublishDoesNotFollowRedirectOrLeakSecrets(t *testing.T) {
	var redirected atomic.Int32
	trap := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer trap.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, trap.URL+"/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	p, _ := NewPublisher(Config{Endpoint: server.URL + "/secret-topic", Token: "secret-token"})
	err := p.Publish(context.Background(), Message{Body: "test"})
	if err == nil || !strings.Contains(err.Error(), "307") {
		t.Fatalf("redirect not rejected: %v", err)
	}
	if redirected.Load() != 0 {
		t.Fatal("credentials could have followed redirect")
	}
	for _, secret := range []string{"secret-topic", "secret-token", server.URL} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("secret in error")
		}
	}
}

func TestPublishTimeoutAndHTTPFailure(t *testing.T) {
	for _, status := range []int{401, 429, 500} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			io.WriteString(w, "secret response body")
		}))
		p, _ := NewPublisher(Config{Endpoint: server.URL + "/secret-topic"})
		err := p.Publish(context.Background(), Message{Body: "test"})
		server.Close()
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe HTTP result: %v", err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.Copy(io.Discard, r.Body); <-r.Context().Done() }))
	defer server.Close()
	p, _ := NewPublisher(Config{Endpoint: server.URL + "/secret-topic"})
	p.client.Timeout = 50 * time.Millisecond
	started := time.Now()
	err := p.Publish(context.Background(), Message{Body: "test"})
	if err == nil || time.Since(started) > time.Second || strings.Contains(err.Error(), "secret-topic") {
		t.Fatalf("timeout: %v", err)
	}
}

func TestMessageSizeAndUTF8(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if len(body) > 4096 || !utf8.Valid(body) || !strings.HasSuffix(string(body), "(truncated)") {
			t.Errorf("invalid truncated body (%d bytes)", len(body))
		}
	}))
	defer server.Close()
	p, _ := NewPublisher(Config{Endpoint: server.URL + "/test"})
	if err := p.Publish(context.Background(), Message{Body: strings.Repeat("📱", 2000)}); err != nil {
		t.Fatal(err)
	}
}

func TestDispatcherRateLimitsRetryAlerts(t *testing.T) {
	received := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { body, _ := io.ReadAll(r.Body); received <- string(body) }))
	defer server.Close()
	d, err := New(Config{Endpoint: server.URL + "/test", RetryAlerts: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close(context.Background())
	now := time.Now()
	d.now = func() time.Time { return now }
	d.Retry("post", Message{Body: "first"})
	awaitBody(t, received, "first")
	d.Retry("post", Message{Body: "suppressed"})
	d.Retry("user", Message{Body: "other phase"})
	awaitBody(t, received, "other phase")
	now = now.Add(time.Minute)
	d.Retry("post", Message{Body: "later"})
	awaitBody(t, received, "later")
}

func TestDispatcherQueueIsBoundedAndFinalIsReserved(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	received := make(chan string, 128)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) == "first" {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		received <- string(body)
	}))
	defer server.Close()
	d, _ := New(Config{Endpoint: server.URL + "/test"})
	d.Send(Message{Body: "first"})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("publisher did not start")
	}
	before := time.Now()
	for i := 0; i < 1000; i++ {
		d.Send(Message{Body: "intermediate"})
	}
	if time.Since(before) > time.Second {
		t.Fatal("notification queue blocked caller")
	}
	d.Final(Message{Body: "final"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan Report, 1)
	go func() { done <- d.Close(ctx) }()
	<-d.closing
	close(release)
	awaitBody(t, received, "first")
	awaitBody(t, received, "final")
	report := <-done
	if report.Failed != 0 || report.Dropped == 0 {
		t.Fatalf("unexpected report: %+v", report)
	}
}

func TestDispatcherShutdownIsBounded(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	d, _ := New(Config{Endpoint: server.URL + "/test"})
	d.Send(Message{Body: "slow"})
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	before := time.Now()
	report := d.Close(ctx)
	if time.Since(before) > time.Second || report.Failed == 0 {
		t.Fatalf("shutdown was not bounded: %+v", report)
	}
}

func TestDispatcherFinalCannotBeFollowedByOutdatedAlerts(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	received := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) == "first" {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		received <- string(body)
	}))
	defer server.Close()
	d, _ := New(Config{Endpoint: server.URL + "/test"})
	defer d.Close(context.Background())
	d.Send(Message{Body: "first"})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("publisher did not start")
	}
	d.Send(Message{Body: "outdated phase completion"})
	d.Final(Message{Body: "final"})
	close(release)
	// Final must end delivery even if the caller has not reached Close yet.
	select {
	case <-d.done:
	case <-time.After(2 * time.Second):
		t.Fatal("final notification did not finish delivery")
	}
	d.Send(Message{Body: "too late"})
	awaitBody(t, received, "first")
	awaitBody(t, received, "final")
	if len(received) != 0 || len(d.queue) != 1 {
		t.Fatal("outdated alerts delivered after final or late alert accepted")
	}
	if report := d.Close(context.Background()); report.Dropped != 1 || report.Failed != 0 {
		t.Fatalf("unexpected final report: %+v", report)
	}
}

func TestDispatcherCanDisableRetryAlerts(t *testing.T) {
	received := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- string(body)
	}))
	defer server.Close()
	d, _ := New(Config{Endpoint: server.URL + "/test", RetryAlerts: false})
	defer d.Close(context.Background())
	d.Retry("post", Message{Body: "must be suppressed"})
	d.Send(Message{Body: "ordinary alert"})
	awaitBody(t, received, "ordinary alert")
	d.Final(Message{Body: "final"})
	if report := d.Close(context.Background()); report.Failed != 0 {
		t.Fatalf("notification failed: %+v", report)
	}
	awaitBody(t, received, "final")
	if len(received) != 0 {
		t.Fatal("retry alerts not disabled")
	}
}

func TestDisabledNotifications(t *testing.T) {
	d, err := New(Config{})
	if err != nil || d != nil {
		t.Fatal("empty endpoint not disabled")
	}
	d.Send(Message{})
	d.Retry("post", Message{})
	d.Final(Message{})
	d.Close(context.Background())
}

func TestSettingsRoundTripAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "notifications.json")
	if cfg, err := Load(path); err != nil || cfg.Endpoint != "" {
		t.Fatalf("missing settings: %+v %v", cfg, err)
	}
	cfg := Config{Endpoint: "https://example.com/private-topic", Token: "tk_test", RetryAlerts: true}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil || got != cfg {
		t.Fatalf("round trip failed: %v", err)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0600 {
			t.Fatalf("settings permissions: %v", info.Mode())
		}
	}
	if err := Save(path, Config{Token: "must-not-be-saved"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "must-not-be-saved") {
		t.Fatal("disabled settings retained token")
	}
	if err := os.WriteFile(path, []byte(`{"token":"private"`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || strings.Contains(err.Error(), "private") {
		t.Fatal("malformed settings exposed token or were accepted")
	}
}

func awaitBody(t *testing.T, received <-chan string, want string) {
	t.Helper()
	select {
	case got := <-received:
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("did not receive %q", want)
	}
}

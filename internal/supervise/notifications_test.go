package supervise

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jdeepd/vip-index-supervisor/internal/notify"
)

func TestNotificationsForRunOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, title string
		code        int
	}{
		{"success", "Run completed", 0}, {"failure", "Run failed", 1}, {"stop", "Run interrupted", 130},
		{"preflight failure", "Run failed", 2}, {"cancelled before start", "Run interrupted", 130},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type alert struct{ title, body, priority string }
			alerts := make(chan alert, 16)
			started := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				alerts <- alert{r.Header.Get("Title"), string(body), r.Header.Get("Priority")}
				if strings.Contains(r.Header.Get("Title"), "Run started") {
					started <- struct{}{}
				}
			}))
			defer server.Close()
			s := testSupervisor(t)
			s.cfg.Target.AppEnv = "@fake.local" // fakeSearch and fake attempt below; never executed
			s.cfg.Notifications = notify.Config{Endpoint: server.URL + "/local-only", Token: "not-a-real-token", RetryAlerts: true}
			s.attempt = func(context.Context, string, int, []string) attemptOutcome {
				select {
				case <-started:
				case <-time.After(2 * time.Second):
					t.Fatal("start alert missing")
				}
				switch tc.name {
				case "stop":
					s.RequestStop(false)
					return attemptOutcome{}
				case "failure":
					return attemptOutcome{fatal: "private CLI output must never be forwarded"}
				default:
					return attemptOutcome{success: true, indexed: 1000}
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.name == "cancelled before start" {
				cancel()
			}
			if tc.name == "preflight failure" {
				s.client.(*fakeSearch).rows = nil
			}
			s.Run(ctx)
			code := -1
			for e := range s.Events() {
				if done, ok := e.(DoneEvent); ok {
					code = done.ExitCode
				}
			}
			if code != tc.code {
				t.Fatalf("exit=%d, want %d", code, tc.code)
			}
			found := false
			for len(alerts) > 0 {
				a := <-alerts
				if strings.Contains(a.body, "private CLI output") || strings.Contains(a.body, "not-a-real-token") || strings.Contains(a.body, "local-only") {
					t.Fatalf("sensitive content in alert: %q", a.body)
				}
				if a.title == "Index supervisor: "+tc.title {
					found = true
					if !strings.Contains(a.body, "@fake.local") {
						t.Fatal("target missing")
					}
					if tc.code == 1 || tc.code == 2 {
						if a.priority != "4" {
							t.Fatal("failure not high priority")
						}
					}
				}
			}
			if !found {
				t.Fatalf("final notification %q missing", tc.title)
			}
		})
	}
}

func TestNotificationFailureDoesNotChangeIndexingResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer server.Close()
	s := testSupervisor(t)
	s.cfg.Notifications = notify.Config{Endpoint: server.URL + "/private-topic"}
	s.attempt = func(context.Context, string, int, []string) attemptOutcome {
		return attemptOutcome{success: true, indexed: 1}
	}
	s.Run(context.Background())
	code := -1
	warned := false
	for e := range s.Events() {
		switch e := e.(type) {
		case DoneEvent:
			code = e.ExitCode
		case LogEvent:
			if strings.Contains(e.Message, "could not be delivered") {
				warned = true
			}
			if strings.Contains(e.Message, "private-topic") {
				t.Fatal("topic leaked to logs")
			}
		}
	}
	if code != 0 || !warned {
		t.Fatalf("notification failure affected run or was hidden: exit=%d warned=%v", code, warned)
	}
}

func TestMalformedNotificationConfigDoesNotAbortRun(t *testing.T) {
	s := testSupervisor(t)
	s.cfg.Notifications = notify.Config{Endpoint: "invalid-endpoint"}
	s.attempt = func(context.Context, string, int, []string) attemptOutcome {
		return attemptOutcome{success: true, indexed: 1}
	}
	s.Run(context.Background())
	for e := range s.Events() {
		if done, ok := e.(DoneEvent); ok && done.ExitCode != 0 {
			t.Fatalf("notification config aborted indexing: %+v", done)
		}
	}
}

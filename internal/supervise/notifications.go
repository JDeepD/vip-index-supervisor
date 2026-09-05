package supervise

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jdeepd/vip-index-supervisor/internal/notify"
)

func (s *Supervisor) startNotifications() {
	var err error
	s.notifications, err = notify.New(s.cfg.Notifications)
	if err != nil {
		s.logf(LevelWarn, "ntfy notifications disabled: %v", err)
	}
}

func (s *Supervisor) closeNotifications() {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	report := s.notifications.Close(ctx)
	if report.Failed > 0 {
		s.logf(LevelWarn, "ntfy: %d notification(s) could not be delivered: %v (indexing result unchanged)", report.Failed, report.LastError)
	}
	if report.Dropped > 0 {
		s.logf(LevelInfo, "ntfy: %d intermediate alert(s) omitted to keep delivery bounded", report.Dropped)
	}
}

// Never forward raw CLI output, command arguments, credentials, or the ntfy
// topic. Alerts contain only the chosen target and supervisor progress.
func (s *Supervisor) notificationMessage(title, body string, priority int, tags string) notify.Message {
	var detail strings.Builder
	detail.WriteString("Target: " + s.cfg.Target.Label() + "\n" + body)
	s.mu.Lock()
	if s.current >= 0 && s.current < len(s.phases) {
		p := s.phases[s.current]
		fmt.Fprintf(&detail, "\nPhase: %s, version: %d, attempt: %d", p.Name, p.Version, p.Attempt)
		if p.LastObjectID > 0 {
			fmt.Fprintf(&detail, "\nLast checkpoint ID: %d", p.LastObjectID)
		}
	}
	s.mu.Unlock()
	return notify.Message{Title: "Index supervisor: " + title, Body: detail.String(), Priority: priority, Tags: tags}
}

func (s *Supervisor) notifyChange(title, body string, priority int, tags string) {
	if s.notifications == nil {
		return
	}
	s.notifications.Send(s.notificationMessage(title, body, priority, tags))
}

func (s *Supervisor) notifyRetry(indexable, body string) {
	if s.notifications == nil {
		return
	}
	s.notifications.Retry(indexable, s.notificationMessage("Retrying", body, 3, "warning"))
}

func (s *Supervisor) finish(event DoneEvent) {
	event = s.finishHistory(event)
	if s.notifications != nil {
		title, body, priority, tags := "Run failed", "Indexing did not complete. Check the supervisor logs before resuming.", 4, "rotating_light"
		switch event.ExitCode {
		case 0:
			title, body, priority, tags = "Run completed", "100% complete. All indexing phases completed, including any required verification and activation.", 3, "white_check_mark"
		case 130:
			title, body, priority, tags = "Run interrupted", "Indexing was interrupted. Inspect the saved checkpoint before resuming.", 3, "pause_button"
		}
		body += fmt.Sprintf("\nExit code: %d", event.ExitCode)
		s.notifications.Final(s.notificationMessage(title, body, priority, tags))
	}
	s.events <- event
}

// Percentages come from reported object counts, never from object IDs. The
// high-water mark is persisted before enqueueing (best-effort, at most once
// across recovery), and 100% is reserved for verified phase completion.
func (s *Supervisor) progressMilestones() error {
	var crossed []int
	s.mu.Lock()
	p := &s.phases[s.current]
	if s.notifications != nil && p.Total > 0 && p.Done >= 0 {
		for _, percent := range []int{25, 50, 75} {
			// ceil(total*percent/100), without overflowing int64.
			threshold := (p.Total/100)*int64(percent) + ((p.Total%100)*int64(percent)+99)/100
			if p.NotifiedPercent < percent && p.Done >= threshold {
				p.NotifiedPercent = percent
				crossed = append(crossed, percent)
			}
		}
	}
	name, done, total := p.Name, p.Done, p.Total
	s.mu.Unlock()
	if err := s.persistHistory(); err != nil {
		return err
	}
	for _, percent := range crossed {
		s.notifyChange(fmt.Sprintf("%d%% — %s", percent, name),
			fmt.Sprintf("%s reached %d%% of the saved phase workload (%s / %s objects).", name, percent, formatInt(done), formatInt(total)), 3, "chart_with_upwards_trend")
	}
	return nil
}

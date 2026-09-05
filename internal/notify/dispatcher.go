package notify

import (
	"context"
	"sync"
	"time"
)

// Dispatcher drops excess intermediate alerts, rate-limits retries, and gives
// the final outcome its own queue. It does not retry failed HTTP requests:
// ambiguous network failures can otherwise produce duplicate phone alerts.
type Dispatcher struct {
	publisher *Publisher
	ctx       context.Context
	cancel    context.CancelFunc
	queue     chan Message
	terminal  chan Message
	closing   chan struct{}
	done      chan struct{}
	once      sync.Once
	mu        sync.Mutex
	lastRetry map[string]time.Time
	now       func() time.Time
	failed    int
	dropped   int
	lastErr   error
}

type Report struct {
	Failed, Dropped int
	LastError       error
}

func New(cfg Config) (*Dispatcher, error) {
	if cfg.Endpoint == "" {
		return nil, nil
	}
	publisher, err := NewPublisher(cfg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &Dispatcher{publisher: publisher, ctx: ctx, cancel: cancel,
		queue: make(chan Message, 32), terminal: make(chan Message, 1), closing: make(chan struct{}), done: make(chan struct{}),
		lastRetry: make(map[string]time.Time), now: time.Now}
	go d.run()
	return d, nil
}

func (d *Dispatcher) Send(message Message) {
	if d == nil {
		return
	}
	select {
	case <-d.closing:
		return
	default:
	}
	select {
	case d.queue <- message:
	default:
		d.mu.Lock()
		d.dropped++
		d.mu.Unlock()
	}
}

func (d *Dispatcher) Retry(key string, message Message) {
	if d == nil || !d.publisher.cfg.RetryAlerts {
		return
	}
	d.mu.Lock()
	now := d.now()
	last, seen := d.lastRetry[key]
	if seen && now.Sub(last) < time.Minute {
		d.mu.Unlock()
		return
	}
	d.lastRetry[key] = now
	d.mu.Unlock()
	d.Send(message)
}

func (d *Dispatcher) Final(message Message) {
	if d == nil {
		return
	}
	select {
	case d.terminal <- message:
	default:
	}
}

func (d *Dispatcher) deliver(message Message) {
	if err := d.publisher.Publish(d.ctx, message); err != nil {
		d.mu.Lock()
		d.failed++
		d.lastErr = err
		d.mu.Unlock()
	}
}

func (d *Dispatcher) run() {
	defer close(d.done)
	defer func() {
		d.once.Do(func() { close(d.closing) })
		d.mu.Lock()
		d.dropped += len(d.queue)
		d.mu.Unlock()
	}()
	for {
		select {
		case m := <-d.terminal:
			d.deliver(m)
			return
		default:
		}
		select {
		case <-d.ctx.Done():
			return
		case <-d.closing:
			select {
			case m := <-d.terminal:
				d.deliver(m)
			default:
			}
			return
		case m := <-d.terminal:
			d.deliver(m)
			return
		case m := <-d.queue:
			d.deliver(m)
		}
	}
}

// Close waits only for in-flight work and the final result, not a backlog of
// retries. The caller supplies a small shutdown budget independent of the run's
// cancelled context, so an interrupted run can still notify its operator.
func (d *Dispatcher) Close(ctx context.Context) Report {
	if d == nil {
		return Report{}
	}
	d.once.Do(func() { close(d.closing) })
	select {
	case <-d.done:
	case <-ctx.Done():
		d.cancel()
		<-d.done
	}
	d.cancel()
	d.mu.Lock()
	defer d.mu.Unlock()
	return Report{Failed: d.failed, Dropped: d.dropped, LastError: d.lastErr}
}

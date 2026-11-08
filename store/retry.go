package store

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/ncruces/go-sqlite3"
)

// A reader that holds the store for longer than the busy timeout, as a slow
// query of the gateway's can, makes writes fail with SQLITE_BUSY until it
// lets go. Such a write changed nothing and can run again: Retry and Queue
// run it a second later, then twice as long after each failure, at most a
// minute apart, until it lands.
const (
	firstRetry = time.Second
	maxRetry   = time.Minute
	// maxQueued bounds the writes a Queue keeps, so that a store locked
	// for hours cannot use up the connector's memory.
	maxQueued = 10000
)

// Busy reports whether err is the store being locked past the busy timeout.
func Busy(err error) bool { return errors.Is(err, sqlite3.BUSY) }

// Retry runs write until it returns anything but a Busy error, or ctx is
// done.
func Retry(ctx context.Context, write func() error) error {
	for wait := firstRetry; ; wait = min(2*wait, maxRetry) {
		err := write()
		if !Busy(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}
	}
}

// Queue keeps the writes of what a service sends only once, such as
// Discord's gateway events, until the store takes them. A write runs at once
// unless others wait; one that finds the store busy waits in memory, and the
// writes after it wait behind it, so an edit never lands before its message.
type Queue struct {
	log    *slog.Logger
	again  string
	max    int
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	waiting []queued      // the first is being retried
	dropped int           // writes refused since the store went busy
	done    chan struct{} // closed when the goroutine retrying them returns
}

type queued struct {
	what  string
	write func(context.Context) error
}

// NewQueue logs to log. again says what stores a write again after a
// restart, for the writes a shutdown or a full queue leaves out.
func NewQueue(log *slog.Logger, again string) *Queue {
	ctx, cancel := context.WithCancel(context.Background())
	return &Queue{log: log, again: again, max: maxQueued, ctx: ctx, cancel: cancel}
}

// Do runs write, or queues it behind the writes waiting for the store, and
// reports whether it is stored now. A write that fails otherwise is logged
// and dropped, as is one the full queue has no room for. what names it in
// the log by IDs, never by content.
func (q *Queue) Do(what string, write func(ctx context.Context) error) (stored bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.waiting) == 0 {
		err := write(q.ctx)
		if !Busy(err) || q.ctx.Err() != nil {
			if err != nil {
				q.log.Error("not stored", "write", what, "err", err)
			}
			return err == nil
		}
		q.log.Warn("store busy; keeping writes until it is free", "write", what)
		q.done = make(chan struct{})
		go q.retry(q.done)
	}
	if len(q.waiting) >= q.max {
		if q.dropped == 0 {
			q.log.Error("store busy too long; dropping writes past the "+strconv.Itoa(q.max)+" kept until it is free; "+q.again,
				"write", what)
		}
		q.dropped++
		return false
	}
	q.waiting = append(q.waiting, queued{what, write})
	return false
}

func (q *Queue) retry(done chan struct{}) {
	defer close(done)
	stored := 0
	for wait := firstRetry; ; {
		select {
		case <-q.ctx.Done():
			return
		case <-time.After(wait):
		}
		q.mu.Lock()
		w := q.waiting[0]
		q.mu.Unlock()
		err := w.write(q.ctx)
		if Busy(err) || err != nil && q.ctx.Err() != nil {
			wait = min(max(2*wait, firstRetry), maxRetry)
			continue
		}
		if err != nil {
			q.log.Error("not stored", "write", w.what, "err", err)
		} else {
			stored++
		}
		wait = 0
		q.mu.Lock()
		q.waiting[0] = queued{}
		q.waiting = q.waiting[1:]
		left, dropped := len(q.waiting), q.dropped
		if left == 0 {
			q.dropped = 0
		}
		q.mu.Unlock()
		if left == 0 {
			q.log.Info("store free again; stored the writes kept", "count", stored, "dropped", dropped)
			return
		}
	}
}

// Close stops retrying and logs each write still waiting. It returns nil,
// so a Queue closes with a program's other io.Closers.
func (q *Queue) Close() error {
	q.cancel()
	q.mu.Lock()
	done := q.done
	q.mu.Unlock()
	if done != nil {
		<-done
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, w := range q.waiting {
		q.log.Warn("not stored before shutdown; "+q.again, "write", w.what)
	}
	q.waiting = nil
	return nil
}

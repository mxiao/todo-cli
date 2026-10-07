package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mxiao/todo-cli/packages/core"
)

// Events are derived from the task history table, whose ids form the
// store-wide revision. Polling it picks up writes from every process (CLI,
// TUI, this server) without any IPC; the server's own writes additionally
// wake the watcher immediately.

const (
	subscriberBuffer = 256
	replayLimit      = 1000
)

// ChangeEvent is the payload of a "task" event.
type ChangeEvent struct {
	Revision int64                  `json:"revision"`
	TaskID   string                 `json:"task_id"`
	Action   string                 `json:"action"`
	Actor    string                 `json:"actor"`
	Changes  map[string]core.Change `json:"changes"`
	At       time.Time              `json:"at"`
	// Task is the task's state when the event was sent (possibly newer than
	// the change itself); null when the task no longer exists.
	Task *core.Task `json:"task"`
}

type sseEvent struct {
	id   int64
	name string
	data []byte
}

type subscriber struct {
	ch chan sseEvent
}

type hub struct {
	store   *core.Store
	poll    time.Duration
	log     *slog.Logger
	kick    chan struct{}
	done    chan struct{}
	stopped chan struct{}
	once    sync.Once

	mu     sync.Mutex
	subs   map[*subscriber]struct{}
	closed bool
	last   int64 // touched only by run
}

func newHub(store *core.Store, poll time.Duration, log *slog.Logger) *hub {
	h := &hub{store: store, poll: poll, log: log, kick: make(chan struct{}, 1), done: make(chan struct{}),
		stopped: make(chan struct{}), subs: map[*subscriber]struct{}{}}
	h.last, _ = store.Revision()
	go h.run()
	return h
}

func (h *hub) kickNow() {
	select {
	case h.kick <- struct{}{}:
	default:
	}
}

// close stops the watcher (waiting for an in-flight poll, so the store can
// be closed afterwards) and ends every subscription.
func (h *hub) close() {
	h.once.Do(func() {
		close(h.done)
		<-h.stopped
		h.mu.Lock()
		h.closed = true
		for sub := range h.subs {
			close(sub.ch)
			delete(h.subs, sub)
		}
		h.mu.Unlock()
	})
}

func (h *hub) subscribe() (*subscriber, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, false
	}
	sub := &subscriber{ch: make(chan sseEvent, subscriberBuffer)}
	h.subs[sub] = struct{}{}
	return sub, true
}

func (h *hub) unsubscribe(sub *subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[sub]; ok {
		delete(h.subs, sub)
		close(sub.ch)
	}
}

func (h *hub) run() {
	defer close(h.stopped)
	t := time.NewTicker(h.poll)
	defer t.Stop()
	for {
		select {
		case <-h.done:
			return
		case <-t.C:
		case <-h.kick:
		}
		h.check()
	}
}

// check broadcasts every change committed since the last check.
func (h *hub) check() {
	rev, err := h.store.Revision()
	if err != nil {
		h.log.Warn("event watcher: read revision", "err", err)
		return
	}
	for rev > h.last {
		entries, err := h.store.ChangesSince(h.last, replayLimit)
		if err != nil {
			h.log.Warn("event watcher: read changes", "err", err)
			return
		}
		if len(entries) == 0 {
			return
		}
		for _, e := range entries {
			ev, err := buildEvent(h.store, e)
			if err != nil {
				h.log.Warn("event watcher: build event", "err", err)
				return
			}
			h.broadcast(ev)
			h.last = e.ID
		}
	}
}

func (h *hub) broadcast(ev sseEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		select {
		case sub.ch <- ev:
		default:
			// Too slow: drop the stream; the browser reconnects with
			// Last-Event-ID and gets the missed changes replayed.
			delete(h.subs, sub)
			close(sub.ch)
		}
	}
}

func buildEvent(store *core.Store, e core.HistoryEntry) (sseEvent, error) {
	ce := ChangeEvent{Revision: e.ID, TaskID: e.TaskID, Action: e.Action, Actor: e.Actor, Changes: e.Changes, At: e.CreatedAt}
	t, err := store.Get(e.TaskID)
	switch {
	case err == nil:
		ce.Task = t
	case !errors.Is(err, core.ErrNotFound):
		return sseEvent{}, err
	}
	b, err := json.Marshal(ce)
	if err != nil {
		return sseEvent{}, err
	}
	return sseEvent{id: e.ID, name: "task", data: b}, nil
}

func writeEvent(w http.ResponseWriter, ev sseEvent) error {
	var b strings.Builder
	fmt.Fprintf(&b, "id: %d\nevent: %s\ndata: %s\n\n", ev.id, ev.name, ev.data)
	_, err := fmt.Fprint(w, b.String())
	return err
}

func jsonEvent(id int64, name string, v any) sseEvent {
	b, _ := json.Marshal(v)
	return sseEvent{id: id, name: name, data: b}
}

// handleEvents streams task changes as server-sent events:
//
//	event: ready   data: {"revision": N}         sent first; id = N
//	event: task    data: ChangeEvent              one per change; id = its revision
//	event: resync  data: {"revision": N}          too many missed changes: reload everything
//
// Reconnecting clients (Last-Event-ID header, or ?since=N) first receive
// the changes they missed.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	since := int64(-1)
	for _, v := range []string{r.Header.Get("Last-Event-ID"), r.URL.Query().Get("since")} {
		if v == "" {
			continue
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			s.fail(w, r, fmt.Errorf("%w: event id must be a non-negative integer", core.ErrInvalid))
			return
		}
		since = n
		break
	}
	sub, ok := s.hub.subscribe()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "shutting_down", "the server is shutting down")
		return
	}
	defer s.hub.unsubscribe(sub)
	// Subscribe before reading the revision so nothing falls in between.
	rev, err := s.store.Revision()
	if err != nil {
		s.fail(w, r, err)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	send := func(ev sseEvent) bool {
		if writeEvent(w, ev) != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if _, err := fmt.Fprint(w, "retry: 1000\n\n"); err != nil {
		return
	}

	sent := rev
	if since > rev {
		// The client is ahead of this database (e.g. it was restored from
		// a backup): its state cannot be patched up incrementally.
		if !send(jsonEvent(rev, "resync", map[string]int64{"revision": rev})) {
			return
		}
	} else if since >= 0 && since < rev {
		entries, err := s.store.ChangesSince(since, replayLimit+1)
		if err != nil {
			s.log.Warn("event replay", "err", err)
			entries = nil
			since = -1
		}
		if len(entries) > replayLimit || since < 0 {
			if !send(jsonEvent(rev, "resync", map[string]int64{"revision": rev})) {
				return
			}
		} else {
			for _, e := range entries {
				if e.ID > rev {
					break
				}
				ev, err := buildEvent(s.store, e)
				if err != nil {
					s.log.Warn("event replay", "err", err)
					break
				}
				if !send(ev) {
					return
				}
			}
		}
	}
	if !send(jsonEvent(rev, "ready", map[string]int64{"revision": rev})) {
		return
	}

	beat := time.NewTicker(s.opts.Heartbeat)
	defer beat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-sub.ch:
			if !ok {
				return
			}
			if ev.id <= sent {
				continue // already delivered by the replay
			}
			sent = ev.id
			if !send(ev) {
				return
			}
		case <-beat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}

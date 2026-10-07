// Package server is the local todo-cli web service: a REST API over the
// same SQLite store the CLI uses, plus a server-sent event stream that
// reports every task change (from the web, the CLI, the TUI or any other
// process) within the poll interval.
//
// The service only ever listens on a loopback address and has no login;
// it rejects requests whose Host or Origin is not the local server, which
// blocks DNS-rebinding and cross-site requests from other web pages.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mxiao/todo-cli/packages/core"
)

const (
	// DefaultHost is the only kind of address the server binds to.
	DefaultHost = "127.0.0.1"
	// DefaultPort is tried first; the next ports are tried when it is taken.
	DefaultPort = 3210
	// DefaultPortAttempts is how many consecutive ports Listen tries.
	DefaultPortAttempts = 20
	// DefaultPollInterval is how often the event stream checks the database
	// for changes committed by other processes.
	DefaultPollInterval = 200 * time.Millisecond
	// DefaultHeartbeat keeps idle event streams alive through proxies.
	DefaultHeartbeat = 15 * time.Second

	maxBodyBytes = 1 << 20
)

var (
	// ErrNotLoopback rejects bind addresses that would expose the server
	// beyond this machine.
	ErrNotLoopback = errors.New("not_loopback")
	// ErrPortsInUse means every port Listen tried was taken.
	ErrPortsInUse = errors.New("ports_in_use")
)

// Options tunes a Server; zero values use the defaults.
type Options struct {
	PollInterval time.Duration
	Heartbeat    time.Duration
	Logger       *slog.Logger
	// Version is reported by /api/health.
	Version string
}

// Server serves the REST API and event stream for one store.
type Server struct {
	store *core.Store
	opts  Options
	log   *slog.Logger
	mux   *http.ServeMux
	hub   *hub
}

// New builds a server over store and starts its change watcher; call Close
// to stop it.
func New(store *core.Store, opts Options) *Server {
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultPollInterval
	}
	if opts.Heartbeat <= 0 {
		opts.Heartbeat = DefaultHeartbeat
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &Server{store: store, opts: opts, log: log, mux: http.NewServeMux()}
	s.hub = newHub(store, opts.PollInterval, log)
	s.routes()
	return s
}

// Close stops the change watcher and ends every open event stream.
func (s *Server) Close() { s.hub.close() }

// ServeHTTP applies the local-access guard and dispatches to the routes.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	if !isLoopbackHost(r.Host) {
		writeError(w, http.StatusForbidden, "forbidden_host", "only local requests to 127.0.0.1/localhost are served")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
		if o := r.Header.Get("Origin"); o != "" && !sameOrigin(o, r.Host) {
			writeError(w, http.StatusForbidden, "forbidden_origin", "cross-origin writes are not allowed")
			return
		}
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			writeError(w, http.StatusForbidden, "forbidden_origin", "cross-site writes are not allowed")
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

func isLoopbackName(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	return host != "" && isLoopbackName(host)
}

func sameOrigin(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" {
		return false
	}
	return strings.EqualFold(u.Host, host) && isLoopbackHost(u.Host)
}

// CheckHost validates a bind host: only loopback addresses are allowed.
// "localhost" binds 127.0.0.1.
func CheckHost(host string) (string, error) {
	host = strings.TrimSpace(host)
	switch {
	case host == "":
		return DefaultHost, nil
	case strings.EqualFold(host, "localhost"):
		return DefaultHost, nil
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("%w: refusing to listen on %q: only loopback addresses (127.0.0.1, ::1, localhost) are supported; LAN access is not available", ErrNotLoopback, host)
	}
	return ip.String(), nil
}

// Listen binds host:port, moving on to the next port while the current one
// is in use, up to attempts ports in total. Port 0 picks a free port.
func Listen(host string, port, attempts int) (net.Listener, error) {
	host, err := CheckHost(host)
	if err != nil {
		return nil, err
	}
	if port < 0 || port > 65535 {
		return nil, fmt.Errorf("%w: port %d out of range", core.ErrInvalid, port)
	}
	if attempts < 1 || port == 0 {
		attempts = 1
	}
	last := min(port+attempts-1, 65535)
	for p := port; p <= last; p++ {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(p)))
		if err == nil {
			return ln, nil
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			return nil, fmt.Errorf("listen on %s: %w", net.JoinHostPort(host, strconv.Itoa(p)), err)
		}
	}
	return nil, fmt.Errorf("%w: ports %d-%d on %s are all in use; pass another --port", ErrPortsInUse, port, last, host)
}

// URL is the browser address for a listener.
func URL(ln net.Listener) string {
	addr := ln.Addr().(*net.TCPAddr)
	return "http://" + net.JoinHostPort(addr.IP.String(), strconv.Itoa(addr.Port)) + "/"
}

// Serve runs the server on ln until ctx is cancelled, then shuts down
// gracefully (event streams are closed first so shutdown never hangs).
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	hs := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	hs.RegisterOnShutdown(s.Close)
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	select {
	case err := <-errc:
		s.Close()
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := hs.Shutdown(shutdown)
	<-errc
	return err
}

// ---- JSON helpers ----

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": apiError{Code: code, Message: msg}})
}

// errorStatus maps store errors to HTTP status codes and API error codes.
func errorStatus(err error) (int, string) {
	switch {
	case errors.Is(err, core.ErrConflict):
		return http.StatusConflict, "version_conflict"
	case errors.Is(err, core.ErrNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, core.ErrAmbiguousID):
		return http.StatusBadRequest, "ambiguous_id"
	case errors.Is(err, core.ErrInvalid):
		return http.StatusBadRequest, "invalid_input"
	case errors.Is(err, core.ErrDeleted):
		return http.StatusGone, "task_deleted"
	case errors.Is(err, core.ErrNothingToUndo):
		return http.StatusConflict, "nothing_to_undo"
	case errors.Is(err, core.ErrConflictResolved):
		return http.StatusConflict, "conflict_resolved"
	case errors.Is(err, errVersionRequired):
		return http.StatusPreconditionRequired, "version_required"
	case errors.Is(err, errBadJSON):
		return http.StatusBadRequest, "invalid_json"
	case errors.Is(err, errMediaType):
		return http.StatusUnsupportedMediaType, "unsupported_media_type"
	case errors.Is(err, errTooLarge):
		return http.StatusRequestEntityTooLarge, "body_too_large"
	}
	return http.StatusInternalServerError, "internal_error"
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, code := errorStatus(err)
	msg := err.Error()
	if status == http.StatusInternalServerError {
		s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		msg = "internal error; see the server log"
	}
	writeError(w, status, code, msg)
}

var (
	errVersionRequired = errors.New("version_required")
	errBadJSON         = errors.New("invalid_json")
	errMediaType       = errors.New("unsupported_media_type")
	errTooLarge        = errors.New("body_too_large")
)

// readBody returns the raw JSON request body ("{}" when empty).
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, fmt.Errorf("%w: request body exceeds %d bytes", errTooLarge, maxBodyBytes)
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return []byte("{}"), nil
	}
	ct := r.Header.Get("Content-Type")
	if mt, _, _ := strings.Cut(ct, ";"); !strings.EqualFold(strings.TrimSpace(mt), "application/json") {
		return nil, fmt.Errorf("%w: send the body as application/json", errMediaType)
	}
	if !json.Valid(b) {
		return nil, fmt.Errorf("%w: request body is not valid JSON", errBadJSON)
	}
	return b, nil
}

// decodeBody strictly decodes the JSON body into v.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) ([]byte, error) {
	b, err := readBody(w, r)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return nil, fmt.Errorf("%w: %v", core.ErrInvalid, err)
	}
	return b, nil
}

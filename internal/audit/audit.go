// Package audit writes one JSON line per API request: when, which Caller, what
// operation on which path, the outcome, bytes and duration. It never sees a Credential:
// the Caller is recorded by name only.
package audit

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

// Line is one audit log line.
type Line struct {
	Time       time.Time `json:"time"`
	Caller     string    `json:"caller,omitempty"` // empty when authentication failed
	Operation  string    `json:"operation"`
	Path       string    `json:"path"`
	Outcome    string    `json:"outcome"`
	Status     int       `json:"status,omitempty"` // absent for the service's own actions
	Bytes      int64     `json:"bytes"`
	DurationMS int64     `json:"duration_ms"`
}

// Logger writes audit Lines to an io.Writer (stdout in production).
type Logger struct {
	mu  sync.Mutex
	enc *json.Encoder
	now func() time.Time
}

// New returns a Logger writing to w.
func New(w io.Writer) *Logger {
	return &Logger{enc: json.NewEncoder(w), now: time.Now}
}

func (l *Logger) write(line Line) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.enc.Encode(line) // stdout failing is not a reason to fail the request
}

// Event logs an action the service took on its own, outside any request (e.g. removing
// an abandoned Upload): caller is whose data it acted on.
func (l *Logger) Event(caller, operation, path, outcome string, bytes int64) {
	l.write(Line{Time: l.now().UTC(), Caller: caller, Operation: operation, Path: path, Outcome: outcome, Bytes: bytes})
}

// Record collects what a request did, for its audit line. Handlers fill it in through
// FromContext; anything left unset gets a default from the request and response.
type Record struct {
	mu        sync.Mutex
	caller    string
	operation string
	path      string
	outcome   string
	bytes     int64
	bytesSet  bool
}

type ctxKey struct{}

// FromContext returns the request's Record. Outside Middleware it returns a Record that
// is discarded, so callers never need a nil check.
func FromContext(ctx context.Context) *Record {
	if r, ok := ctx.Value(ctxKey{}).(*Record); ok {
		return r
	}
	return &Record{}
}

// SetCaller records the authenticated Caller's name. Never pass a token.
func (r *Record) SetCaller(name string) { r.set(func() { r.caller = name }) }

// SetOperation names the operation, e.g. "list".
func (r *Record) SetOperation(op string) { r.set(func() { r.operation = op }) }

// SetPath records the Upload Root-relative path the request acted on (uploadroot.Path
// String form). Defaults to the raw request path.
func (r *Record) SetPath(p string) { r.set(func() { r.path = p }) }

// SetOutcome overrides the outcome derived from the status code.
func (r *Record) SetOutcome(o string) { r.set(func() { r.outcome = o }) }

// SetBytes records the payload bytes (e.g. bytes written to the Upload Root).
// Defaults to the response body size.
func (r *Record) SetBytes(n int64) { r.set(func() { r.bytes, r.bytesSet = n, true }) }

func (r *Record) set(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f()
}

// Middleware logs one Line per request through next.
func (l *Logger) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		start := l.now()
		rec := &Record{}
		cw := &countingWriter{ResponseWriter: w}
		next.ServeHTTP(cw, req.WithContext(context.WithValue(req.Context(), ctxKey{}, rec)))

		status := cw.status
		if status == 0 {
			status = http.StatusOK
		}
		rec.mu.Lock()
		line := Line{
			Time:       start.UTC(),
			Caller:     rec.caller,
			Operation:  orDefault(rec.operation, req.Method),
			Path:       orDefault(rec.path, req.URL.EscapedPath()),
			Outcome:    orDefault(rec.outcome, outcomeOf(status)),
			Status:     status,
			Bytes:      cw.n,
			DurationMS: l.now().Sub(start).Milliseconds(),
		}
		if rec.bytesSet {
			line.Bytes = rec.bytes
		}
		rec.mu.Unlock()
		l.write(line)
	})
}

func outcomeOf(status int) string {
	switch {
	case status < 400:
		return "ok"
	case status == http.StatusUnauthorized:
		return "unauthorized"
	case status == http.StatusForbidden:
		return "forbidden"
	case status == http.StatusNotFound:
		return "not_found"
	case status < 500:
		return "rejected"
	default:
		return "error"
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

type countingWriter struct {
	http.ResponseWriter
	status int
	n      int64
}

func (w *countingWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *countingWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.n += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer (flush, deadlines).
func (w *countingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

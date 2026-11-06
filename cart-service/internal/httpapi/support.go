package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"gocart/cart-service/internal/reqid"
)

// Errors are RFC 9457 problem details (application/problem+json). The type is a
// URN rather than a URL: it names the kind of problem without pretending that
// a documentation page exists at that address.
const problemTypePrefix = "urn:gocart:problem:"

// Problem is an RFC 9457 problem document with optional extension members.
type Problem struct {
	Type   string
	Title  string
	Status int
	Detail string
	Extra  map[string]any
}

// MarshalJSON flattens Extra into the document. Standard members win over
// extensions of the same name.
func (p Problem) MarshalJSON() ([]byte, error) {
	m := map[string]any{
		"type":   problemTypePrefix + p.Type,
		"title":  p.Title,
		"status": p.Status,
	}
	if p.Detail != "" {
		m["detail"] = p.Detail
	}
	for k, v := range p.Extra {
		if _, taken := m[k]; !taken {
			m[k] = v
		}
	}
	return json.Marshal(m)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeProblem(w http.ResponseWriter, r *http.Request, p Problem) {
	if id := reqid.From(r.Context()); id != "" {
		if p.Extra == nil {
			p.Extra = map[string]any{}
		}
		p.Extra["request_id"] = id
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}

func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(reqid.Header)
		if !reqid.Valid(id) {
			id = reqid.New()
		}
		w.Header().Set(reqid.Header, id)
		next.ServeHTTP(w, r.WithContext(reqid.With(r.Context(), id)))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func accessLog(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		level := slog.LevelInfo
		if r.URL.Path == "/healthz" {
			level = slog.LevelDebug // probes would otherwise drown the log
		}
		log.Log(r.Context(), level, "request",
			"method", r.Method, "path", r.URL.Path, "status", rec.status,
			"bytes", rec.bytes, "duration_ms", time.Since(start).Milliseconds(),
			"request_id", reqid.From(r.Context()))
	})
}

func recoverer(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				log.Error("panic in handler", "panic", v, "request_id", reqid.From(r.Context()))
				writeProblem(w, r, Problem{Type: "internal", Title: "Internal error", Status: http.StatusInternalServerError})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

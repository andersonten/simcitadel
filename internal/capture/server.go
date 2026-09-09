package capture

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"
)

// Server provides HTTP handlers for the event capture query API.
type Server struct {
	buf    *Buffer
	status func() map[string]string
	logger *slog.Logger
}

// NewServer creates a server with the given buffer, status function, and logger.
func NewServer(buf *Buffer, status func() map[string]string, logger *slog.Logger) *Server {
	return &Server{
		buf:    buf,
		status: status,
		logger: logger,
	}
}

// Handler returns an http.Handler that routes requests to the correct handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /captured", s.handleCaptured)
	mux.HandleFunc("GET /captured/count", s.handleCapturedCount)
	mux.HandleFunc("DELETE /captured", s.handleDeleteCaptured)
	mux.HandleFunc("GET /health", s.handleHealth)
	return mux
}

func (s *Server) handleCaptured(w http.ResponseWriter, r *http.Request) {
	f := ParseFilter(r)
	messages := s.buf.Query(f)
	if messages == nil {
		messages = []Message{}
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(messages); err != nil {
		s.logger.Error("encode captured response", "error", err)
	}
}

func (s *Server) handleCapturedCount(w http.ResponseWriter, r *http.Request) {
	f := ParseFilter(r)
	count := s.buf.Count(f)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]int{"count": count}); err != nil {
		s.logger.Error("encode count response", "error", err)
	}
}

func (s *Server) handleDeleteCaptured(w http.ResponseWriter, _ *http.Request) {
	s.buf.Clear()
	w.WriteHeader(http.StatusNoContent)
}

// handleHealth answers the container health check. An exchange that is not bound
// captures nothing, and preflight of SDD section 4.10 refuses to run against
// that state, so the service answers 503 rather than 200. SDD section 11.1
// builds the start-up order out of service_healthy conditions, and a service
// that always answers 200 makes every such condition pass the moment the
// listener opens.
//
// A configuration that names no exchange is healthy. The service knows the list
// it was given and nothing else, so citadel doctor owns the comparison against
// capture.exchanges of the config.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	exchanges := s.status()

	var unbound []string
	for name, state := range exchanges {
		if state != exchangeStateBound {
			unbound = append(unbound, name)
		}
	}

	status, code := "ok", http.StatusOK
	if len(unbound) > 0 {
		status, code = "degraded", http.StatusServiceUnavailable
		sort.Strings(unbound)
		s.logger.Warn("health: exchanges are not bound", "exchanges", strings.Join(unbound, ","))
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	resp := map[string]any{
		"status":    status,
		"exchanges": exchanges,
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.logger.Error("encode health response", "error", err)
	}
}

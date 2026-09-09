package trigger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	citadelamqp "github.com/andersonten/simcitadel/internal/amqp"
	amqpdriver "github.com/rabbitmq/amqp091-go"
)

// MessagePublisher sends a message to an AMQP exchange.
type MessagePublisher interface {
	Publish(ctx context.Context, exchange, routingKey string, msg amqpdriver.Publishing) (*citadelamqp.PublishResult, error)
}

// DefaultConfirmTimeout is the publishConfirmTimeout default of SDD section 7.5.
const DefaultConfirmTimeout = 5 * time.Second

// confirmWindower reports the window a publisher waits for the broker confirm.
// The publisher of internal/amqp holds that window, and SDD section 7.5 gives it
// one name and one value, so the server reads it from the publisher rather than
// keep a second copy that a caller must remember to set.
type confirmWindower interface {
	ConfirmTimeout() time.Duration
}

// Server handles HTTP requests for the event trigger service.
type Server struct {
	publisher      MessagePublisher
	budget         *Budget
	connected      func() bool
	confirmTimeout time.Duration
}

// NewServer creates a new event trigger server. It takes the confirm window of
// SDD section 7.5 from the publisher when the publisher reports one, and falls
// back to the default. Call SetConfirmTimeout to bound a request by any other
// window.
func NewServer(pub MessagePublisher, budget *Budget, connected func() bool) *Server {
	confirmTimeout := DefaultConfirmTimeout
	if window, ok := pub.(confirmWindower); ok && window.ConfirmTimeout() > 0 {
		confirmTimeout = window.ConfirmTimeout()
	}
	return &Server{
		publisher:      pub,
		budget:         budget,
		connected:      connected,
		confirmTimeout: confirmTimeout,
	}
}

// SetConfirmTimeout sets the window a publish waits for the broker confirm.
// The value appears in the 502 message of SDD section 7.5, so it must equal the
// window the publisher enforces. Call it before the server starts to serve.
func (s *Server) SetConfirmTimeout(d time.Duration) {
	s.confirmTimeout = d
}

// Handler returns an http.Handler with all event trigger routes registered.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /publish", s.publishHandler)
	mux.HandleFunc("POST /publish-sequence", s.publishSequenceHandler)
	mux.HandleFunc("GET /health", s.healthHandler)
	mux.HandleFunc("POST /budget/reset", s.budgetResetHandler)
	return mux
}

// PublishProperties holds optional AMQP publishing properties.
type PublishProperties struct {
	DeliveryMode *uint8 `json:"deliveryMode"`
}

type publishRequest struct {
	Delivery   string             `json:"delivery"`
	Exchange   string             `json:"exchange"`
	RoutingKey string             `json:"routingKey"`
	Event      map[string]any     `json:"event"`
	Headers    map[string]string  `json:"headers"`
	Properties *PublishProperties `json:"properties"`
}

// publishResponse holds the success response for a single publish operation.
type publishResponse struct {
	Published bool   `json:"published"`
	Confirmed bool   `json:"confirmed"`
	Routed    bool   `json:"routed"`
	EventID   string `json:"eventId"`
}

func (s *Server) publishHandler(w http.ResponseWriter, r *http.Request) {
	var req publishRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	resp, code, err := s.executePublish(r, &req)
	if err != nil {
		writeJSON(w, code, errorBody(err))
		return
	}

	writeJSON(w, code, resp)
}

// executePublish validates and publishes a single request. It returns the
// response body, the HTTP status code, and an error for failure cases.
func (s *Server) executePublish(r *http.Request, req *publishRequest) (*publishResponse, int, error) {
	if req.Exchange == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("exchange is required")
	}
	if req.RoutingKey == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("routingKey is required")
	}
	if req.Event == nil {
		return nil, http.StatusBadRequest, fmt.Errorf("event is required")
	}

	if err := ValidateCloudEvents(req.Event); err != nil {
		return nil, http.StatusBadRequest, err
	}

	depth := 0
	if dh := r.Header.Get("X-Citadel-Depth"); dh != "" {
		parsed, parseErr := strconv.Atoi(dh)
		if parseErr == nil {
			depth = parsed
		}
	}
	if depth >= 10 {
		return nil, http.StatusBadRequest, fmt.Errorf("cycle depth exceeded")
	}

	runID := r.Header.Get("X-Citadel-Run")

	if !s.budget.Acquire(runID) {
		return nil, http.StatusTooManyRequests, fmt.Errorf("publish budget exhausted")
	}

	req.Event["citadelrun"] = runID

	eventID, _ := req.Event["id"].(string)

	body, err := json.Marshal(req.Event)
	if err != nil {
		slog.Error("marshal event", "error", err)
		return nil, http.StatusInternalServerError, fmt.Errorf("marshal event")
	}

	headers := make(amqpdriver.Table, len(req.Headers)+2)
	for k, v := range req.Headers {
		headers[k] = v
	}
	headers["citadeldepth"] = int32(depth + 1)
	if runID != "" {
		headers["citadelrun"] = runID
	}

	msg := amqpdriver.Publishing{
		ContentType: "application/json",
		Body:        body,
		Headers:     headers,
	}

	if req.Properties != nil && req.Properties.DeliveryMode != nil {
		msg.DeliveryMode = *req.Properties.DeliveryMode
	}

	publishCtx := r.Context()
	if s.confirmTimeout > 0 {
		var cancel context.CancelFunc
		publishCtx, cancel = context.WithTimeout(publishCtx, s.confirmTimeout)
		defer cancel()
	}

	start := time.Now()
	result, err := s.publisher.Publish(publishCtx, req.Exchange, req.RoutingKey, msg)
	if err != nil {
		if window, expired := confirmWindow(err, time.Since(start), s.confirmTimeout); expired {
			return nil, http.StatusBadGateway, fmt.Errorf("broker did not confirm within %s", window)
		}
		return nil, http.StatusBadGateway, err
	}

	if result.Returned {
		return nil, http.StatusUnprocessableEntity, newUnroutableError(req, result)
	}

	resp := &publishResponse{
		Published: true,
		Confirmed: true,
		Routed:    true,
		EventID:   eventID,
	}
	return resp, http.StatusAccepted, nil
}

func (s *Server) publishSequenceHandler(w http.ResponseWriter, r *http.Request) {
	var reqs []publishRequest
	if err := json.NewDecoder(r.Body).Decode(&reqs); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	results := make([]*publishResponse, 0, len(reqs))
	for i := range reqs {
		resp, errCode, pubErr := s.executePublish(r, &reqs[i])
		if pubErr != nil {
			body := errorBody(pubErr)
			body["index"] = strconv.Itoa(i)
			writeJSON(w, errCode, body)
			return
		}
		results = append(results, resp)
	}

	writeJSON(w, http.StatusAccepted, results)
}

// healthHandler answers the container health check. A trigger with no broker
// connection cannot publish, so it answers 503 and not 200. SDD section 11.1
// builds the start-up order out of service_healthy conditions, and
// internal/health.Probe accepts a 200 alone, so a degraded service that answers
// 200 reports healthy to Docker and the first step of the run fails instead.
//
// The body keeps the detail, because a developer needs to know which half is
// wrong. The status code carries the verdict.
func (s *Server) healthHandler(w http.ResponseWriter, _ *http.Request) {
	if s.connected() {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "broker": "connected"})
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "degraded", "broker": "disconnected"})
}

func (s *Server) budgetResetHandler(w http.ResponseWriter, r *http.Request) {
	runID := r.Header.Get("X-Citadel-Run")
	if runID == "" {
		var req struct {
			RunID string `json:"runId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		runID = req.RunID
	}
	s.budget.Reset(runID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// unroutableError reports a message the broker returned. SDD section 7.5 makes
// the exchange and the routing key part of the 422, because "unroutable" alone
// does not say which binding is missing.
type unroutableError struct {
	exchange   string
	routingKey string
}

func (e *unroutableError) Error() string { return "unroutable" }

// newUnroutableError takes the exchange and the routing key from the
// basic.return when the broker supplies it, and from the request when it does not.
func newUnroutableError(req *publishRequest, result *citadelamqp.PublishResult) *unroutableError {
	out := &unroutableError{exchange: req.Exchange, routingKey: req.RoutingKey}
	if result != nil && result.Return != nil {
		if result.Return.Exchange != "" {
			out.exchange = result.Return.Exchange
		}
		if result.Return.RoutingKey != "" {
			out.routingKey = result.Return.RoutingKey
		}
	}
	return out
}

// errorBody builds the failure body. The error key stays whatever the failure
// says, so a consumer that reads only that key keeps working.
func errorBody(err error) map[string]string {
	body := map[string]string{"error": err.Error()}
	var unroutable *unroutableError
	if errors.As(err, &unroutable) {
		body["exchange"] = unroutable.exchange
		body["routingKey"] = unroutable.routingKey
	}
	return body
}

// confirmWindow returns the window that expired and reports whether a publish
// failed because the broker did not confirm inside it.
//
// The publisher reports its own window on a typed error, which is the exact
// value SDD section 7.5 asks the 502 to name. The handler bounds the publish as
// well, so a deadline it enforces arrives as context.DeadlineExceeded. An
// elapsed time at or beyond the window is the same fact by a third route, for a
// publisher that reports neither signal. No test reads the words of the error,
// which change without notice.
func confirmWindow(err error, elapsed, window time.Duration) (time.Duration, bool) {
	var timeout *citadelamqp.ConfirmTimeoutError
	if errors.As(err, &timeout) {
		return timeout.Timeout, true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return window, true
	}
	if window > 0 && elapsed >= window {
		return window, true
	}
	return 0, false
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

package exampleapp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// Handler returns the HTTP routes. The readiness path matches the example config,
// which names /healthz, and the GraphQL path matches app.graphqlPath.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.handleHealth)
	mux.HandleFunc("POST /graphql", a.handleGraphQL)
	return mux
}

func (a *App) setDBReady(ready bool) {
	a.mu.Lock()
	a.dbReady = ready
	a.mu.Unlock()
}

func (a *App) setAMQPReady(ready bool) {
	a.mu.Lock()
	a.amqpBusy = ready
	a.mu.Unlock()
}

// handleHealth reports readiness. It answers 503 while any dependency is missing,
// for the reason the two sidecars answer 503: SDD section 11.1 builds the start-up
// order out of service_healthy conditions, and a service that always answers 200
// makes every such condition pass the moment the listener opens.
//
// Debezium depends on this endpoint. It resolves table.include.list against the
// live catalog when it starts and exits rather than wait, so it must not start
// before this app creates the outbox table.
func (a *App) handleHealth(w http.ResponseWriter, _ *http.Request) {
	a.mu.RLock()
	dbReady, consuming, processed := a.dbReady, a.amqpBusy, a.processed
	a.mu.RUnlock()

	status, code := "ok", http.StatusOK
	if !dbReady || !consuming {
		status, code = "degraded", http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":    status,
		"database":  readyWord(dbReady),
		"consumer":  readyWord(consuming),
		"processed": processed,
	})
}

func readyWord(ready bool) string {
	if ready {
		return "ready"
	}
	return "not-ready"
}

type graphqlBody struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

// handleGraphQL answers queries about the manuscripts the app created.
//
// This is a demonstration resolver and not a GraphQL server. It selects a
// resolver by looking for a field name in the query text, and it rejects anything
// it does not know with a GraphQL error. That is enough for a scenario to assert
// against application state through app/graphql, and it is honest about its
// limits: it does not parse the query, it does not validate against a schema, and
// it ignores an alias and a fragment.
func (a *App) handleGraphQL(w http.ResponseWriter, r *http.Request) {
	var body graphqlBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeGraphQLError(w, "the request body is not JSON: "+err.Error())
		return
	}

	switch {
	case strings.Contains(body.Query, "manuscriptByArticleId"):
		a.resolveManuscriptByArticleID(r.Context(), w, body.Variables)
	case strings.Contains(body.Query, "manuscripts"):
		a.resolveManuscripts(r.Context(), w)
	default:
		writeGraphQLError(w, "this example app resolves manuscripts and manuscriptByArticleId only")
	}
}

// manuscript is the shape both resolvers return.
type manuscript struct {
	ID        string `json:"id"`
	ArticleID string `json:"articleId"`
	Title     string `json:"title"`
	Status    string `json:"status"`
}

func (a *App) resolveManuscripts(ctx context.Context, w http.ResponseWriter) {
	rows, err := a.db.QueryContext(ctx,
		"SELECT id, article_id, title, status FROM public.manuscript ORDER BY created_at")
	if err != nil {
		writeGraphQLError(w, "read the manuscripts: "+err.Error())
		return
	}
	defer rows.Close()

	list := []manuscript{}
	for rows.Next() {
		var m manuscript
		if err := rows.Scan(&m.ID, &m.ArticleID, &m.Title, &m.Status); err != nil {
			writeGraphQLError(w, "read a manuscript row: "+err.Error())
			return
		}
		list = append(list, m)
	}
	if err := rows.Err(); err != nil {
		writeGraphQLError(w, "read the manuscript rows: "+err.Error())
		return
	}

	writeGraphQLData(w, map[string]any{"manuscripts": list})
}

func (a *App) resolveManuscriptByArticleID(ctx context.Context, w http.ResponseWriter, vars map[string]any) {
	articleID, _ := vars["articleId"].(string)
	if articleID == "" {
		writeGraphQLError(w, "manuscriptByArticleId needs the articleId variable as a string")
		return
	}

	var m manuscript
	err := a.db.QueryRowContext(ctx,
		"SELECT id, article_id, title, status FROM public.manuscript WHERE article_id = $1",
		articleID).Scan(&m.ID, &m.ArticleID, &m.Title, &m.Status)
	if err != nil {
		// An absent manuscript is a null field and not an error. A scenario that
		// asserts the app has not yet created one needs that distinction.
		writeGraphQLData(w, map[string]any{"manuscriptByArticleId": nil})
		return
	}

	writeGraphQLData(w, map[string]any{"manuscriptByArticleId": m})
}

// writeGraphQLData answers with the data envelope. The status is 200 even for a
// resolver error, which is what a GraphQL server does, so app/graphql steps read
// the errors array rather than the status code.
func writeGraphQLData(w http.ResponseWriter, data map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func writeGraphQLError(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data":   nil,
		"errors": []map[string]string{{"message": message}},
	})
}

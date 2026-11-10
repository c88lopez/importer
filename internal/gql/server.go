package gql

import (
	"context"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"os"

	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/lru"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"

	"github.com/vandelay-labs/importer/internal/ratelimit"
)

// maxComplexity caps how much work one query can ask for. Every field counts
// 1, so listing all items with all 6 fields costs 7. It stops a client from
// sending one huge query; it isn't tuned beyond that.
const maxComplexity = 200

// NewRouter serves GraphQL on /graphql, GraphiQL on GET / (to try queries in
// the browser), and the probes. readyz is the same readiness check as the
// API's. POST / is GraphQL too, so a client pointed at the bare host (as
// Postman suggests) works instead of getting 405. limit wraps the GraphQL
// endpoint (see RateLimit); nil means no limit.
func NewRouter(store itemStore, version string, readyz http.Handler, limit func(http.Handler) http.Handler) http.Handler {
	mux := http.NewServeMux()
	if limit == nil {
		limit = func(h http.Handler) http.Handler { return h }
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	})
	mux.Handle("GET /readyz", readyz)
	mux.Handle("GET /{$}", playground.Handler(fmt.Sprintf("importer-graphql %s (%s)", version, hostname()), "/graphql"))
	// Limited before anything else, so requests refused for their content
	// type or size count too.
	gql := limit(requireGraphQLContentType(http.MaxBytesHandler(NewHandler(store), 1<<20)))
	mux.Handle("/graphql", gql)
	mux.Handle("POST /{$}", gql)
	return mux
}

// NewHandler is the GraphQL endpoint: queries and mutations over POST (and
// queries over GET), with introspection on so GraphiQL can autocomplete.
func NewHandler(store itemStore) http.Handler {
	srv := handler.New(NewExecutableSchema(Config{Resolvers: &Resolver{store: store}}))
	srv.AddTransport(transport.Options{})
	srv.AddTransport(transport.GET{})
	srv.AddTransport(transport.POST{})
	// The query as the whole body: curl -H 'Content-Type: application/graphql' -d '{ items { id } }'
	srv.AddTransport(transport.GRAPHQL{})
	// Parsed and validated queries are cached by their text.
	srv.SetQueryCache(lru.New[*ast.QueryDocument](1000))
	srv.Use(extension.Introspection{})
	srv.Use(extension.FixedComplexityLimit(maxComplexity))
	srv.Use(newOperationTelemetry())
	srv.SetRecoverFunc(func(ctx context.Context, p any) error {
		return internalError(ctx, fmt.Errorf("panic: %v", p))
	})
	return srv
}

// requireGraphQLContentType answers a POST that isn't JSON or
// application/graphql with 415 and what to send, instead of gqlgen's bare
// "transport not supported". curl -d without -H sends a form type, so this is
// the usual first mistake.
//
// Form posts stay refused on purpose: a browser lets any web page send one
// to another site without asking first, so any page you visit could call
// deleteItem here (CSRF). It must ask the server before sending JSON or
// application/graphql, and this server doesn't say yes.
func requireGraphQLContentType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if mediaType != "application/json" && mediaType != "application/graphql" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnsupportedMediaType)
				_, _ = fmt.Fprintf(w, `{"errors":[{"message":%q}]}`+"\n",
					"POST needs Content-Type: application/json (body {\"query\": \"...\"}) or application/graphql (body is the query)")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// RateLimit returns the middleware that limits the GraphQL endpoint per
// client IP, answering 429 with a GraphQL-shaped error (code RATE_LIMITED)
// past the limit.
func RateLimit(l ratelimit.Allower) func(http.Handler) http.Handler {
	return ratelimit.Middleware(l, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"errors":[{"message":"rate limit exceeded","extensions":{"code":"RATE_LIMITED"}}]}` + "\n"))
	})
}

// userError is an error the client caused and can fix. It's returned as is,
// with extensions.code BAD_USER_INPUT, like gqlgen's own validation errors.
func userError(ctx context.Context, msg string, field ...string) error {
	return &gqlerror.Error{
		Message:    msg,
		Path:       graphql.GetPath(ctx),
		Extensions: map[string]any{"code": "BAD_USER_INPUT", "field": field},
	}
}

// internalError logs err (a database error, a panic) with its trace and
// returns a bare "internal error" in its place, so details don't leak to
// clients: the same rule as the REST API's 500s. Resolvers wrap every store
// error in it. GraphQL's own errors (bad syntax, unknown fields, wrong
// argument types) and userError reach the client as they are.
func internalError(ctx context.Context, err error) error {
	slog.ErrorContext(ctx, "resolver error", "path", graphql.GetPath(ctx).String(), "err", err)
	return &gqlerror.Error{
		Message:    "internal error",
		Path:       graphql.GetPath(ctx),
		Extensions: map[string]any{"code": "INTERNAL_SERVER_ERROR"},
	}
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

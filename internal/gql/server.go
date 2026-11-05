package gql

import (
	"context"
	"fmt"
	"log/slog"
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
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// maxComplexity caps how much work one query can ask for. Every field counts
// 1, so listing all items with all 6 fields costs 7. It stops a client from
// sending one huge query; it isn't tuned beyond that.
const maxComplexity = 200

// NewRouter serves GraphQL on /graphql, GraphiQL on / (to try queries in the
// browser), and the probes. readyz is the same readiness check as the API's.
func NewRouter(store itemStore, version string, readyz http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	})
	mux.Handle("GET /readyz", readyz)
	mux.Handle("GET /{$}", playground.Handler(fmt.Sprintf("importer-graphql %s (%s)", version, hostname()), "/graphql"))
	mux.Handle("/graphql", http.MaxBytesHandler(NewHandler(store), 1<<20))
	return mux
}

// NewHandler is the GraphQL endpoint: queries and mutations over POST (and
// queries over GET), with introspection on so GraphiQL can autocomplete.
func NewHandler(store itemStore) http.Handler {
	srv := handler.New(NewExecutableSchema(Config{Resolvers: &Resolver{store: store}}))
	srv.AddTransport(transport.Options{})
	srv.AddTransport(transport.GET{})
	srv.AddTransport(transport.POST{})
	// Parsed and validated queries are cached by their text.
	srv.SetQueryCache(lru.New[*ast.QueryDocument](1000))
	srv.Use(extension.Introspection{})
	srv.Use(extension.FixedComplexityLimit(maxComplexity))
	srv.Use(operationTracer{tracer: otel.Tracer("github.com/vandelay-labs/importer/internal/gql")})
	srv.SetRecoverFunc(func(ctx context.Context, p any) error {
		return internalError(ctx, fmt.Errorf("panic: %v", p))
	})
	return srv
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

// operationTracer adds a span per operation, named like "query Items", under
// the request's "POST /graphql" span, so traces of different operations can
// be told apart. Resolvers (and their database spans) run inside it.
type operationTracer struct {
	tracer trace.Tracer
}

var _ interface {
	graphql.HandlerExtension
	graphql.OperationInterceptor
} = operationTracer{}

func (operationTracer) ExtensionName() string                   { return "OperationTracer" }
func (operationTracer) Validate(graphql.ExecutableSchema) error { return nil }

func (t operationTracer) InterceptOperation(ctx context.Context, next graphql.OperationHandler) graphql.ResponseHandler {
	oc := graphql.GetOperationContext(ctx)
	opType := "query"
	if oc.Operation != nil {
		opType = string(oc.Operation.Operation)
	}
	name := opType
	if oc.OperationName != "" {
		name += " " + oc.OperationName
	}
	ctx, span := t.tracer.Start(ctx, name, trace.WithAttributes(
		attribute.String("graphql.operation.type", opType),
		attribute.String("graphql.operation.name", oc.OperationName),
		attribute.String("graphql.document", oc.RawQuery),
	))
	responses := next(ctx)
	return func(ctx context.Context) *graphql.Response {
		resp := responses(ctx)
		if resp != nil && len(resp.Errors) > 0 {
			span.SetStatus(codes.Error, resp.Errors.Error())
		}
		span.End() // queries and mutations have exactly one response
		return resp
	}
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

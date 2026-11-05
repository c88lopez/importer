package gql

import (
	"context"
	"sync"
	"time"

	"github.com/99designs/gqlgen/graphql"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const instrumentation = "github.com/vandelay-labs/importer/internal/gql"

// maxOperationNames caps how many distinct operation names get their own
// metric series per pod. Clients choose the names, so without a cap a client
// sending random ones would create a new Prometheus series each time. Names
// past the cap are recorded as "_OTHER" (still in traces and logs, by name).
const maxOperationNames = 100

// operationTelemetry gives each GraphQL operation what the HTTP middleware
// can't, since every operation is the same "/graphql" request to it:
//
//   - a span named like "query Items", under the request's "/graphql" span.
//     Resolvers, and their database spans, run inside it.
//   - graphql.server.operation.duration, a histogram by operation type, name
//     and error.type (the first error's code, e.g. BAD_USER_INPUT; absent on
//     success), for the importer-graphql dashboard.
//
// Queries the schema rejects (unknown field, bad enum) never reach it: they
// show up as 422s on the HTTP metric instead.
type operationTelemetry struct {
	tracer   trace.Tracer
	duration metric.Float64Histogram
	names    *nameLimiter
}

var _ interface {
	graphql.HandlerExtension
	graphql.OperationInterceptor
} = operationTelemetry{}

func newOperationTelemetry() operationTelemetry {
	// The meter provider is set up before the router (cmd/graphql), and
	// creating an instrument only fails on an invalid name.
	duration, _ := otel.Meter(instrumentation).Float64Histogram("graphql.server.operation.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Time to execute a GraphQL operation, from parsed request to response."))
	return operationTelemetry{
		tracer:   otel.Tracer(instrumentation),
		duration: duration,
		names:    &nameLimiter{max: maxOperationNames, seen: map[string]bool{}},
	}
}

func (operationTelemetry) ExtensionName() string                   { return "OperationTelemetry" }
func (operationTelemetry) Validate(graphql.ExecutableSchema) error { return nil }

func (t operationTelemetry) InterceptOperation(ctx context.Context, next graphql.OperationHandler) graphql.ResponseHandler {
	start := time.Now()
	oc := graphql.GetOperationContext(ctx)
	// The name from the document (query Items { ... }), not oc.OperationName:
	// that's the request's optional "operationName" field, which plain curl
	// and many clients leave out.
	opType, opName := "query", ""
	if oc.Operation != nil {
		opType, opName = string(oc.Operation.Operation), oc.Operation.Name
	}
	spanName := opType
	if opName != "" {
		spanName += " " + opName
	}
	ctx, span := t.tracer.Start(ctx, spanName, trace.WithAttributes(
		attribute.String("graphql.operation.type", opType),
		attribute.String("graphql.operation.name", opName),
		attribute.String("graphql.document", oc.RawQuery),
	))
	responses := next(ctx)
	return func(ctx context.Context) *graphql.Response {
		resp := responses(ctx)
		attrs := []attribute.KeyValue{
			attribute.String("graphql.operation.type", opType),
			attribute.String("graphql.operation.name", t.names.label(opName)),
		}
		if resp != nil && len(resp.Errors) > 0 {
			span.SetStatus(codes.Error, resp.Errors.Error())
			attrs = append(attrs, attribute.String("error.type", errorCode(resp)))
		}
		t.duration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attrs...))
		span.End() // queries and mutations have exactly one response
		return resp
	}
}

// errorCode is the first error's extensions.code (BAD_USER_INPUT,
// INTERNAL_SERVER_ERROR, ...), or "_OTHER" for an error without one, like a
// bad argument.
func errorCode(resp *graphql.Response) string {
	if code, ok := resp.Errors[0].Extensions["code"].(string); ok && code != "" {
		return code
	}
	return "_OTHER"
}

// nameLimiter hands out metric labels for operation names: the name itself
// for the first max distinct names a pod sees, "_OTHER" after that, and
// "(anonymous)" for an unnamed operation. Parentheses can't appear in a
// GraphQL name, so that one can't clash with a real name.
type nameLimiter struct {
	mu   sync.Mutex
	max  int
	seen map[string]bool
}

func (l *nameLimiter) label(name string) string {
	if name == "" {
		return "(anonymous)"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen[name] || len(l.seen) < l.max {
		l.seen[name] = true
		return name
	}
	return "_OTHER"
}

package gql

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/99designs/gqlgen/client"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestOperationDurationMetric(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	c := client.New(NewHandler(&memStore{}))
	c.MustPost(`query Items { items { id } }`, &struct{ Items []item }{})
	c.MustPost(`query Items { items { id } }`, &struct{ Items []item }{})
	c.MustPost(`{ items { id } }`, &struct{ Items []item }{})
	_ = c.Post(`mutation Blank { createItem(input: {name: " ", kind: IMPORT}) { id } }`, &struct{}{})

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "graphql.server.operation.duration" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Histogram[float64]).DataPoints {
				var kv []string
				for _, a := range dp.Attributes.ToSlice() {
					kv = append(kv, fmt.Sprintf("%s=%s", a.Key, a.Value.AsString()))
				}
				got = append(got, fmt.Sprintf("%s count=%d", strings.Join(kv, " "), dp.Count))
			}
		}
	}
	sort.Strings(got)
	want := []string{
		"error.type=BAD_USER_INPUT graphql.operation.name=Blank graphql.operation.type=mutation count=1",
		"graphql.operation.name=(anonymous) graphql.operation.type=query count=1",
		"graphql.operation.name=Items graphql.operation.type=query count=2",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got series:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestNameLimiter(t *testing.T) {
	l := &nameLimiter{max: 2, seen: map[string]bool{}}
	for _, tt := range []struct{ name, want string }{
		{"A", "A"},
		{"", "(anonymous)"}, // doesn't use up a slot
		{"B", "B"},
		{"C", "_OTHER"}, // past the cap
		{"A", "A"},      // names seen before keep their label
	} {
		if got := l.label(tt.name); got != tt.want {
			t.Errorf("label(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

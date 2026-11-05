// Package gql is importer-graphql's GraphQL side: the schema, its resolvers
// and the HTTP handler. cmd/graphql wires it to the database and the servers.
package gql

import (
	"context"

	"github.com/vandelay-labs/importer/internal/items"
)

// itemStore is what the resolvers need: items.Store in production, a fake in
// tests. Get and Delete return items.ErrNotFound for an unknown id.
type itemStore interface {
	List(ctx context.Context) ([]items.Item, error)
	Get(ctx context.Context, id int64) (items.Item, error)
	Create(ctx context.Context, name, kind string) (items.Item, error)
	Delete(ctx context.Context, id int64) error
}

// Resolver is the root resolver; gqlgen calls its methods for each field.
type Resolver struct {
	store itemStore
}

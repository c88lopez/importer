// Package scalar has the GraphQL scalars gqlgen doesn't ship. It's separate
// from package gql because gqlgen loads it before generating that package.
package scalar

import (
	"fmt"
	"io"
	"strconv"

	"github.com/99designs/gqlgen/graphql"
)

// MarshalInt64ID writes an int64 as a GraphQL ID: a JSON string, like "42".
func MarshalInt64ID(id int64) graphql.Marshaler {
	return graphql.WriterFunc(func(w io.Writer) {
		_, _ = io.WriteString(w, strconv.Quote(strconv.FormatInt(id, 10)))
	})
}

// UnmarshalInt64ID accepts an ID given as a string ("42") or a number (42),
// as the GraphQL spec allows.
func UnmarshalInt64ID(v any) (int64, error) {
	switch v := v.(type) {
	case string:
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%q is not a valid item id", v)
		}
		return id, nil
	case int64:
		return v, nil
	case int:
		return int64(v), nil
	default:
		return graphql.UnmarshalInt64(v)
	}
}

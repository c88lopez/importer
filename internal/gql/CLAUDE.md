# GraphQL (importer-graphql)

Schema first: edit `schema.graphqls`, run `make generate` from the repo root, fill in stubs in `schema.resolvers.go`.

- `generated.go` and `models_gen.go` are generated and committed. CI fails if they're stale, so a schema change isn't done until `make generate` has run and the generated files are part of the change.
- Form-encoded POSTs are refused (415) on purpose as CSRF protection.
- Queries over complexity 200 are rejected.
- Middleware, health checks and the rate limit come from `internal/api`, shared with importer-api.

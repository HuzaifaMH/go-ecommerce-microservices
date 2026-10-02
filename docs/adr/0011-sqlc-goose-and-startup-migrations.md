# ADR-0011: sqlc for queries, goose for migrations, migrate on startup

**Status:** Accepted

## Context
Each service owns a PostgreSQL schema (ADR-0006). We want SQL to stay visible and reviewable, queries checked at build time, and a simple way to get a schema onto a fresh database in every environment.

## Decision
- **Queries:** write plain SQL in `internal/adapters/postgres/queries/*.sql`; `sqlc` generates typed Go (`sqlcgen/`, committed) targeting pgx. CI regenerates and fails if the result differs.
- **Schema:** SQL migrations in `services/<name>/migrations`, in goose format, embedded in the binary with `go:embed`. sqlc reads the same files as its schema, so queries are checked against the real schema.
- **Applying migrations:** the service runs pending migrations at startup, before it serves traffic.
- **Transactions:** repositories obtain their executor from `pgstore.Store.Querier(ctx)`, so every query joins the transaction opened by `WithTx` or the inbox. Use cases stay free of SQL and of `pgx`.

## Alternatives considered
- **ORM (GORM, ent):** less SQL to write, but harder to reason about locking (`FOR UPDATE`, `SKIP LOCKED`) and generated queries are harder to review.
- **Hand-written pgx scanning:** full control, much boilerplate and no compile-time check of columns.
- **Migrations as a separate job or init container:** better separation and permissions in production, more moving parts locally.

## Consequences
- Queries and schema cannot drift silently; column or type mistakes fail at generation or compile time.
- Startup migrations are convenient and fine at this scale, but with several replicas starting at once they rely on goose's locking, and the service's database role needs DDL rights. A production deployment would move migrations to a dedicated step; the embedded files make that a small change.
- Quantity types in the database are 32-bit; the domain rejects larger values explicitly instead of letting them wrap.

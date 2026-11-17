# Ent query model

This package is generated from `lib/ent/schema` with Ent **v0.14.5**. From
the repository root, regenerate it reproducibly with:

```sh
go run entgo.io/ent/cmd/ent@v0.14.5 generate --target ./lib/ent ./lib/ent/schema
```

The explicit version pins generation tooling; it is not needed to build or run
the application. `lib/bootstrap.go` attaches the generated client to the
application's already-configured `*sql.DB` (WAL, synchronous FULL, foreign
keys, busy timeout, immediate transactions, and one connection). It does not
open another pool. The generated client is never asked to create, reconcile,
or migrate schema. Embedded goose SQL remains the only schema executor.

`schema/` maps the tables and stored fields from the immutable goose SQL migrations,
including the singleton OPAQUE profile and administrator and the existing
project/deployment/job relationships. Composite-key tables retain their full
unique indexes in the Ent model; because Ent's entity API requires an ID, the
first physical key component is used as the generated ID. For such tables,
bulk operations are safe only when their predicate identifies the complete
physical key, and inserts must explicitly supply the complete key tuple. Do
not use generated ID-only `Get`, `UpdateOne`, or `DeleteOne` operations: the
generated ID is not the full identity and can select or modify the wrong row.
Use the existing raw SQL transaction path when the complete-key operation
cannot be expressed safely through Ent. This restriction does not apply to
`ProjectSSHKey`: its `project_id` is its sole physical primary key.

The account workflows use Ent for OPAQUE profile binding, registration grants,
the singleton administrator, login record lookup, and atomic credential
replacement. Transactions are explicitly owned by the account workflow and
share the configured SQLite connection with retained raw-query project and
deployment workflows.

// Package migrations holds the SQL schema migrations for the whole shared
// database, and nothing else.
//
// # Why the .sql files are here rather than in package migrate
//
// go:embed cannot reach outside the directory of the package that declares it,
// so embedding `migrations/*.sql` from `migrate` is impossible. The
// alternative was to move the files under `migrate/migrations/`, which
// would work and would bury them three directories deep. They stay at the top
// level of the module on purpose: a migration is the artefact an operator reads
// before an upgrade and the thing a reviewer diffs, and `logservice-go/migrations/`
// is where somebody coming from `logservice/migrations/` will look for it.
//
// # These files are copies, and their names are a contract
//
// They are byte-identical to `logservice/migrations/*.sql`, which the frozen Bun
// service applied. The `_migrations` table records an applied migration by its
// FILENAME — no checksum, no ordinal — so a database migrated by the Bun runner
// sees all 26 of these as already applied and this service changes nothing on
// its first boot. Renaming one would re-run it, and six of them contain
// `ALTER TABLE ... ADD COLUMN` and `CREATE INDEX` statements that are not
// idempotent. Never rename a shipped file; add 027 and upwards here only.
package migrations

import "embed"

// FS holds every migration. `all:` is not needed — there are no dot-files or
// underscore-prefixed names here — but the pattern is restricted to *.sql so
// this doc comment and any future README in the directory are not embedded into
// the binary and then offered to the runner as a migration.
//
//go:embed *.sql
var FS embed.FS

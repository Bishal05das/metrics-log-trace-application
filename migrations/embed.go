// Package migrations holds the SQL schema files, embedded into the binary so
// that the service can migrate itself on startup. A real deployment usually
// runs migrations as a separate step (a Kubernetes Job, a CI stage) rather
// than on every pod start; embedding them keeps this project to one command.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS

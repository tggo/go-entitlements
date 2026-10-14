//go:build integration

package postgres

import (
	"context"
	"testing/fstest"
)

// MigrateFS lets the external test package run migrations from a fake FS.
func (s *Store) MigrateFS(ctx context.Context, fsys fstest.MapFS) error {
	return s.migrate(ctx, fsys)
}

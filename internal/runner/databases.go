package runner

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/andersonten/simcitadel/internal/config"
	"github.com/andersonten/simcitadel/internal/db"
)

// databaseTargets returns the configured database targets keyed by the name a
// scenario step uses in its target field.
func databaseTargets(cfg *config.CitadelConfig) map[string]*config.DatabaseTarget {
	targets := make(map[string]*config.DatabaseTarget, 2)
	if cfg.Targets.AppDB != nil && cfg.Targets.AppDB.DSN != "" {
		targets["app-db"] = cfg.Targets.AppDB
	}
	if cfg.Targets.UpstreamShadow != nil && cfg.Targets.UpstreamShadow.DSN != "" {
		targets["upstream-shadow"] = cfg.Targets.UpstreamShadow
	}
	return targets
}

// OpenDatabases opens a connection for every configured database target.
// OpenDatabases returns the open connections and a function that closes them.
// A target that does not answer is an environment failure, because a scenario
// that references it cannot run.
func OpenDatabases(ctx context.Context, cfg *config.CitadelConfig) (map[string]*sql.DB, func(), error) {
	return openDatabaseTargets(ctx, cfg, nil)
}

// openDatabaseTargets opens the configured database targets that the wanted set
// names. A nil set opens every configured target. SDD section 4.10 checks only
// the targets a scenario references, so a run must not fail on a database that
// no scenario touches.
func openDatabaseTargets(ctx context.Context, cfg *config.CitadelConfig, wanted map[string]bool) (map[string]*sql.DB, func(), error) {
	handles := make(map[string]*sql.DB)

	closeAll := func() {
		for name, handle := range handles {
			if err := handle.Close(); err != nil {
				slog.Warn("close database connection failed", "target", name, "error", err.Error())
			}
		}
	}

	for name, target := range databaseTargets(cfg) {
		if wanted != nil && !wanted[name] {
			continue
		}
		handle, err := db.Open(ctx, target.Driver, target.DSN)
		if err != nil {
			closeAll()
			return nil, func() {}, &EnvironmentError{
				Msg: fmt.Sprintf("connect to database target %q: %v", name, err),
			}
		}
		handles[name] = handle
		slog.Info("database connection open", "target", name)
	}

	return handles, closeAll, nil
}

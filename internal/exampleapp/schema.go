// Package exampleapp holds a small application that stands in for the real
// service under test. SDD section 3.1 names the application under test as one
// process of a complete environment, and this repository holds no such
// application, so the compose stack could not run the outbound half of the
// pipeline and section 14.4 could not run at all.
//
// This app is deliberately the smallest thing that makes the whole pipeline
// real. It consumes an inbound event, it reads an upstream through WireMock, it
// keeps an inbox row for idempotency, and it writes an outbox row that Debezium
// turns into an outbound event. Every one of those is a genuine step of the
// pipeline rather than a simulation of one.
//
// It is not a production application and it does not pretend to be. A consuming
// repository replaces it with its own image, and the compose file keeps the
// image name in one variable so that replacement is one edit.
package exampleapp

import (
	"context"
	"fmt"
	"time"
)

// The outbox columns follow the table of SDD section 10.1 exactly, because the
// Debezium EventRouter expects those names and the compose file remaps only the
// timestamp. aggregatetype holds the full exchange name, which is what makes the
// routing of section 10.1 resolve to one exchange per entity.
//
// payload is jsonb and not varchar. Section 10.1 calls that out against the
// outbox reference schema, and expand.json.payload of the transform
// needs real JSON to expand.
//
// The inbox columns follow the defaults of internal/config, so inbox/assert
// works against this app with no column names in the scenario.
//
// processed_at is nullable, and that carries the meaning of the table. A row says
// the app saw the event, and a non-null processed_at says the app finished it. A
// failure therefore records retry_count and last_error while it leaves
// processed_at null, so a redelivery is processed rather than dropped as a
// duplicate. A NOT NULL default of now() would make every failed event look
// finished and would make all four columns unusable for an assertion.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS public.outbox (
	id            uuid         PRIMARY KEY,
	aggregatetype varchar(255) NOT NULL,
	aggregateid   varchar(255) NOT NULL,
	type          varchar(255) NOT NULL,
	payload       jsonb        NOT NULL,
	created_at    timestamptz  NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS public.inbox (
	message_id   varchar(255) PRIMARY KEY,
	processed_at timestamptz,
	retry_count  integer      NOT NULL DEFAULT 0,
	last_error   text
);

CREATE TABLE IF NOT EXISTS public.manuscript (
	id         uuid         PRIMARY KEY,
	article_id varchar(255) NOT NULL UNIQUE,
	title      text         NOT NULL,
	status     varchar(64)  NOT NULL,
	created_at timestamptz  NOT NULL DEFAULT now()
);
`

// Migrate creates the tables the app needs. It runs on every start and it is
// idempotent, because the compose stack recreates the container without
// recreating the volume.
//
// The order matters outside this function: Debezium resolves table.include.list
// against the live catalog when it starts, and it exits rather than wait, so the
// outbox table must exist before Debezium starts. The compose file holds that
// order with a service_healthy condition on this app.
func (a *App) Migrate(ctx context.Context) error {
	if _, err := a.db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("create the application schema: %w", err)
	}
	a.setDBReady(true)
	a.logger.Info("schema ready", "tables", "outbox, inbox, manuscript")
	return nil
}

// migrateTimeout bounds the wait for the database. The compose file already holds
// a service_healthy condition on the database, so this covers a database that
// accepts a connection before it finishes recovery.
const migrateTimeout = 60 * time.Second

// MigrateWithRetry runs Migrate until it succeeds or the timeout passes.
//
// It polls a condition rather than sleeping for a fixed period, because a fixed
// sleep is either too short on a slow machine or wasted on a fast one, and
// CONTRIBUTING.md forbids one as a synchronisation mechanism.
func (a *App) MigrateWithRetry(ctx context.Context) error {
	deadline := time.Now().Add(migrateTimeout)
	const interval = 500 * time.Millisecond

	var lastErr error
	for attempt := 1; ; attempt++ {
		if err := a.Migrate(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}

		if ctx.Err() != nil {
			return fmt.Errorf("wait for the database: %w", ctx.Err())
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the database did not accept the schema within %s: %w", migrateTimeout, lastErr)
		}
		if attempt == 1 || attempt%10 == 0 {
			a.logger.Info("waiting for the database", "attempt", attempt, "error", lastErr)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for the database: %w", ctx.Err())
		case <-time.After(interval):
		}
	}
}

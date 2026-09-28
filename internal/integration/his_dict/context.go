package his_dict

import "context"

// sqlCtx returns a fresh background context. Used by SQLite/MySQL/PG
// constructors that need to run CREATE TABLE statements at startup but
// don't take a context.Context parameter (callers pass ctx at every
// Apply/Read call, but construction-time schema setup happens before
// any request reaches the engine).
//
// Returns a context.Context — not interface{} — so callers can swap it
// for a WithTimeout if they want startup to fail fast on a slow DB.
func sqlCtx() context.Context { return context.Background() }
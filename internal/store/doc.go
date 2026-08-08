// Package store is the persistence layer: SQLite through modernc.org/sqlite,
// which is pure Go and needs no CGO, because cross-compiling to linux/amd64,
// linux/arm64 and windows/amd64 must keep working.
//
// It holds the repositories for users, tokens, servers, backups and the audit
// log, plus the migrations applied at daemon start.
//
// Populated in task 1.1. Until then the API runs against api.MemoryAuth, an
// in-memory placeholder that implements the same interfaces.
package store

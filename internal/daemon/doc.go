// Package daemon is the core of Mirocraft: it owns the state of every server
// and is the only place where server management decisions are made.
//
// It sits between the API and the runner, and is responsible for:
//   - CRUD over servers and their configuration
//   - lifecycle orchestration (start, stop, restart, kill) on top of a Runner
//   - downloading server software through the CoreProvider registry
//   - the file manager scoped to a server's directory
//   - resource limits, backups and scheduled tasks
//
// The API, the web panel and the bots never touch files or processes
// themselves; they go through this package, per the project's hard rule that
// management logic lives in exactly one place.
//
// Populated from task 2.2 onwards. Console handling already lives in
// internal/runner, which the API currently drives directly.
package daemon

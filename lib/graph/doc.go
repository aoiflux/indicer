// Package graph is the single owner of the GrapheneDB store.
//
// Every other package in DUES reaches the graph through this one. Nothing else
// may import github.com/aoiflux/graphene/... — see the CI guard in
// import_guard_test.go for why that rule is enforced mechanically rather than
// by convention.
//
// # Why a single owner
//
// GrapheneDB has no locking of any kind. There is no flock, no LockFileEx, no
// O_EXCL and no lock file anywhere in its disk backend, so nothing stops two
// *graphene.Graph handles being opened on one directory. Two handles means two
// independent node/edge ID sequences, two O_APPEND writers into one
// graphene.wal, two in-memory delta overlays, and — if either compacts — one
// rebuilt image renamed over the other's. That is duplicate IDs and interleaved
// batch frames, which on evidence is corruption.
//
// DUES did exactly that before this package existed: cli/cmdstore.go held an
// enrichment repository across a partition loop while lib/parser/common.go
// opened a second one inside it, and cli/cmdmicro.go held the micro-artefact
// repository while running an enrichment backfill through a second handle.
//
// Handle solves it by reference-counting one handle per directory for the
// lifetime of the process, so nested opens are the same store, plus a
// cross-process advisory lock so a second dues invocation is refused rather
// than allowed to interleave.
package graph

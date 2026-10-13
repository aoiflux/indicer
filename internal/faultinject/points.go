// Package faultinject lets tests make DUES fail at named points: return an
// error, report a full disk, panic, exit, stall or signal itself.
//
// Code marks a point with
//
//	if err := faultinject.Hit(faultinject.IngestBeforeCommit); err != nil {
//		return err
//	}
//
// In a normal build Hit is an empty function that always returns nil, and the
// compiler removes the call. Only a build with -tags faultinject reads the
// DUES_FAULTS environment variable and acts on it. Release builds must never
// carry the tag; the verify gate checks binaries with `go version -m`.
//
// DUES_FAULTS is a comma-separated list of point=action[@N[+]]:
//
//	error        return ErrInjected
//	enospc       return the platform's disk-full error
//	eio          return the platform's I/O error
//	eacces       return the platform's permission error
//	panic        panic with a faultinject message
//	exit:CODE    exit the process with CODE, without deferred cleanup
//	sleep:DUR    sleep for a time.ParseDuration value, then continue
//	block        report the point, then block until the process is killed
//	signal:INT   send the process an interrupt (Ctrl-Break on Windows)
//	signal:TERM  send the process SIGTERM (Unix only)
//
// @N fires only on the Nth hit of the point, @N+ on the Nth and every later
// hit; with neither, every hit fires. When DUES_FAULTS_NOTIFY names a file,
// the first hit of each point appends "<point>\n" to it, so a parent test can
// wait for the child to get there.
package faultinject

import "errors"

// ErrInjected is the error the "error" action returns.
var ErrInjected = errors.New("faultinject: injected fault")

// Known fault points. A spec naming any other point is rejected, so a typo in
// a test cannot silently inject nothing.
const (
	IngestAfterPending        = "ingest.after_pending"
	StoreChunkBeforeBarrier   = "store.chunk.before_barrier"
	StoreChunkAfterBarrier    = "store.chunk.after_barrier"
	IngestBeforeFlushed       = "ingest.before_flushed"
	IngestBeforeCommit        = "ingest.before_commit"
	IngestInCommit            = "ingest.in_commit"
	IngestDuplicateCleanup    = "ingest.duplicate_cleanup"
	IndexMid                  = "index.mid"
	ContainerMidRecord        = "container.mid_record"
	ManifestTempWritten       = "manifest.temp_written"
	RestoreMid                = "restore.mid"
	ResetMid                  = "reset.mid"
	UploadBeforeIngest        = "upload.before_ingest"
	UploadAfterIngest         = "upload.after_ingest"
	QwikpakArchiveBeforeClose = "qwikpak.archive.before_close"
	// SelfTest is used only by this package's tests.
	SelfTest = "faultinject.selftest"
)

var known = map[string]bool{
	IngestAfterPending:        true,
	StoreChunkBeforeBarrier:   true,
	StoreChunkAfterBarrier:    true,
	IngestBeforeFlushed:       true,
	IngestBeforeCommit:        true,
	IngestInCommit:            true,
	IngestDuplicateCleanup:    true,
	IndexMid:                  true,
	ContainerMidRecord:        true,
	ManifestTempWritten:       true,
	RestoreMid:                true,
	ResetMid:                  true,
	UploadBeforeIngest:        true,
	UploadAfterIngest:         true,
	QwikpakArchiveBeforeClose: true,
	SelfTest:                  true,
}

// Known reports whether name is a registered fault point.
func Known(name string) bool { return known[name] }

// Points returns every registered fault point.
func Points() []string {
	out := make([]string, 0, len(known))
	for p := range known {
		out = append(out, p)
	}
	return out
}

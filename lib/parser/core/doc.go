// Package core defines the interfaces and shared types for the pure-Go forensic
// parsing stack that replaces the libtsk/libtusk C++ (CGo) dependency.
//
// The stack has three layered concerns, each a separate sub-package that consumes
// the layer below as an io.ReaderAt and produces a value implementing an interface
// defined here:
//
//	Image      (lib/parser/image)     — a decoded, contiguous logical device
//	                                     (raw/dd, EWF/E01, VHD/VHDX, …)
//	Partition  (lib/parser/partition) — a partition table (MBR/GPT/APM/BSD/Sun)
//	Filesystem (lib/parser/fs, fs/*)  — a mounted filesystem (FAT/exFAT/NTFS/ext/
//	                                     HFS+/XFS, …) over a partition sub-range
//
// The behavioural contract reproduced from libtusk: every file resolves to a set
// of absolute byte ranges within the Image (AbsRange) plus deleted / fragmented
// flags and — new — MACB timestamps. Downstream ingest reads those raw ranges
// directly; it does not use a filesystem API at read time.
//
// This package holds only interfaces and data types (no CGo, no backing-library
// imports), so it compiles on every platform and every adapter depends on it
// without cycles.
//
// See docs/LIBTSK_REMOVAL_PLAN.md for the full design and migration plan.
package core

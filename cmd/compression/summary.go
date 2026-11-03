package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"indicer/internal/termsafe"
	"indicer/internal/ui"

	"golang.org/x/term"
)

// The values of --summary.
const (
	summaryAuto = "auto"
	summaryOn   = "on"
	summaryOff  = "off"
)

// isTerminal reports whether w is a terminal.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// summaryEnvelope is what a summary reads of a response envelope.
type summaryEnvelope struct {
	Status     string `json:"status"`
	DurationMS int64  `json:"duration_ms"`
	Result     struct {
		InputPath       string   `json:"input_path"`
		OutputPath      string   `json:"output_path"`
		OutputFiles     []string `json:"output_files"`
		InputBytes      int64    `json:"input_bytes"`
		ArchiveBytes    int64    `json:"archive_bytes"`
		ArchivedFiles   int64    `json:"archived_files"`
		ArchivedDirs    int64    `json:"archived_dirs"`
		SkippedInputs   int64    `json:"skipped_inputs"`
		OriginalBytes   int64    `json:"original_bytes"`
		CompressedBytes int64    `json:"compressed_bytes"`
		ExtractedFiles  int64    `json:"extracted_files"`
		ExtractedDirs   int64    `json:"extracted_dirs"`
		SkippedEntries  int64    `json:"skipped_entries"`
		TaskID          string   `json:"task_id"`
	} `json:"result"`
}

// writeSummary says on w what a file or archive operation did, from its
// response envelope out: what it read and wrote, how much smaller that is,
// and how long it took. A failure, a task started in the background and
// the other operations have no summary: the envelope says it all.
func writeSummary(w io.Writer, operation, out string) {
	var env summaryEnvelope
	if json.Unmarshal([]byte(out), &env) != nil || env.Status != "ok" || env.Result.TaskID != "" {
		return
	}
	r := env.Result
	took := ui.Duration(time.Duration(env.DurationMS) * time.Millisecond)
	switch operation {
	case opCreateArchive:
		fmt.Fprintf(w, "archived %s%s in %s\n", filesAndFolders(r.ArchivedFiles, r.ArchivedDirs), skipped(r.SkippedInputs), took)
		fmt.Fprintf(w, "  input %s, archive %s: %s\n", ui.Bytes(r.InputBytes), ui.Bytes(r.ArchiveBytes), smaller(r.InputBytes, r.ArchiveBytes))
		if len(r.OutputFiles) == 1 && r.OutputFiles[0] == r.OutputPath {
			fmt.Fprintf(w, "  written to %s\n", termsafe.Line(r.OutputPath))
		} else {
			// A split archive: its parts, and the sidecar naming them.
			fmt.Fprintf(w, "  written as %s, described by %s.split.json\n", counted(int64(len(r.OutputFiles)), "part"), termsafe.Line(r.OutputPath))
		}
	case opCompressFile:
		fmt.Fprintf(w, "compressed %s in %s\n", termsafe.Line(r.InputPath), took)
		fmt.Fprintf(w, "  input %s, archive %s: %s\n", ui.Bytes(r.OriginalBytes), ui.Bytes(r.CompressedBytes), smaller(r.OriginalBytes, r.CompressedBytes))
		fmt.Fprintf(w, "  written to %s\n", termsafe.Line(r.OutputPath))
	case opDecompressFile:
		fmt.Fprintf(w, "restored %s in %s\n", termsafe.Line(r.OutputPath), took)
		fmt.Fprintf(w, "  archive %s, file %s\n", ui.Bytes(r.CompressedBytes), ui.Bytes(r.OriginalBytes))
	case opExtractArchive:
		fmt.Fprintf(w, "extracted %s%s in %s\n", filesAndFolders(r.ExtractedFiles, r.ExtractedDirs), skipped(r.SkippedEntries), took)
		fmt.Fprintf(w, "  to %s\n", termsafe.Line(r.OutputPath))
	}
}

// counted is n of a noun: "1 file", "1,204 files".
func counted(n int64, noun string) string {
	if n != 1 {
		noun += "s"
	}
	return ui.Count(n) + " " + noun
}

// filesAndFolders is "1,204 files and 87 folders", or the files alone.
func filesAndFolders(files, dirs int64) string {
	if dirs == 0 {
		return counted(files, "file")
	}
	return counted(files, "file") + " and " + counted(dirs, "folder")
}

// skipped is " (3 skipped)", or "" when nothing was.
func skipped(n int64) string {
	if n == 0 {
		return ""
	}
	return " (" + ui.Count(n) + " skipped)"
}

// smaller compares the original size with the archive's: "3.29x smaller,
// 69.6% saved", or "grew 2.1%".
func smaller(original, archived int64) string {
	if archived > original {
		return ui.Saved(original, archived)
	}
	return ui.Ratio(original, archived) + " smaller, " + ui.Saved(original, archived)
}

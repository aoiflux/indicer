package cli

import (
	"bufio"
	"errors"
	"indicer/internal/console"
	"indicer/internal/lifecycle"
	"indicer/internal/ui"
	"indicer/lib/dbopen"
	"indicer/lib/logging"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"
)

// ResetData deletes the DUES database at dbpath, and only that: the data
// root must hold a database and must not be the home directory, the working
// directory or a filesystem root, and nothing in it that DUES does not own
// is touched (ING-16). Unless yes is set, the user confirms by typing the
// root's folder name; with stdin not a terminal, yes is required, so a
// script cannot delete a case by piping "y".
func ResetData(dbpath string, yes bool) error {
	return resetData(dbpath, yes, os.Stdin, console.Interactive())
}

func resetData(dbpath string, yes bool, in io.Reader, interactive bool) error {
	start := time.Now()
	root, err := dbopen.ResolveRoot(dbpath)
	if err != nil {
		return err
	}
	logging.GetLogger().Info("ResetData START", zap.String("root", root))

	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(entries) == 0) {
		p := ui.For(console.Out())
		console.Printf("Nothing to reset: no data at %s\n", p.Paint(ui.Evidence, root))
		return nil
	}
	if err != nil {
		return err
	}
	if err := dbopen.CheckResettable(root); err != nil {
		return &lifecycle.UsageError{Err: err}
	}

	if !yes {
		if !interactive {
			return lifecycle.Usagef("reset of %s needs --yes when stdin is not a terminal", root)
		}
		name := filepath.Base(root)
		p := ui.For(console.Out())
		console.Printf("%s This deletes the DUES database at %s: all stored evidence, indexes, graph and full-text data.\n",
			p.Strong(ui.Warning, "WARNING!"), p.Paint(ui.Evidence, root))
		console.Printf("Type the folder name (%s) to confirm: ", p.Strong(ui.Highlight, name))
		line, _ := bufio.NewReader(in).ReadString('\n')
		if strings.TrimSpace(line) != name {
			console.Println(p.Strong(ui.Success, "Your data is SAFE!"))
			logging.GetLogger().Info("ResetData CANCELLED", zap.Int64("duration_ms", time.Since(start).Milliseconds()))
			return nil
		}
	}

	_, removed, err := dbopen.Reset(root)
	if err != nil {
		logging.GetLogger().Error("ResetData DELETE_ERROR", zap.Error(err), zap.String("root", root))
		return err
	}
	p := ui.For(console.Out())
	console.Printf("%s %s from %s\n", p.Strong(ui.Success, "Deleted"), strings.Join(removed, ", "), p.Paint(ui.Evidence, root))
	logging.GetLogger().Info("ResetData COMPLETE",
		zap.Int64("duration_ms", time.Since(start).Milliseconds()),
		zap.String("root", root),
		zap.Strings("removed", removed),
	)
	return nil
}

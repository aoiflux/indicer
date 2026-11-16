// Package api runs `dues server`: it opens the database and serves it with
// lib/server.
package api

import (
	"context"
	"errors"
	"fmt"

	"indicer/internal/console"
	"indicer/internal/lifecycle"
	"indicer/internal/ui"
	"indicer/lib/dbopen"
	"indicer/lib/logging"
	"indicer/lib/server"
)

// Options are the server's command-line settings.
type Options struct {
	Listen      []string
	Tokens      []server.Credential
	CORSOrigins []string
	// UploadJobs is how many uploads are stored at once; 0 keeps the
	// server's default.
	UploadJobs int
}

// Server serves the database db names until ctx ends. It opens it writable,
// and first finishes or fails the ingests a crash left incomplete. A
// configuration problem is a usage error found before the database is
// opened. Once it serves, the server owns the database: it closes it after a
// clean stop, and a stop that leaves work running
// (server.ErrUncleanShutdown) leaves it as it is, for recovery at its next
// open.
func Server(ctx context.Context, db dbopen.Request, so Options) error {
	cfg := server.Config{Listen: so.Listen, Tokens: so.Tokens, CORSOrigins: so.CORSOrigins, Limits: server.DefaultLimits()}
	if so.UploadJobs > 0 {
		cfg.Limits.Upload.Jobs = so.UploadJobs
	}
	if err := cfg.Validate(); err != nil {
		return lifecycle.Usagef("server: %v", err)
	}
	db.Mode, db.Recover = dbopen.ReadWrite, true
	h, err := dbopen.Open(ctx, db)
	if err != nil {
		printErr("Server startup failed", []string{fmt.Sprintf("Database connection failed: %v", err)})
		return err
	}
	for _, n := range h.Notices {
		ui.Notef("%s", n)
	}
	srv, err := server.New(cfg, server.Deps{Handle: h, Log: logging.GetLogger()})
	if err != nil {
		return errors.Join(err, h.Close())
	}
	if err := srv.Listen(); err != nil {
		printErr("Server startup failed", []string{err.Error()})
		return errors.Join(err, h.Close())
	}
	lines := []string{fmt.Sprintf("Database: %s", h.Root)}
	for _, a := range srv.Addrs() {
		lines = append(lines, fmt.Sprintf("Listening on %s", a))
	}
	lines = append(lines, srv.Notes...)
	lines = append(lines, tokenLines(so.Tokens)...)
	lines = append(lines, "Service path: /dues.DuesService/*", "Health: GET /healthz, GET /readyz", "Press Ctrl+C to stop gracefully")
	printSuccess("Server online", lines)
	for _, a := range cfg.Listen {
		if !server.IsLoopbackAddr(a) {
			printWarn("Not encrypted", []string{"Traffic on non-loopback addresses is plain HTTP; put a TLS proxy or an SSH tunnel in front of it"})
			break
		}
	}

	err = srv.Serve(ctx)
	switch {
	case errors.Is(err, server.ErrUncleanShutdown):
		printErr("Server stopped with work still running", []string{"The database was left open; it is recovered at its next open"})
		return err
	case err != nil:
		printErr("Server stopped", []string{err.Error()})
		return err
	}
	printSuccess("Server stopped", []string{"All listeners closed", "Database closed gracefully"})
	// A server stopped by a signal exits with the signal's code.
	var sig *lifecycle.SignalError
	if errors.As(context.Cause(ctx), &sig) {
		return sig
	}
	return nil
}

func printSuccess(title string, lines []string) {
	printSection(ui.Success, func(g ui.Glyphs) string { return g.OK }, title, lines)
}

func printWarn(title string, lines []string) {
	printSection(ui.Warning, func(g ui.Glyphs) string { return g.Warn }, title, lines)
}

func printErr(title string, lines []string) {
	printSection(ui.Error, func(g ui.Glyphs) string { return g.Fail }, title, lines)
}

func printSection(role ui.Role, mark func(ui.Glyphs) string, title string, lines []string) {
	p := ui.For(console.Out())
	g := p.Glyphs()
	console.Printf("  %s  %s\n", p.Strong(role, mark(g)), p.Strong(role, title))
	bullet := p.Paint(ui.Muted, "     "+g.Sep+" ")
	for _, line := range lines {
		console.Println(bullet + line)
	}
	console.Println()
}

// tokenLines say what each token may do, by its label; the token itself is
// never printed.
func tokenLines(tokens []server.Credential) []string {
	if len(tokens) == 0 {
		return []string{fmt.Sprintf("No token: every request may %s; %s is off", server.DefaultScopes, server.ScopeExport)}
	}
	lines := []string{"Bearer token required on every request except /healthz and /readyz"}
	for _, c := range tokens {
		lines = append(lines, fmt.Sprintf("Token %s: %s", c.Label(), c.Scopes()))
	}
	return lines
}

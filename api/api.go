// Package api runs `dues server`: it opens the database and serves it with
// lib/server.
package api

import (
	"context"
	"errors"
	"fmt"

	"indicer/cli"
	"indicer/internal/console"
	"indicer/internal/lifecycle"
	"indicer/lib/dbopen"
	"indicer/lib/logging"
	"indicer/lib/server"

	"github.com/fatih/color"
)

// Options are the server's command-line settings.
type Options struct {
	Listen      []string
	Token       string
	CORSOrigins []string
}

// Server serves the database opts names until ctx ends. A configuration
// problem is a usage error found before the database is opened. A stop that
// leaves work running (server.ErrUncleanShutdown) leaves the database as it
// is, for recovery at its next open.
func Server(ctx context.Context, opts cli.DBOptions, so Options) (err error) {
	cfg := server.Config{Listen: so.Listen, Token: so.Token, CORSOrigins: so.CORSOrigins, Limits: server.DefaultLimits()}
	if err := cfg.Validate(); err != nil {
		return lifecycle.Usagef("server: %v", err)
	}
	h, err := cli.Common(ctx, opts, dbopen.ReadWrite, true)
	if err != nil {
		printErr("Server startup failed", []string{fmt.Sprintf("Database connection failed: %v", err)})
		return err
	}
	unclean := false
	defer func() {
		if unclean {
			return
		}
		if cerr := h.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	srv, err := server.New(cfg, server.Deps{DB: h.DB, Root: h.Root, Log: logging.GetLogger()})
	if err != nil {
		return err
	}
	if err := srv.Listen(); err != nil {
		printErr("Server startup failed", []string{err.Error()})
		return err
	}
	lines := []string{fmt.Sprintf("Database: %s", h.Root)}
	for _, a := range srv.Addrs() {
		lines = append(lines, fmt.Sprintf("Listening on %s", a))
	}
	lines = append(lines, srv.Notes...)
	if so.Token != "" {
		lines = append(lines, "Bearer token required on every request except /healthz and /readyz")
	}
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
		unclean = true
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

const (
	iconSuccess = "✓"
	iconWarn    = "▲"
	iconError   = "✗"
)

func printSuccess(title string, lines []string) {
	printSection(iconSuccess, title, lines, color.New(color.FgGreen, color.Bold))
}

func printWarn(title string, lines []string) {
	printSection(iconWarn, title, lines, color.New(color.FgYellow, color.Bold))
}

func printErr(title string, lines []string) {
	printSection(iconError, title, lines, color.New(color.FgRed, color.Bold))
}

func printSection(icon, title string, lines []string, c *color.Color) {
	c.Printf("  %s  %s\n", icon, title)
	dim := color.New(color.FgHiBlack)
	for _, line := range lines {
		dim.Print("     · ")
		console.Println(line)
	}
	console.Println()
}

package api

import (
	"context"
	"errors"
	"fmt"
	"indicer/cli"
	"indicer/lib/cnst"
	"indicer/lib/server"
	"indicer/lib/util"
	"indicer/pb/pbconnect"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/fatih/color"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

const PORT = "50051"

func Server(chonkSize int, dbpath string, key []byte) error {
	var err error

	cnst.DB, dbpath, err = cli.Common(chonkSize, dbpath, key)
	if err != nil {
		printErr("Server startup failed", []string{fmt.Sprintf("Database connection failed: %v", err)})
		return err
	}
	defer cnst.DB.Close()
	dbpath, err = filepath.Abs(dbpath)
	if err != nil {
		return err
	}

	err = util.EnsureBlobPath(dbpath)
	if err != nil {
		printErr("Server startup failed", []string{fmt.Sprintf("Blob path check failed: %v", err)})
		return err
	}
	uploadsDir := filepath.Join(dbpath, cnst.UploadsDir)

	if err = ensureUploadDir(uploadsDir); err != nil {
		printErr("Server startup failed", []string{fmt.Sprintf("Failed to prepare uploads dir: %v", err)})
		return err
	}
	printSuccess("Database ready", []string{
		fmt.Sprintf("DB path: %s", dbpath),
		fmt.Sprintf("Uploads dir: %s", uploadsDir),
	})

	printInfo("DUES Server", []string{
		"Protocol: Connect + gRPC + gRPC-Web",
		fmt.Sprintf("Port: %s", PORT),
	})

	// Create Connect handler (supports gRPC, gRPC-Web, and Connect protocols)
	mux := http.NewServeMux()
	path, handler := pbconnect.NewDuesServiceHandler(
		server.NewConnectService(),
	)
	mux.Handle(path, handler)

	// Unary web upload endpoints (browser-compatible alternative to client-streaming RPC).
	mux.HandleFunc("/web/upload/start", server.HandleUploadStart)
	mux.HandleFunc("/web/upload/chunk", server.HandleUploadChunk)
	mux.HandleFunc("/web/upload/finalize", server.HandleUploadFinalize)

	// Add a health check endpoint
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Connect server running - supports gRPC, gRPC-Web, and Connect protocols"))
	})

	// Wrap with CORS middleware for web clients
	corsHandler := corsMiddleware(mux)

	// HTTP server with h2c (HTTP/2 without TLS) for Connect
	httpServer := &http.Server{
		Addr:    ":" + PORT,
		Handler: h2c.NewHandler(corsHandler, &http2.Server{}),
	}

	// Start Connect HTTP server in a goroutine
	serveErr := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()
	printSuccess("Server online", []string{
		fmt.Sprintf("Listening on 0.0.0.0:%s", PORT),
		"Health endpoint: GET /",
		"Service path: /dues.DuesService/*",
		"Press Ctrl+C to stop gracefully",
	})

	// Set up signal handling for graceful shutdown
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)

	var serveRuntimeErr error

	select {
	case sig := <-sigs:
		printWarn("Shutdown requested", []string{fmt.Sprintf("Signal received: %v", sig)})
	case serveRuntimeErr = <-serveErr:
		if serveRuntimeErr != nil {
			printErr("Server exited unexpectedly", []string{serveRuntimeErr.Error()})
			return serveRuntimeErr
		}
		printWarn("Server exited", []string{"Server stopped without shutdown signal"})
		return nil
	}

	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Shutdown Connect server
	printInfo("Graceful shutdown", []string{"Stopping HTTP server"})
	if err := httpServer.Shutdown(ctx); err != nil {
		printErr("Server shutdown failed", []string{err.Error()})
		return err
	}

	// Wait for server goroutine to finish
	serveRuntimeErr = <-serveErr
	if serveRuntimeErr != nil {
		printErr("Server exited unexpectedly", []string{serveRuntimeErr.Error()})
		return serveRuntimeErr
	}

	// Cleanup resources
	err = cnst.ENCODER.Close()
	if err != nil {
		printErr("Cleanup failed", []string{fmt.Sprintf("Encoder close failed: %v", err)})
		return err
	}
	cnst.DECODER.Close()
	printSuccess("Server stopped", []string{"All listeners closed", "Database closed gracefully", "Encoder/decoder resources released"})

	return err
}

const (
	iconInfo    = "◆"
	iconSuccess = "✓"
	iconWarn    = "▲"
	iconError   = "✗"
)

func printInfo(title string, lines []string) {
	printSection(iconInfo, title, lines, color.New(color.FgCyan, color.Bold))
}

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
		fmt.Println(line)
	}
	fmt.Println()
}

func isLocalhostOrigin(origin string) bool {
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func corsMiddleware(next http.Handler) http.Handler {
	const allowMethods = "POST, OPTIONS"
	const allowHeaders = "content-type,x-grpc-web,grpc-timeout,x-user-agent,connect-protocol-version,upload-id"
	const exposeHeaders = "grpc-status,grpc-message,grpc-status-details-bin"

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isPreflight := r.Method == http.MethodOptions
		isGRPCWeb := r.Header.Get("X-Grpc-Web") != ""
		origin := r.Header.Get("Origin")

		// Only attach CORS headers for browser-originated traffic, preflight, or grpc-web.
		if isPreflight || isGRPCWeb || isLocalhostOrigin(origin) {
			allowedOrigin := origin
			if allowedOrigin == "" {
				allowedOrigin = "*"
			}
			w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
			w.Header().Set("Access-Control-Allow-Methods", allowMethods)
			w.Header().Set("Access-Control-Allow-Headers", allowHeaders)
			w.Header().Set("Access-Control-Expose-Headers", exposeHeaders)
			w.Header().Add("Vary", "Origin")
			w.Header().Add("Vary", "Access-Control-Request-Method")
			w.Header().Add("Vary", "Access-Control-Request-Headers")
		}

		if isPreflight {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func ensureUploadDir(uploadsDir string) error {
	_, err := os.Stat(uploadsDir)
	if os.IsNotExist(err) {
		return os.MkdirAll(uploadsDir, 0o755)
	}
	return nil
}

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
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/fatih/color"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

const PORT = "50051"

func Server(chonkSize int, dbpath string, key []byte) error {
	var err error

	cnst.DB, dbpath, err = cli.Common(chonkSize, dbpath, key)
	if err != nil {
		printErrorBox("Server startup failed", []string{fmt.Sprintf("Database connection failed: %v", err)})
		return err
	}
	defer cnst.DB.Close()
	dbpath, err = filepath.Abs(dbpath)
	if err != nil {
		return err
	}

	err = util.EnsureBlobPath(dbpath)
	if err != nil {
		printErrorBox("Server startup failed", []string{fmt.Sprintf("Blob path check failed: %v", err)})
		return err
	}
	printSuccessBox("Database ready", []string{fmt.Sprintf("DB path: %s", dbpath)})
	uploadsDir := filepath.Join(dbpath, cnst.UploadsDir)

	printInfoBox("DUES Server", []string{
		"Booting server runtime",
		"Protocol: Connect + gRPC + gRPC-Web",
		fmt.Sprintf("Port: %s", PORT),
	})

	if err = ensureUploadDir(uploadsDir); err != nil {
		printErrorBox("Server startup failed", []string{fmt.Sprintf("Failed to prepare uploads dir: %v", err)})
		return err
	}
	printSuccessBox("Startup check", []string{fmt.Sprintf("Uploads dir ready: %s", uploadsDir)})

	// Create Connect handler (supports gRPC, gRPC-Web, and Connect protocols)
	mux := http.NewServeMux()
	path, handler := pbconnect.NewDuesServiceHandler(
		server.NewConnectService(),
	)
	mux.Handle(path, handler)

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
	printSuccessBox("Server online", []string{
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
		printWarnBox("Shutdown requested", []string{fmt.Sprintf("Signal received: %v", sig)})
	case serveRuntimeErr = <-serveErr:
		if serveRuntimeErr != nil {
			printErrorBox("Server exited because of error", []string{serveRuntimeErr.Error()})
			return serveRuntimeErr
		}
		printWarnBox("Server exited", []string{"Server stopped without shutdown signal"})
		return nil
	}

	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Shutdown Connect server
	printInfoBox("Graceful shutdown", []string{"Stopping HTTP server"})
	if err := httpServer.Shutdown(ctx); err != nil {
		printErrorBox("Server shutdown failed", []string{err.Error()})
		return err
	}

	// Wait for server goroutine to finish
	serveRuntimeErr = <-serveErr
	if serveRuntimeErr != nil {
		printErrorBox("Server exited because of error", []string{serveRuntimeErr.Error()})
		return serveRuntimeErr
	}
	printSuccessBox("Server exit (graceful)", []string{"All listeners closed cleanly"})

	// Cleanup resources
	err = cnst.ENCODER.Close()
	if err != nil {
		printErrorBox("Cleanup failed", []string{fmt.Sprintf("Encoder close failed: %v", err)})
		return err
	}
	cnst.DECODER.Close()
	printSuccessBox("Cleanup complete", []string{"Encoder/decoder resources released"})

	return err
}

func printInfoBox(title string, lines []string) {
	printBox(title, lines, color.New(color.FgCyan, color.Bold))
}

func printSuccessBox(title string, lines []string) {
	printBox(title, lines, color.New(color.FgGreen, color.Bold))
}

func printWarnBox(title string, lines []string) {
	printBox(title, lines, color.New(color.FgYellow, color.Bold))
}

func printErrorBox(title string, lines []string) {
	printBox(title, lines, color.New(color.FgRed, color.Bold))
}

func printBox(title string, lines []string, titleColor *color.Color) {
	maxWidth := len(title)
	for _, line := range lines {
		if len(line) > maxWidth {
			maxWidth = len(line)
		}
	}

	top := "+" + strings.Repeat("-", maxWidth+2) + "+"
	bottom := top

	titleColor.Println(top)
	titleColor.Printf("| %-*s |\n", maxWidth, title)
	titleColor.Println(top)

	lineColor := color.New(color.Reset)
	for _, line := range lines {
		lineColor.Printf("| %-*s |\n", maxWidth, line)
	}
	titleColor.Println(bottom)
	fmt.Println()
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set CORS headers for web clients
		origin := r.Header.Get("Origin")
		if origin != "" {
			// Allow all origins - customize this for production to restrict to specific domains
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}

		// Handle preflight requests
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers",
				"Content-Type, Connect-Protocol-Version, Connect-Timeout-Ms, "+
					"Connect-Accept-Encoding, Connect-Content-Encoding, "+
					"Grpc-Timeout, X-Grpc-Web, X-User-Agent")
			w.Header().Set("Access-Control-Max-Age", "7200")
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// Expose headers for web clients
		w.Header().Set("Access-Control-Expose-Headers",
			"Connect-Protocol-Version, Connect-Content-Encoding, "+
				"Grpc-Status, Grpc-Message, Grpc-Status-Details-Bin")

		next.ServeHTTP(w, r)
	})
}

func corsInterceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			return next(ctx, req)
		}
	}
}

func ensureUploadDir(uploadsDir string) error {
	_, err := os.Stat(uploadsDir)
	if os.IsNotExist(err) {
		return os.MkdirAll(uploadsDir, os.ModeDir)
	}
	return nil
}

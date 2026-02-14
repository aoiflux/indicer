package main

import (
	"context"
	"errors"
	"fmt"
	"indicer/cli"
	"indicer/lib/cnst"
	"indicer/lib/util"
	"indicer/pb/pbconnect"
	"indicer/server"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

const PORT = "50051"

func init() {
	var err error

	cnst.DECODER, err = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
	handle(err)

	cnst.ENCODER, err = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevel(zstd.SpeedBestCompression)))
	handle(err)
}

func main() {
	log.Println("Ensuring upload dir - ", cnst.UploadsDir)
	ensureUploadDir()

	log.Println("Connecting to DB")
	cnst.KEY = util.HashPassword("")
	db, _, err := cli.Common(int(cnst.DefaultChonkSize), cnst.DefaultDBPath, cnst.KEY)
	handle(err)
	defer db.Close()
	cnst.DB = db
	err = util.EnsureBlobPath(cnst.DefaultDBPath)
	handle(err)

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
		log.Printf("Connect server running at :%s (supports gRPC, gRPC-Web, and Connect protocols)\n", PORT)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	// Set up signal handling for graceful shutdown
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)

	select {
	case sig := <-sigs:
		log.Printf("Received signal %v, initiating graceful shutdown...", sig)
	case err := <-serveErr:
		if err != nil {
			log.Printf("Connect server exited with error: %v", err)
		} else {
			log.Printf("Connect server exited")
		}
	}

	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Shutdown Connect server
	log.Println("Shutting down Connect server...")
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("Error shutting down server: %v", err)
	}

	// Wait for server goroutine to finish
	<-serveErr
	log.Println("Server stopped gracefully")

	// Cleanup resources
	err = cnst.ENCODER.Close()
	handle(err)
	cnst.DECODER.Close()
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

func ensureUploadDir() error {
	_, err := os.Stat(cnst.UploadsDir)
	if os.IsNotExist(err) {
		return os.MkdirAll(cnst.UploadsDir, os.ModeDir)
	}
	return nil
}

func handle(err error) {
	if err != nil {
		fmt.Printf("\n\n %v \n\n", err)
		os.Exit(1)
	}
}

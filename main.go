package main

import (
	"context"
	"errors"
	"fmt"
	"indicer/cli"
	"indicer/lib/cnst"
	"indicer/lib/util"
	"indicer/pb"
	"indicer/server"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"
	"google.golang.org/grpc"
)

func init() {
	var err error

	cnst.DECODER, err = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
	handle(err)

	cnst.ENCODER, err = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevel(zstd.SpeedBestCompression)))
	handle(err)
}

func main() {
	lis, err := net.Listen("tcp", ":50051")
	handle(err)

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

	grpcServer := grpc.NewServer()
	pb.RegisterDuesServiceServer(grpcServer, &server.GrpcService{})

	// Start gRPC server in a goroutine
	serveErr := make(chan error, 1)
	go func() {
		log.Println("gRPC server running at :50051")
		if err := grpcServer.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	// Set up signal handling for graceful shutdown
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)

	var receivedSig os.Signal
	select {
	case receivedSig = <-sigs:
		log.Printf("Received signal %v, initiating graceful shutdown...", receivedSig)
	case err := <-serveErr:
		if err != nil {
			log.Printf("gRPC server exited with error: %v", err)
		} else {
			log.Printf("gRPC server exited")
		}
	}

	// Attempt graceful stop with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		grpcServer.GracefulStop()
		close(done)
	}()

	select {
	case <-done:
		log.Println("gRPC server stopped gracefully")
	case <-ctx.Done():
		log.Println("Graceful shutdown timed out; forcing stop")
		grpcServer.Stop()
	}

	// Ensure listener is closed and wait for Serve to return
	_ = lis.Close()
	<-serveErr

	// Cleanup resources
	err = cnst.ENCODER.Close()
	handle(err)
	cnst.DECODER.Close()
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

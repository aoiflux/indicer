package main

import (
	"fmt"
	"indicer/cli"
	"indicer/lib/cnst"
	"indicer/lib/util"
	"indicer/pb"
	"indicer/server"
	"log"
	"net"
	"os"

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

	log.Println("gRPC server running at :50051")
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("Failed to serve: %v", err)
	}

	err = cnst.ENCODER.Close()
	handle(err)
	cnst.DECODER.Close()
}

func handle(err error) {
	if err != nil {
		fmt.Printf("\n\n %v \n\n", err)
		os.Exit(1)
	}
}

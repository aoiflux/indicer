package server

import (
	"indicer/pb"

	"google.golang.org/grpc"
)

func (g *GrpcService) StreamFile(stream grpc.ClientStreamingServer[pb.StreamFileReq, pb.StreamFileRes]) error {

	return nil
}

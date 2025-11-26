package server

import (
	"indicer/pb"
)

type GrpcService struct {
	pb.UnimplementedDuesServiceServer
}

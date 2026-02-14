package server

import (
	"context"
	"indicer/pb"
	"io"

	"connectrpc.com/connect"
	"google.golang.org/grpc/metadata"
)

// ConnectService wraps GrpcService to be compatible with Connect handlers
type ConnectService struct {
	svc *GrpcService
}

func NewConnectService() *ConnectService {
	return &ConnectService{
		svc: &GrpcService{},
	}
}

func (c *ConnectService) AppendIfExists(
	ctx context.Context,
	req *connect.Request[pb.AppendIfExistsReq],
) (*connect.Response[pb.AppendIfExistsRes], error) {
	res, err := c.svc.AppendIfExists(ctx, req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(res), nil
}

func (c *ConnectService) StreamFile(
	ctx context.Context,
	stream *connect.ClientStream[pb.StreamFileReq],
) (*connect.Response[pb.StreamFileRes], error) {
	// Create an adapter for the stream
	adapter := &connectStreamAdapter{
		clientStream: stream,
		ctx:          ctx,
	}

	// Call the gRPC service method using the adapter
	err := c.svc.StreamFile(adapter)
	if err != nil {
		return nil, err
	}

	// The adapter stores the response from SendAndClose
	return connect.NewResponse(adapter.response), nil
}

func (c *ConnectService) GetEviFiles(
	ctx context.Context,
	req *connect.Request[pb.GetEviFilesReq],
) (*connect.Response[pb.GetEviFilesRes], error) {
	res, err := c.svc.GetEviFiles(ctx, req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(res), nil
}

func (c *ConnectService) GetPartiFiles(
	ctx context.Context,
	req *connect.Request[pb.GetPartiFilesReq],
) (*connect.Response[pb.GetPartiFilesRes], error) {
	res, err := c.svc.GetPartiFiles(ctx, req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(res), nil
}

func (c *ConnectService) GetIdxFiles(
	ctx context.Context,
	req *connect.Request[pb.GetIdxFilesReq],
) (*connect.Response[pb.GetIdxFilesRes], error) {
	res, err := c.svc.GetIdxFiles(ctx, req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(res), nil
}

func (c *ConnectService) Search(
	ctx context.Context,
	req *connect.Request[pb.SearchReq],
) (*connect.Response[pb.SearchRes], error) {
	res, err := c.svc.Search(ctx, req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(res), nil
}

// connectStreamAdapter adapts Connect's ClientStream to gRPC's ClientStreamingServer
type connectStreamAdapter struct {
	clientStream *connect.ClientStream[pb.StreamFileReq]
	ctx          context.Context
	response     *pb.StreamFileRes
}

func (a *connectStreamAdapter) Recv() (*pb.StreamFileReq, error) {
	if !a.clientStream.Receive() {
		if err := a.clientStream.Err(); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
	return a.clientStream.Msg(), nil
}

func (a *connectStreamAdapter) SendAndClose(res *pb.StreamFileRes) error {
	a.response = res
	return nil
}

func (a *connectStreamAdapter) Context() context.Context {
	return a.ctx
}

func (a *connectStreamAdapter) SetHeader(metadata.MD) error {
	return nil
}

func (a *connectStreamAdapter) SendHeader(metadata.MD) error {
	return nil
}

func (a *connectStreamAdapter) SetTrailer(metadata.MD) {
}

func (a *connectStreamAdapter) SendMsg(m any) error {
	return nil
}

func (a *connectStreamAdapter) RecvMsg(m any) error {
	return nil
}

// Code generated by protoc-gen-go-grpc. DO NOT EDIT.
// versions:
// - protoc-gen-go-grpc v1.2.0
// - protoc             v3.12.4
// source: rgrpc.proto

package rgrpc

import (
	context "context"
	grpc "google.golang.org/grpc"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
)

// This is a compile-time assertion to ensure that this generated file
// is compatible with the grpc package it is being compiled against.
// Requires gRPC-Go v1.32.0 or later.
const _ = grpc.SupportPackageIsVersion7

// RgrpcServiceClient is the client API for RgrpcService service.
//
// For semantics around ctx use and closing/ending streaming RPCs, please refer to https://pkg.go.dev/google.golang.org/grpc/?tab=doc#ClientConn.NewStream.
type RgrpcServiceClient interface {
	// OpenTunnel creates a channel to the server which can be used to send
	// additional RPCs, all of which will be sent to the same server via a
	// single underlying gRPC stream. This can provide affinity for a "chatty"
	// sequence of calls, where the gRPC connection is load balanced (so there
	// may be multiple backend servers), but a particular "conversation" (which
	// may consist of numerous RPCs) needs to all go to a single server, for
	// consistency.
	// rpc OpenTunnel(stream ClientToServer) returns (stream ServerToClient);
	// OpenReverseTunnel creates a "reverse" channel, which allows the server to
	// act as a client and send RPCs to the client that creates the tunnel. It
	// is in most respects identical to OpenTunnel except that the roles are
	// reversed: the server initiates RPCs and sends requests and the client
	// replies to them and sends responses.
	OpenRgrpc(ctx context.Context, opts ...grpc.CallOption) (RgrpcService_OpenRgrpcClient, error)
}

type rgrpcServiceClient struct {
	cc grpc.ClientConnInterface
}

func NewRgrpcServiceClient(cc grpc.ClientConnInterface) RgrpcServiceClient {
	return &rgrpcServiceClient{cc}
}

func (c *rgrpcServiceClient) OpenRgrpc(ctx context.Context, opts ...grpc.CallOption) (RgrpcService_OpenRgrpcClient, error) {
	stream, err := c.cc.NewStream(ctx, &RgrpcService_ServiceDesc.Streams[0], "/rgrpc.RgrpcService/OpenRgrpc", opts...)
	if err != nil {
		return nil, err
	}
	x := &rgrpcServiceOpenRgrpcClient{stream}
	return x, nil
}

type RgrpcService_OpenRgrpcClient interface {
	Send(*ServerToClient) error
	Recv() (*ClientToServer, error)
	grpc.ClientStream
}

type rgrpcServiceOpenRgrpcClient struct {
	grpc.ClientStream
}

func (x *rgrpcServiceOpenRgrpcClient) Send(m *ServerToClient) error {
	return x.ClientStream.SendMsg(m)
}

func (x *rgrpcServiceOpenRgrpcClient) Recv() (*ClientToServer, error) {
	m := new(ClientToServer)
	if err := x.ClientStream.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}

// RgrpcServiceServer is the server API for RgrpcService service.
// All implementations must embed UnimplementedRgrpcServiceServer
// for forward compatibility
type RgrpcServiceServer interface {
	// OpenTunnel creates a channel to the server which can be used to send
	// additional RPCs, all of which will be sent to the same server via a
	// single underlying gRPC stream. This can provide affinity for a "chatty"
	// sequence of calls, where the gRPC connection is load balanced (so there
	// may be multiple backend servers), but a particular "conversation" (which
	// may consist of numerous RPCs) needs to all go to a single server, for
	// consistency.
	// rpc OpenTunnel(stream ClientToServer) returns (stream ServerToClient);
	// OpenReverseTunnel creates a "reverse" channel, which allows the server to
	// act as a client and send RPCs to the client that creates the tunnel. It
	// is in most respects identical to OpenTunnel except that the roles are
	// reversed: the server initiates RPCs and sends requests and the client
	// replies to them and sends responses.
	OpenRgrpc(RgrpcService_OpenRgrpcServer) error
	mustEmbedUnimplementedRgrpcServiceServer()
}

// UnimplementedRgrpcServiceServer must be embedded to have forward compatible implementations.
type UnimplementedRgrpcServiceServer struct {
}

func (UnimplementedRgrpcServiceServer) OpenRgrpc(RgrpcService_OpenRgrpcServer) error {
	return status.Errorf(codes.Unimplemented, "method OpenRgrpc not implemented")
}
func (UnimplementedRgrpcServiceServer) mustEmbedUnimplementedRgrpcServiceServer() {}

// UnsafeRgrpcServiceServer may be embedded to opt out of forward compatibility for this service.
// Use of this interface is not recommended, as added methods to RgrpcServiceServer will
// result in compilation errors.
type UnsafeRgrpcServiceServer interface {
	mustEmbedUnimplementedRgrpcServiceServer()
}

func RegisterRgrpcServiceServer(s grpc.ServiceRegistrar, srv RgrpcServiceServer) {
	s.RegisterService(&RgrpcService_ServiceDesc, srv)
}

func _RgrpcService_OpenRgrpc_Handler(srv interface{}, stream grpc.ServerStream) error {
	return srv.(RgrpcServiceServer).OpenRgrpc(&rgrpcServiceOpenRgrpcServer{stream})
}

type RgrpcService_OpenRgrpcServer interface {
	Send(*ClientToServer) error
	Recv() (*ServerToClient, error)
	grpc.ServerStream
}

type rgrpcServiceOpenRgrpcServer struct {
	grpc.ServerStream
}

func (x *rgrpcServiceOpenRgrpcServer) Send(m *ClientToServer) error {
	return x.ServerStream.SendMsg(m)
}

func (x *rgrpcServiceOpenRgrpcServer) Recv() (*ServerToClient, error) {
	m := new(ServerToClient)
	if err := x.ServerStream.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}

// RgrpcService_ServiceDesc is the grpc.ServiceDesc for RgrpcService service.
// It's only intended for direct use with grpc.RegisterService,
// and not to be introspected or modified (even as a copy)
var RgrpcService_ServiceDesc = grpc.ServiceDesc{
	ServiceName: "rgrpc.RgrpcService",
	HandlerType: (*RgrpcServiceServer)(nil),
	Methods:     []grpc.MethodDesc{},
	Streams: []grpc.StreamDesc{
		{
			StreamName:    "OpenRgrpc",
			Handler:       _RgrpcService_OpenRgrpc_Handler,
			ServerStreams: true,
			ClientStreams: true,
		},
	},
	Metadata: "rgrpc.proto",
}
package rgrpc

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"

	"github.com/golang/protobuf/proto"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const maxChunkSize = 16384

// ServeChannel uses the given services to handle incoming RPC requests
// that arrive via the given client tunnel stream. Since this is a reverse
// tunnel, RPC requests are initiated by the server, and this end (the client)
// processes the requests and sends responses.
//
// It returns if, in the process of reading requests, it detects invalid usage
// of the stream (client sending references to invalid stream IDs or sending
// frames for a stream ID in improper order) or if the stream itself fails (for
// example, if the client cancels the tunnel or there is a network disruption).
//
// On return the provided stream should be canceled as soon as possible. Typical
// usage looks like so:
//
//	ctx, cancel := context.WithCancel(ctx)
//	defer cancel()
//	stream, err := stub.OpenRgrpc(ctx)
//	if err != nil {
//	    return err
//	}
//	return rgrpc.Serve(stream, handlers)
func Serve(stream RgrpcService_OpenRgrpcClient, handlers HandlerMap, isClosing func() bool) error {
	svr := &ServerChannel{
		stream:    stream,
		services:  handlers,
		isClosing: isClosing,
		streams:   map[int64]*serverStream{},
		lastSeen:  -1,
	}
	return svr.serve()
}

type streamServer interface {
	Context() context.Context
	Send(*ServerToClient) error
	Recv() (*ClientToServer, error)
}

type ServerChannel struct {
	stream    streamServer
	services  HandlerMap
	isClosing func() bool

	mu       sync.RWMutex
	streams  map[int64]*serverStream
	lastSeen int64
}

func (s *ServerChannel) serve() error {
	ctx, cancel := context.WithCancel(s.stream.Context())
	defer cancel()
	for {
		in, err := s.stream.Recv()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}

		if f, ok := in.Frame.(*ClientToServer_NewStream); ok {
			if ok, err := s.createStream(ctx, in.StreamId, f.NewStream); err != nil {
				if !ok {
					return err
				}

				st, _ := status.FromError(err)
				_ = s.stream.Send(&ServerToClient{
					StreamId: in.StreamId,
					Frame: &ServerToClient_CloseStream{
						CloseStream: &CloseStream{
							Status: st.Proto(),
						},
					},
				})
			}

			continue
		}

		str, err := s.getStream(in.StreamId)
		if err != nil {
			return err
		}
		str.acceptClientFrame(in.Frame)
	}
}

func (s *ServerChannel) createStream(ctx context.Context, streamID int64, frame *NewStream) (bool, error) {
	if s.isClosing() {
		return true, status.Errorf(codes.Unavailable, "server is shutting down")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	_, ok := s.streams[streamID]
	if ok {
		// stream already active!
		return false, fmt.Errorf("cannot create stream ID %d: already exists", streamID)
	}
	if streamID <= s.lastSeen {
		return false, fmt.Errorf("cannot create stream ID %d: that ID has already been used", streamID)
	}
	s.lastSeen = streamID

	if frame.MethodName[0] == '/' {
		frame.MethodName = frame.MethodName[1:]
	}
	parts := strings.SplitN(frame.MethodName, "/", 2)
	if len(parts) != 2 {
		return true, status.Errorf(codes.InvalidArgument, "%s is not a well-formed method name", frame.MethodName)
	}

	service := parts[0]
	method := parts[1]

	srv, knownService := s.services[service]
	if knownService {
		if md, ok := srv.methods[method]; ok {
			s.processUnaryRPC(ctx, streamID, frame, md, srv)
			return true, nil
		}
		if sd, ok := srv.streams[method]; ok {
			s.processStreamingRPC(ctx, streamID, frame, sd, srv)
			return true, nil
		}
	}

	delete(s.streams, streamID)
	return true, status.Errorf(codes.Unimplemented, "%s not implemented", frame.MethodName)
}

func (s *ServerChannel) processUnaryRPC(ctx context.Context, streamID int64, frame *NewStream, md *grpc.MethodDesc, srv *serviceInfo) {
	ctx = metadata.NewIncomingContext(ctx, fromProto(frame.RequestHeaders))

	ch := make(chan isClientToServer_Frame, 1)
	str := &serverStream{
		ctx:            ctx,
		svr:            s,
		streamID:       streamID,
		method:         frame.MethodName,
		stream:         s.stream,
		isClientStream: false,
		isServerStream: false,
		readChan:       ch,
		ingestChan:     ch,
	}
	s.streams[streamID] = str
	str.ctx = grpc.NewContextWithServerTransportStream(str.ctx, (*serverChannelTransportStream)(str))

	go str.processUnaryRPC(md, srv.serviceImpl)
}

func (s *ServerChannel) processStreamingRPC(ctx context.Context, streamID int64, frame *NewStream, sd *grpc.StreamDesc, srv *serviceInfo) {
	ctx = metadata.NewIncomingContext(ctx, fromProto(frame.RequestHeaders))

	ch := make(chan isClientToServer_Frame, 1)
	str := &serverStream{
		ctx:            ctx,
		svr:            s,
		streamID:       streamID,
		method:         frame.MethodName,
		stream:         s.stream,
		isClientStream: sd.ClientStreams,
		isServerStream: sd.ServerStreams,
		readChan:       ch,
		ingestChan:     ch,
	}
	s.streams[streamID] = str
	str.ctx = grpc.NewContextWithServerTransportStream(str.ctx, (*serverChannelTransportStream)(str))

	go str.processStreamingRPC(sd, srv.serviceImpl)
}

func (s *ServerChannel) getStream(streamID int64) (*serverStream, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	target, ok := s.streams[streamID]
	if !ok {
		if streamID <= s.lastSeen {
			// used and disposed of stream; ignore subsequent frames
			return nil, nil
		}
		// stream never created!
		return nil, fmt.Errorf("received frame for stream ID %d: stream never created", streamID)
	}

	return target, nil
}

func (s *ServerChannel) removeStream(streamID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.streams, streamID)
}

func findMethod(sd *grpc.ServiceDesc, method string) interface{} {
	for i, md := range sd.Methods {
		if md.MethodName == method {
			return &sd.Methods[i]
		}
	}
	for i, md := range sd.Streams {
		if md.StreamName == method {
			return &sd.Streams[i]
		}
	}
	return nil
}

type serverStream struct {
	ctx      context.Context
	svr      *ServerChannel
	streamID int64
	method   string
	stream   streamServer

	isClientStream bool
	isServerStream bool

	// for "ingesting" frames into channel, from receive loop
	ingestMu   sync.Mutex
	ingestChan chan<- isClientToServer_Frame
	halfClosed error

	// for reading frames from channel, to read message data
	readMu   sync.Mutex
	readChan <-chan isClientToServer_Frame
	readErr  error

	// for sending frames to client
	writeMu     sync.Mutex
	numSent     uint32
	headers     metadata.MD
	trailers    metadata.MD
	sentHeaders bool
	closed      bool
}

func (st *serverStream) acceptClientFrame(frame isClientToServer_Frame) {
	if st == nil {
		// can happen if server decided that the stream ID was recently used
		// yet inactive -- it returns nil error but also nil stream, which
		// just discards incoming messages (we assume they arrive late, racing
		// with stream being closed)
		return
	}

	switch frame.(type) {
	case *ClientToServer_HalfClose:
		st.halfClose(io.EOF)
		return

	case *ClientToServer_Cancel:
		st.finishStream(context.Canceled)
		return
	}

	st.ingestMu.Lock()
	defer st.ingestMu.Unlock()

	if st.halfClosed != nil {
		// stream is half closed -- ignore subsequent messages
		return
	}

	select {
	case st.ingestChan <- frame:
	case <-st.ctx.Done():
	}
}

func (st *serverStream) SetHeader(md metadata.MD) error {
	return st.setHeader(md, false)
}

func (st *serverStream) SendHeader(md metadata.MD) error {
	return st.setHeader(md, true)
}

func (st *serverStream) setHeader(md metadata.MD, send bool) error {
	st.writeMu.Lock()
	defer st.writeMu.Unlock()

	if st.sentHeaders {
		return errors.New("already sent headers")
	}
	if md != nil {
		st.headers = metadata.Join(st.headers, md)
	}
	if send {
		return st.sendHeadersLocked()
	}
	return nil
}

func (st *serverStream) sendHeadersLocked() error {
	err := st.stream.Send(&ServerToClient{
		StreamId: st.streamID,
		Frame: &ServerToClient_ResponseHeaders{
			ResponseHeaders: toProto(st.headers),
		},
	})
	st.sentHeaders = true
	st.headers = nil
	return err
}

func fromProto(md *Metadata) metadata.MD {
	if md == nil {
		return nil
	}
	vals := metadata.MD{}
	for k, v := range md.Md {
		vals[k] = v.Val
	}
	return vals
}

func toProto(md metadata.MD) *Metadata {
	vals := map[string]*Metadata_Values{}
	for k, v := range md {
		vals[k] = &Metadata_Values{Val: v}
	}
	return &Metadata{Md: vals}
}

func (st *serverStream) SetTrailer(md metadata.MD) {
	_ = st.setTrailer(md)
}

func (st *serverStream) setTrailer(md metadata.MD) error {
	st.writeMu.Lock()
	defer st.writeMu.Unlock()

	if st.closed {
		return errors.New("already finished")
	}
	st.trailers = metadata.Join(st.trailers, md)
	return nil
}

func (st *serverStream) Context() context.Context {
	return st.ctx
}

func (st *serverStream) SendMsg(m interface{}) error {
	st.writeMu.Lock()
	defer st.writeMu.Unlock()

	if !st.sentHeaders {
		if err := st.sendHeadersLocked(); err != nil {
			return err
		}
	}

	if !st.isServerStream && st.numSent == 1 {
		return status.Errorf(codes.Internal, "Already sent response for non-server-stream method %v", st.method)
	}
	st.numSent++

	// TODO: support alternate codecs, compressors, etc
	b, err := proto.Marshal(m.(proto.Message))
	if err != nil {
		return err
	}

	i := 0
	for {
		if err := st.ctx.Err(); err != nil {
			return err
		}

		chunk := b
		if len(b) > maxChunkSize {
			chunk = b[:maxChunkSize]
		}

		if i == 0 {
			err = st.stream.Send(&ServerToClient{
				StreamId: st.streamID,
				Frame: &ServerToClient_ResponseMessage{
					ResponseMessage: &MessageData{
						Size: int32(len(b)),
						Data: chunk,
					},
				},
			})
		} else {
			err = st.stream.Send(&ServerToClient{
				StreamId: st.streamID,
				Frame: &ServerToClient_MoreResponseData{
					MoreResponseData: chunk,
				},
			})
		}

		if err != nil {
			return err
		}

		if len(b) <= maxChunkSize {
			break
		}

		b = b[maxChunkSize:]
		i++
	}

	return nil
}

func (st *serverStream) RecvMsg(m interface{}) error {
	data, ok, err := st.readMsg()
	if err != nil {
		if !ok {
			st.finishStream(err)
		}
		return err
	}
	// TODO: support alternate codecs, compressors, etc
	return proto.Unmarshal(data, m.(proto.Message))
}

func (st *serverStream) readMsg() (data []byte, ok bool, err error) {
	st.readMu.Lock()
	defer st.readMu.Unlock()

	data, ok, err = st.readMsgLocked()
	if err == nil && !st.isClientStream {
		// no stream; so eagerly see if there's another message
		// and fail RPC if so (due to bad input)
		_, ok, err := st.readMsgLocked()
		if err == nil {
			err = status.Errorf(codes.InvalidArgument, "Already received request for non-client-stream method %v", st.method)
			st.readErr = err
			return nil, false, err
		}
		if err != io.EOF || !ok {
			return nil, ok, err
		}
	}

	return data, ok, err
}

func (st *serverStream) readMsgLocked() (data []byte, ok bool, err error) {
	if st.readErr != nil {
		return nil, true, st.readErr
	}

	defer func() {
		if err != nil {
			st.readErr = err
		}
	}()

	msgLen := -1
	var b []byte
	for {
		// if stream is canceled, return context error
		if err := st.ctx.Err(); err != nil {
			return nil, true, err
		}

		// otherwise, try to read request data, but interrupt if
		// stream is canceled or half-closed
		select {
		case <-st.ctx.Done():
			return nil, true, st.ctx.Err()

		case in, ok := <-st.readChan:
			if !ok {
				// don't need lock to read st.halfClosed; observing
				// input channel close provides safe visibility
				return nil, true, st.halfClosed
			}

			switch in := in.(type) {
			case *ClientToServer_RequestMessage:
				if msgLen != -1 {
					return nil, false, status.Errorf(codes.InvalidArgument, "received redundant request message envelope")
				}
				msgLen = int(in.RequestMessage.Size)
				b = in.RequestMessage.Data
				if len(b) > msgLen {
					return nil, false, status.Errorf(codes.InvalidArgument, "received more data than indicated by request message envelope")
				}
				if len(b) == msgLen {
					return b, true, nil
				}

			case *ClientToServer_MoreRequestData:
				if msgLen == -1 {
					return nil, false, status.Errorf(codes.InvalidArgument, "never received envelope for request message")
				}
				b = append(b, in.MoreRequestData...)
				if len(b) > msgLen {
					return nil, false, status.Errorf(codes.InvalidArgument, "received more data than indicated by request message envelope")
				}
				if len(b) == msgLen {
					return b, true, nil
				}

			default:
				return nil, false, status.Errorf(codes.InvalidArgument, "unrecognized frame type: %T", in)
			}
		}
	}
}

func (st *serverStream) processUnaryRPC(md *grpc.MethodDesc, srv interface{}) {
	var err error
	panicked := true // pessimistic assumption

	defer func() {
		if err == nil && panicked {
			err = status.Errorf(codes.Internal, "panic")
		}
		st.finishStream(err)
	}()

	var resp interface{}
	resp, err = md.Handler(srv, st.ctx, st.RecvMsg, nil)
	if err == nil {
		err = st.SendMsg(resp)
	}

	// if we get here, we did not panic
	panicked = false
}

func (st *serverStream) processStreamingRPC(sd *grpc.StreamDesc, srv interface{}) {
	var err error
	panicked := true // pessimistic assumption

	defer func() {
		if err == nil && panicked {
			err = status.Errorf(codes.Internal, "panic")
		}
		st.finishStream(err)
	}()

	err = sd.Handler(srv, st)

	// if we get here, we did not panic
	panicked = false
}

func (st *serverStream) finishStream(err error) {
	st.svr.removeStream(st.streamID)

	st.halfClose(err)

	st.writeMu.Lock()
	defer st.writeMu.Unlock()

	if st.closed {
		return
	}

	if !st.sentHeaders {
		_ = st.sendHeadersLocked()
	}

	stat, _ := status.FromError(err)
	_ = st.stream.Send(&ServerToClient{
		StreamId: st.streamID,
		Frame: &ServerToClient_CloseStream{
			CloseStream: &CloseStream{
				Status:           stat.Proto(),
				ResponseTrailers: toProto(st.trailers),
			},
		},
	})

	st.closed = true
	st.trailers = nil
}

func (st *serverStream) halfClose(err error) {
	st.ingestMu.Lock()
	defer st.ingestMu.Unlock()

	if st.halfClosed != nil {
		// already closed
		return
	}

	if err == nil {
		err = io.EOF
	}
	st.halfClosed = err
	close(st.ingestChan)
}

type serverChannelTransportStream serverStream

func (st *serverChannelTransportStream) Method() string {
	return (*serverStream)(st).method
}

func (st *serverChannelTransportStream) SetHeader(md metadata.MD) error {
	return (*serverStream)(st).SetHeader(md)
}

func (st *serverChannelTransportStream) SendHeader(md metadata.MD) error {
	return (*serverStream)(st).SendHeader(md)
}

func (st *serverChannelTransportStream) SetTrailer(md metadata.MD) error {
	return (*serverStream)(st).setTrailer(md)
}

// HandlerMap is used to accumulate service handlers into a map. The handlers
// can be registered once in the map, and then re-used to configure multiple
// servers that should expose the same handlers. HandlerMap can also be used
// as the internal store of registered handlers for a server implementation.
type HandlerMap map[string]*serviceInfo

// serviceInfo wraps information about a service. It is very similar to
// ServiceDesc and is constructed from it for internal purposes.
type serviceInfo struct {
	// Contains the implementation for the methods in this service.
	serviceImpl interface{}
	methods     map[string]*grpc.MethodDesc
	streams     map[string]*grpc.StreamDesc
	mdata       interface{}
}

// RegisterService registers a service and its implementation to the gRPC
// server. It is called from the IDL generated code. This must be called before
// invoking Serve. If ss is non-nil (for legacy code), its type is checked to
// ensure it implements sd.HandlerType.
func (m HandlerMap) RegisterService(desc *grpc.ServiceDesc, ss interface{}) {
	if ss != nil {
		ht := reflect.TypeOf(desc.HandlerType).Elem()
		st := reflect.TypeOf(ss)
		if !st.Implements(ht) {
			panic(fmt.Sprintf("service %s: handler of type %v does not satisfy %v", desc.ServiceName, st, ht))
		}
	}
	m.register(desc, ss)
}

func (m HandlerMap) register(desc *grpc.ServiceDesc, ss interface{}) {
	if _, ok := m[desc.ServiceName]; ok {
		panic(fmt.Sprintf("service %s: handler already registered", desc.ServiceName))
	}
	info := &serviceInfo{
		serviceImpl: ss,
		methods:     make(map[string]*grpc.MethodDesc),
		streams:     make(map[string]*grpc.StreamDesc),
		mdata:       desc.Metadata,
	}
	for i := range desc.Methods {
		d := &desc.Methods[i]
		info.methods[d.MethodName] = d
	}
	for i := range desc.Streams {
		d := &desc.Streams[i]
		info.streams[d.StreamName] = d
	}
	m[desc.ServiceName] = info
}

// GetServiceInfo returns a map from service names to ServiceInfo.
// Service names include the package names, in the form of <package>.<service>.
func (m HandlerMap) GetServiceInfo() map[string]grpc.ServiceInfo {
	ret := make(map[string]grpc.ServiceInfo)
	for n, srv := range m {
		methods := make([]grpc.MethodInfo, 0, len(srv.methods)+len(srv.streams))
		for m := range srv.methods {
			methods = append(methods, grpc.MethodInfo{
				Name:           m,
				IsClientStream: false,
				IsServerStream: false,
			})
		}
		for m, d := range srv.streams {
			methods = append(methods, grpc.MethodInfo{
				Name:           m,
				IsClientStream: d.ClientStreams,
				IsServerStream: d.ServerStreams,
			})
		}

		ret[n] = grpc.ServiceInfo{
			Methods:  methods,
			Metadata: srv.mdata,
		}
	}
	return ret
}

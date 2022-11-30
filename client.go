package rgrpc

import (
	"errors"
	"fmt"
	"io"
	reflect "reflect"
	"sync"

	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes/empty"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type streamClient interface {
	Context() context.Context
	Send(*ClientToServer) error
	Recv() (*ServerToClient, error)
}

type Client struct {
	stream streamClient
	ctx    context.Context
	cancel context.CancelFunc

	mu            sync.RWMutex
	streams       map[int64]*clientStream
	lastStreamID  int64
	streamCreated bool
	err           error
	finished      bool

	Peer           *peer.Peer
	RequestHeaders metadata.MD
}

func NewClient(stream RgrpcService_OpenRgrpcServer) *Client {
	p, _ := peer.FromContext(stream.Context())
	md, _ := metadata.FromIncomingContext(stream.Context())

	ctx, cancel := context.WithCancel(stream.Context())
	c := &Client{
		stream:         stream,
		ctx:            ctx,
		cancel:         cancel,
		streams:        map[int64]*clientStream{},
		Peer:           p,
		RequestHeaders: md,
	}
	go c.recvLoop()
	return c
}

func (c *Client) Context() context.Context {
	return c.ctx
}

func (c *Client) Done() <-chan struct{} {
	return c.ctx.Done()
}

func (c *Client) IsDone() bool {
	if c.ctx.Err() != nil {
		return true
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.finished
}

func (c *Client) Err() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	switch c.err {
	case nil:
		return c.ctx.Err()
	case io.EOF:
		return nil
	default:
		return c.err
	}
}

func (c *Client) Close() {
	c.close(nil)
}

func (c *Client) Invoke(ctx context.Context, methodName string, req, resp interface{}, opts ...grpc.CallOption) error {
	str, err := c.newStream(ctx, false, false, methodName, opts...)
	if err != nil {
		return err
	}
	if err := str.SendMsg(req); err != nil {
		return err
	}
	if err := str.CloseSend(); err != nil {
		return err
	}
	err = str.RecvMsg(resp)
	if err != nil {
		return err
	}

	// Make sure there are no more messages on the stream.
	// Allocate another response (to make sure this call to
	// RecvMsg can't modify the resp we already received).
	rv := reflect.Indirect(reflect.ValueOf(resp))
	extraResp := reflect.New(rv.Type()).Interface()
	extraErr := str.RecvMsg(extraResp)
	switch extraErr {
	case nil:
		return status.Errorf(codes.Internal, "unary RPC returned >1 response message")
	case io.EOF:
		// this is what we want: nothing else in the stream
		return nil
	default:
		return err
	}
}

func (c *Client) NewStream(ctx context.Context, desc *grpc.StreamDesc, methodName string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	return c.newStream(ctx, desc.ClientStreams, desc.ServerStreams, methodName, opts...)
}

func (c *Client) newStream(ctx context.Context, clientStreams, serverStreams bool, methodName string, opts ...grpc.CallOption) (*clientStream, error) {
	str, md, err := c.allocateStream(ctx, clientStreams, serverStreams, methodName, opts)
	if err != nil {
		return nil, err
	}
	err = c.stream.Send(&ClientToServer{
		StreamId: str.streamID,
		Frame: &ClientToServer_NewStream{
			NewStream: &NewStream{
				MethodName:     methodName,
				RequestHeaders: toProto(md),
			},
		},
	})
	if err != nil {
		c.removeStream(str.streamID)
		return nil, err
	}
	go func() {
		// if context gets cancelled, make sure
		// we shutdown the stream
		<-str.ctx.Done()
		str.cancel(str.ctx.Err())
	}()
	return str, nil
}

func (c *Client) allocateStream(ctx context.Context, clientStreams, serverStreams bool, methodName string, opts []grpc.CallOption) (*clientStream, metadata.MD, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.finished {
		return nil, nil, errors.New("channel is closed")
	}

	if c.lastStreamID == -1 {
		return nil, nil, errors.New("all stream IDs exhausted (must create a new channel)")
	}

	c.streamCreated = true
	c.lastStreamID++
	streamID := c.lastStreamID
	if _, ok := c.streams[streamID]; ok {
		// should never happen... panic?
		return nil, nil, errors.New("next stream ID not available")
	}

	md, _ := metadata.FromOutgoingContext(ctx)
	var hdrs, tlrs []*metadata.MD
	pr, _ := peer.FromContext(c.ctx)
	authority := "<unknown>"
	isSecure := false
	if pr != nil {
		authority = pr.Addr.String()
		isSecure = pr.AuthInfo != nil
	}

	for _, opt := range opts {
		switch opt := opt.(type) {
		case grpc.HeaderCallOption:
			hdrs = append(hdrs, opt.HeaderAddr)

		case grpc.TrailerCallOption:
			tlrs = append(tlrs, opt.TrailerAddr)

		case grpc.PeerCallOption:
			if pr != nil {
				*opt.PeerAddr = *pr
			}

		case grpc.PerRPCCredsCallOption:
			if opt.Creds.RequireTransportSecurity() && !isSecure {
				return nil, nil, fmt.Errorf("per-RPC credentials %T cannot be used with insecure channel", opt.Creds)
			}

			mdVals, err := opt.Creds.GetRequestMetadata(ctx, fmt.Sprintf("tunnel://%s%s", authority, methodName))
			if err != nil {
				return nil, nil, err
			}
			for k, v := range mdVals {
				md.Append(k, v)
			}

			// TODO: custom codec and compressor support
			//case grpc.ContentSubtypeCallOption:
			//case grpc.CustomCodecCallOption:
			//case grpc.CompressorCallOption:
		}
	}

	ch := make(chan isServerToClient_Frame, 1)
	ctx, cncl := context.WithCancel(ctx)
	str := &clientStream{
		ctx:              ctx,
		cncl:             cncl,
		ch:               c,
		streamID:         streamID,
		method:           methodName,
		stream:           c.stream,
		headersTargets:   hdrs,
		trailersTargets:  tlrs,
		isClientStream:   clientStreams,
		isServerStream:   serverStreams,
		ingestChan:       ch,
		readChan:         ch,
		gotHeadersSignal: make(chan struct{}),
		doneSignal:       make(chan struct{}),
	}
	c.streams[streamID] = str

	return str, md, nil
}

func (c *Client) recvLoop() {
	for {
		in, err := c.stream.Recv()
		if err != nil {
			c.close(err)
			return
		}
		str, err := c.getStream(in.StreamId)
		if err != nil {
			c.close(err)
			return
		}
		str.acceptServerFrame(in.Frame)
	}
}

func (c *Client) getStream(streamID int64) (*clientStream, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	target, ok := c.streams[streamID]
	if !ok {
		if c.streamCreated && streamID <= c.lastStreamID {
			// used and disposed of stream; ignore subsequent frames
			return nil, nil
		}
		// stream never created!
		return nil, fmt.Errorf("received frame for stream ID %d: stream never created", streamID)
	}

	return target, nil
}

func (c *Client) removeStream(streamID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.streams != nil {
		delete(c.streams, streamID)
	}
}

func (c *Client) close(err error) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.finished {
		return false
	}

	defer c.cancel()

	c.finished = true
	if err == nil {
		err = io.EOF
	}
	c.err = err
	for _, st := range c.streams {
		st.cncl()
	}
	c.streams = nil
	return true
}

type clientStream struct {
	ctx      context.Context
	cncl     context.CancelFunc
	ch       *Client
	streamID int64
	method   string
	stream   streamClient

	headersTargets  []*metadata.MD
	trailersTargets []*metadata.MD

	isClientStream bool
	isServerStream bool

	// for "ingesting" frames into channel, from receive loop
	ingestMu         sync.Mutex
	ingestChan       chan<- isServerToClient_Frame
	gotHeaders       bool
	gotHeadersSignal chan struct{}
	headers          metadata.MD
	done             error
	doneSignal       chan struct{}
	trailers         metadata.MD

	// for reading frames from channel, to read message data
	readMu   sync.Mutex
	readChan <-chan isServerToClient_Frame
	readErr  error

	// for sending frames to server
	writeMu    sync.Mutex
	numSent    uint32
	halfClosed bool
}

func (st *clientStream) Header() (metadata.MD, error) {
	// if we've already received headers, return them
	select {
	case <-st.gotHeadersSignal:
		return st.headers, nil
	default:
	}

	select {
	case <-st.gotHeadersSignal:
		return st.headers, nil
	case <-st.ctx.Done():
		// in the event of a race, always respect getting headers first
		select {
		case <-st.gotHeadersSignal:
			return st.headers, nil
		default:
		}
		return nil, st.ctx.Err()
	}
}

func (st *clientStream) Trailer() metadata.MD {
	// Unlike Header(), this method does not block and should only be
	// used by client after stream is closed.
	select {
	case <-st.doneSignal:
		return st.trailers
	default:
		return nil
	}
}

func (st *clientStream) CloseSend() error {
	st.writeMu.Lock()
	defer st.writeMu.Unlock()

	select {
	case <-st.doneSignal:
		return st.done
	default:
		// don't block since we are holding writeMu
	}

	if st.halfClosed {
		return errors.New("already half-closed")
	}
	st.halfClosed = true
	return st.stream.Send(&ClientToServer{
		StreamId: st.streamID,
		Frame: &ClientToServer_HalfClose{
			HalfClose: &empty.Empty{},
		},
	})
}

func (st *clientStream) Context() context.Context {
	return st.ctx
}

func (st *clientStream) SendMsg(m interface{}) error {
	st.writeMu.Lock()
	defer st.writeMu.Unlock()

	if !st.isClientStream && st.numSent == 1 {
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
		if err := st.err(); err != nil {
			return io.EOF
		}

		chunk := b
		if len(b) > maxChunkSize {
			chunk = b[:maxChunkSize]
		}

		if i == 0 {
			err = st.stream.Send(&ClientToServer{
				StreamId: st.streamID,
				Frame: &ClientToServer_RequestMessage{
					RequestMessage: &MessageData{
						Size: int32(len(b)),
						Data: chunk,
					},
				},
			})
		} else {
			err = st.stream.Send(&ClientToServer{
				StreamId: st.streamID,
				Frame: &ClientToServer_MoreRequestData{
					MoreRequestData: chunk,
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

func (st *clientStream) RecvMsg(m interface{}) error {
	data, ok, err := st.readMsg()
	if err != nil {
		if !ok {
			st.cancel(err)
		}
		return err
	}
	// TODO: support alternate codecs, compressors, etc
	return proto.Unmarshal(data, m.(proto.Message))
}

func (st *clientStream) readMsg() (data []byte, ok bool, err error) {
	st.readMu.Lock()
	defer st.readMu.Unlock()

	data, ok, err = st.readMsgLocked()
	if err == nil && !st.isServerStream {
		// no stream; so eagerly see if there's another message
		// and fail RPC if so (due to bad input)
		_, ok, err := st.readMsgLocked()
		if err == nil {
			err = status.Errorf(codes.Internal, "Server sent multiple responses for non-server-stream method %v", st.method)
			st.readErr = err
			return nil, false, err
		}
		if err != io.EOF || !ok {
			return nil, ok, err
		}
	}

	return data, ok, err
}

func (st *clientStream) readMsgLocked() (data []byte, ok bool, err error) {
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
		in, ok := <-st.readChan
		if !ok {
			// don't need lock to read st.done; observing
			// input channel close provides safe visibility
			return nil, true, st.done
		}

		switch in := in.(type) {
		case *ServerToClient_ResponseMessage:
			if msgLen != -1 {
				return nil, false, status.Errorf(codes.Internal, "server sent redundant response message envelope")
			}
			msgLen = int(in.ResponseMessage.Size)
			b = in.ResponseMessage.Data
			if len(b) > msgLen {
				return nil, false, status.Errorf(codes.Internal, "server sent more data than indicated by response message envelope")
			}
			if len(b) == msgLen {
				return b, true, nil
			}

		case *ServerToClient_MoreResponseData:
			if msgLen == -1 {
				return nil, false, status.Errorf(codes.Internal, "server never sent envelope for response message")
			}
			b = append(b, in.MoreResponseData...)
			if len(b) > msgLen {
				return nil, false, status.Errorf(codes.Internal, "server sent more data than indicated by response message envelope")
			}
			if len(b) == msgLen {
				return b, true, nil
			}

		default:
			return nil, false, status.Errorf(codes.Internal, "unrecognized frame type: %T", in)
		}
	}
}

func (st *clientStream) err() error {
	select {
	case <-st.doneSignal:
		return st.done
	default:
		return st.ctx.Err()
	}
}

func (st *clientStream) acceptServerFrame(frame isServerToClient_Frame) {
	if st == nil {
		// can happen if client decided that the stream ID was recently used
		// yet inactive -- it returns nil error but also nil stream, which
		// just discards incoming messages (we assume they arrive late, racing
		// with stream being closed)
		return
	}

	switch frame := frame.(type) {
	case *ServerToClient_ResponseHeaders:
		st.ingestMu.Lock()
		defer st.ingestMu.Unlock()
		if st.gotHeaders {
			// TODO: cancel RPC and fail locally with internal error?
			return
		}
		st.gotHeaders = true
		st.headers = fromProto(frame.ResponseHeaders)
		for _, hdrs := range st.headersTargets {
			*hdrs = st.headers
		}
		close(st.gotHeadersSignal)
		return

	case *ServerToClient_CloseStream:
		trailers := fromProto(frame.CloseStream.ResponseTrailers)
		err := status.FromProto(frame.CloseStream.Status).Err()
		st.finishStream(err, trailers)
	}

	st.ingestMu.Lock()
	defer st.ingestMu.Unlock()

	if st.done != nil {
		return
	}

	select {
	case st.ingestChan <- frame:
	case <-st.ctx.Done():
	}
}

func (st *clientStream) cancel(err error) {
	st.finishStream(err, nil)
	// let server know
	st.writeMu.Lock()
	defer st.writeMu.Unlock()
	_ = st.stream.Send(&ClientToServer{
		StreamId: st.streamID,
		Frame: &ClientToServer_Cancel{
			Cancel: &empty.Empty{},
		},
	})
}

func (st *clientStream) finishStream(err error, trailers metadata.MD) {
	st.ch.removeStream(st.streamID)
	defer st.cncl()

	st.ingestMu.Lock()
	defer st.ingestMu.Unlock()

	if st.done != nil {
		// RPC already finished! just ignore...
		return
	}
	st.trailers = trailers
	for _, tlrs := range st.trailersTargets {
		*tlrs = trailers
	}
	if !st.gotHeaders {
		st.gotHeaders = true
		close(st.gotHeadersSignal)
	}
	switch err {
	case nil:
		err = io.EOF
	case context.DeadlineExceeded:
		err = status.Error(codes.DeadlineExceeded, err.Error())
	case context.Canceled:
		err = status.Error(codes.Canceled, err.Error())
	}
	st.done = err

	close(st.ingestChan)
	close(st.doneSignal)
}

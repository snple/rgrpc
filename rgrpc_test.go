package rgrpc

import (
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/fullstorydev/grpchan/grpchantesting"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestTunnelServer(t *testing.T) {
	// Basic tests of the tunnel service as a gRPC channel

	var svr grpchantesting.TestServer

	ready := make(chan struct{})
	ts := RrpcService{
		OnChannelConnect: func(*Client) {
			// don't block; just make sure there's something in the channel
			select {
			case ready <- struct{}{}:
			default:
			}
		},
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	gs := grpc.NewServer()
	RegisterRgrpcServiceServer(gs, &ts)
	go gs.Serve(l)
	defer gs.Stop()

	cc, err := grpc.Dial(l.Addr().String(), grpc.WithBlock(), grpc.WithInsecure())
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	defer cc.Close()

	cli := NewRgrpcServiceClient(cc)

	checkForGoroutineLeak(t, func() {
		stream, err := cli.OpenRgrpc(context.Background())
		if err != nil {
			t.Fatalf("failed to open reverse stream: %v", err)
		}

		// client now acts as the server
		handlerMap := HandlerMap{}
		grpchantesting.RegisterHandlerTestService(handlerMap, &svr)
		errs := make(chan error)
		go func() {
			errs <- Serve(stream, handlerMap, func() bool { return false })
		}()

		defer func() {
			stream.CloseSend()
			err := <-errs
			if err != nil {
				t.Errorf("ServeReverseTunnel returned error: %v", err)
			}
		}()

		// make sure server has registered client, so we can issue RPCs to it
		<-ready
		ch := ts.AsChannel()

		grpchantesting.RunChannelTestCases(t, ch, true)

		time.Sleep(time.Millisecond)
	})
}

func checkForGoroutineLeak(t *testing.T, fn func()) {
	before := runtime.NumGoroutine()

	fn()

	// check for goroutine leaks
	deadline := time.Now().Add(time.Second * 5)
	after := 0
	for deadline.After(time.Now()) {
		after = runtime.NumGoroutine()
		if after <= before {
			// number of goroutines returned to previous level: no leak!
			return
		}
		time.Sleep(time.Millisecond * 50)
	}
	buf := make([]byte, 1024*1024)
	n := runtime.Stack(buf, true)
	t.Errorf("%d goroutines leaked:\n%s", after-before, string(buf[:n]))
}

// TODO: also need more tests around channel lifecycle, and ensuring it
// properly respects things like context cancellations, etc

// TODO: also need some concurrency checks, to make sure the channel works
// as expected, and race detector finds no bugs, when used from many
// goroutines at once

// RrpcService provides an implementation for grpctunnel.TunnelServiceServer.
// You can register handlers with it, and it will then expose those handlers
// for incoming tunnels. If no handlers are registered, the server will reply
// to OpenTunnel requests with an "Unimplemented" error code. The server may
// still be used for reverse tunnels
//
// For reverse tunnels, if supported, all connected channels (e.g. all clients
// that have created reverse tunnels) are available. You can also configure a
// listener to receive notices when channels are connected and disconnected.
type RrpcService struct {
	// If set, reverse tunnels will not be allowed. The server will reply to
	// OpenReverseTunnel requests with an "Unimplemented" error code.
	// NoReverseTunnels bool
	// If reverse tunnels are allowed, this callback may be configured to
	// receive information when clients open a reverse tunnel.
	OnChannelConnect func(*Client)
	// If reverse tunnels are allowed, this callback may be configured to
	// receive information when reverse tunnels are torn down.
	OnChannelDisconnect func(*Client)
	// Optional function that accepts a reverse tunnel and returns an affinity
	// key. The affinity key values can be used to look up outbound channels,
	// for targeting calls to particular clients or groups of clients.
	AffinityKey func(*Client) any

	channels Channels

	mu            sync.RWMutex
	channelsByKey map[any]*Channels

	UnimplementedRgrpcServiceServer
}

var _ RgrpcServiceServer = (*RrpcService)(nil)

func (s *RrpcService) OpenRgrpc(stream RgrpcService_OpenRgrpcServer) error {
	ch := NewClient(stream)
	defer ch.Close()

	var key any
	if s.AffinityKey != nil {
		key = s.AffinityKey(ch)
	}

	s.channels.add(ch)
	defer s.channels.remove(ch)

	rc := func() *Channels {
		s.mu.Lock()
		defer s.mu.Unlock()

		rc := s.channelsByKey[key]
		if rc == nil {
			rc = &Channels{}
			if s.channelsByKey == nil {
				s.channelsByKey = map[any]*Channels{}
			}
			s.channelsByKey[key] = rc
		}
		return rc
	}()
	rc.add(ch)
	defer rc.remove(ch)

	if s.OnChannelConnect != nil {
		s.OnChannelConnect(ch)
	}
	if s.OnChannelDisconnect != nil {
		defer s.OnChannelDisconnect(ch)
	}

	<-ch.Done()
	return ch.Err()
}

type Channels struct {
	mu    sync.Mutex
	chans []*Client
	idx   int
}

func (c *Channels) allChans() []*Client {
	c.mu.Lock()
	defer c.mu.Unlock()

	cp := make([]*Client, len(c.chans))
	copy(cp, c.chans)
	return cp
}

func (c *Channels) pick() grpc.ClientConnInterface {
	if c == nil {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.chans) == 0 {
		return nil
	}
	c.idx++
	if c.idx >= len(c.chans) {
		c.idx = 0
	}
	return c.chans[c.idx]
}

func (c *Channels) add(ch *Client) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.chans = append(c.chans, ch)
}

func (c *Channels) remove(ch *Client) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i := range c.chans {
		if c.chans[i] == ch {
			c.chans = append(c.chans[:i], c.chans[i+1:]...)
			break
		}
	}
}

func (s *RrpcService) AllTunnels() []*Client {
	return s.channels.allChans()
}

func (s *RrpcService) AsChannel() grpc.ClientConnInterface {
	return multiChannel(s.channels.pick)
}

func (s *RrpcService) KeyAsChannel(key any) grpc.ClientConnInterface {
	return multiChannel(func() grpc.ClientConnInterface {
		return s.pickKey(key)
	})
}

func (s *RrpcService) FindChannel(search func(*Client) bool) *Client {
	allChans := s.channels.allChans()

	for _, ch := range allChans {
		if !ch.IsDone() && search(ch) {
			return ch
		}
	}
	return nil
}

func (s *RrpcService) pickKey(key any) grpc.ClientConnInterface {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.channelsByKey[key].pick()
}

type multiChannel func() grpc.ClientConnInterface

func (c multiChannel) Invoke(ctx context.Context, methodName string, req, resp any, opts ...grpc.CallOption) error {
	ch := c()
	if ch == nil {
		return status.Errorf(codes.Unavailable, "no channels ready")
	}
	return ch.Invoke(ctx, methodName, req, resp, opts...)
}

func (c multiChannel) NewStream(ctx context.Context, desc *grpc.StreamDesc, methodName string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	ch := c()
	if ch == nil {
		return nil, status.Errorf(codes.Unavailable, "no channels ready")
	}
	return ch.NewStream(ctx, desc, methodName, opts...)
}

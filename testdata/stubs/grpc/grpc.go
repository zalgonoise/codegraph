// Package grpc is a minimal offline stand-in for google.golang.org/grpc: just
// the API surface the ledger sample and codegraph's gRPC rules look at.
package grpc

import (
	"context"
	"errors"
	"net"
)

type CallOption interface{}

type DialOption interface{}

type MethodDesc struct {
	MethodName string
	Handler    func(srv any, ctx context.Context, dec func(any) error) (any, error)
}

type ServiceDesc struct {
	ServiceName string
	HandlerType any
	Methods     []MethodDesc
}

type ServiceRegistrar interface {
	RegisterService(desc *ServiceDesc, impl any)
}

type ClientConnInterface interface {
	Invoke(ctx context.Context, method string, args, reply any, opts ...CallOption) error
}

type ClientConn struct{ target string }

func NewClient(target string, _ ...DialOption) (*ClientConn, error) { return &ClientConn{target}, nil }

func (cc *ClientConn) Invoke(context.Context, string, any, any, ...CallOption) error {
	return errors.New("grpc stub: " + cc.target)
}

type service struct {
	desc *ServiceDesc
	impl any
}

type Server struct{ services []service }

func NewServer() *Server { return &Server{} }

func (s *Server) RegisterService(desc *ServiceDesc, impl any) {
	s.services = append(s.services, service{desc, impl})
}

// Serve dispatches through the method tables, like the real server does.
func (s *Server) Serve(lis net.Listener) error {
	defer lis.Close()
	for _, svc := range s.services {
		for _, m := range svc.desc.Methods {
			if _, err := m.Handler(svc.impl, context.Background(), func(any) error { return nil }); err != nil {
				return err
			}
		}
	}
	return nil
}

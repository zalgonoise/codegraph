// Package pb stands in for protoc-gen-go-grpc output of ledger.proto and
// notifier.proto, trimmed to what codegraph looks at.
package pb

import (
	"context"
	"errors"

	"google.golang.org/grpc"
)

var errUnimplemented = errors.New("unimplemented")

type CreditRequest struct {
	Account string
	Amount  int64
}

type BalanceRequest struct{ Account string }

type DebitRequest struct {
	Account string
	Amount  int64
}

type BalanceReply struct{ Balance int64 }

// LedgerServiceServer is the server API for LedgerService.
type LedgerServiceServer interface {
	Credit(context.Context, *CreditRequest) (*BalanceReply, error)
	Balance(context.Context, *BalanceRequest) (*BalanceReply, error)
	Debit(context.Context, *DebitRequest) (*BalanceReply, error)
	mustEmbedUnimplementedLedgerServiceServer()
}

// UnimplementedLedgerServiceServer must be embedded for forward compatibility.
type UnimplementedLedgerServiceServer struct{}

func (UnimplementedLedgerServiceServer) Credit(context.Context, *CreditRequest) (*BalanceReply, error) {
	return nil, errUnimplemented
}

func (UnimplementedLedgerServiceServer) Balance(context.Context, *BalanceRequest) (*BalanceReply, error) {
	return nil, errUnimplemented
}

func (UnimplementedLedgerServiceServer) Debit(context.Context, *DebitRequest) (*BalanceReply, error) {
	return nil, errUnimplemented
}

func (UnimplementedLedgerServiceServer) mustEmbedUnimplementedLedgerServiceServer() {}

func RegisterLedgerServiceServer(s grpc.ServiceRegistrar, srv LedgerServiceServer) {
	s.RegisterService(&LedgerService_ServiceDesc, srv)
}

func _LedgerService_Credit_Handler(srv any, ctx context.Context, dec func(any) error) (any, error) {
	in := new(CreditRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	return srv.(LedgerServiceServer).Credit(ctx, in)
}

func _LedgerService_Balance_Handler(srv any, ctx context.Context, dec func(any) error) (any, error) {
	in := new(BalanceRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	return srv.(LedgerServiceServer).Balance(ctx, in)
}

func _LedgerService_Debit_Handler(srv any, ctx context.Context, dec func(any) error) (any, error) {
	in := new(DebitRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	return srv.(LedgerServiceServer).Debit(ctx, in)
}

var LedgerService_ServiceDesc = grpc.ServiceDesc{
	ServiceName: "ledger.LedgerService",
	HandlerType: (*LedgerServiceServer)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: "Credit", Handler: _LedgerService_Credit_Handler},
		{MethodName: "Balance", Handler: _LedgerService_Balance_Handler},
		{MethodName: "Debit", Handler: _LedgerService_Debit_Handler},
	},
}

type NotifyRequest struct{ Account string }

type NotifyReply struct{}

// NotifierServiceClient is the client API for NotifierService.
type NotifierServiceClient interface {
	Notify(ctx context.Context, in *NotifyRequest, opts ...grpc.CallOption) (*NotifyReply, error)
}

type notifierServiceClient struct{ cc grpc.ClientConnInterface }

func NewNotifierServiceClient(cc grpc.ClientConnInterface) NotifierServiceClient {
	return &notifierServiceClient{cc}
}

func (c *notifierServiceClient) Notify(ctx context.Context, in *NotifyRequest, opts ...grpc.CallOption) (*NotifyReply, error) {
	out := new(NotifyReply)
	if err := c.cc.Invoke(ctx, "/notifier.NotifierService/Notify", in, out, opts...); err != nil {
		return nil, err
	}
	return out, nil
}

// LedgerServiceClient is the client API for LedgerService: what its gateway forwards to.
type LedgerServiceClient interface {
	Credit(ctx context.Context, in *CreditRequest, opts ...grpc.CallOption) (*BalanceReply, error)
	Balance(ctx context.Context, in *BalanceRequest, opts ...grpc.CallOption) (*BalanceReply, error)
}

type ledgerServiceClient struct{ cc grpc.ClientConnInterface }

func NewLedgerServiceClient(cc grpc.ClientConnInterface) LedgerServiceClient {
	return &ledgerServiceClient{cc}
}

func (c *ledgerServiceClient) Credit(ctx context.Context, in *CreditRequest, opts ...grpc.CallOption) (*BalanceReply, error) {
	out := new(BalanceReply)
	if err := c.cc.Invoke(ctx, "/ledger.LedgerService/Credit", in, out, opts...); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *ledgerServiceClient) Balance(ctx context.Context, in *BalanceRequest, opts ...grpc.CallOption) (*BalanceReply, error) {
	out := new(BalanceReply)
	if err := c.cc.Invoke(ctx, "/ledger.LedgerService/Balance", in, out, opts...); err != nil {
		return nil, err
	}
	return out, nil
}

// Package pb is the SDK's copy of the generated LedgerService client.
package pb

import (
	"context"

	"google.golang.org/grpc"
)

type BalanceRequest struct{ Account string }

type BalanceReply struct{ Balance int64 }

type LedgerServiceClient interface {
	Balance(ctx context.Context, in *BalanceRequest, opts ...grpc.CallOption) (*BalanceReply, error)
}

type ledgerServiceClient struct{ cc grpc.ClientConnInterface }

func NewLedgerServiceClient(cc grpc.ClientConnInterface) LedgerServiceClient {
	return &ledgerServiceClient{cc}
}

func (c *ledgerServiceClient) Balance(ctx context.Context, in *BalanceRequest, opts ...grpc.CallOption) (*BalanceReply, error) {
	out := new(BalanceReply)
	if err := c.cc.Invoke(ctx, "/ledger.LedgerService/Balance", in, out, opts...); err != nil {
		return nil, err
	}
	return out, nil
}

// LedgerService_ServiceDesc names the proto service, as generated code does.
var LedgerService_ServiceDesc = grpc.ServiceDesc{ServiceName: "ledger.LedgerService", HandlerType: (*LedgerServiceClient)(nil)}

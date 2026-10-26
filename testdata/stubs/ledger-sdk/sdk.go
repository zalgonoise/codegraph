// Package ledgersdk stands in for a service's client library (*-sdk-go): a typed client whose
// methods make the gRPC calls, so the calls live outside the service.
package ledgersdk

import (
	"context"

	"google.golang.org/grpc"

	"example.com/inhouse/ledger-sdk-go/pb"
)

type Client interface {
	Balance(ctx context.Context, account string) (int64, error)
}

type client struct{ ledger pb.LedgerServiceClient }

func NewClient(target string) (Client, error) {
	conn, err := grpc.NewClient(target)
	if err != nil {
		return nil, err
	}
	return &client{ledger: pb.NewLedgerServiceClient(conn)}, nil
}

func (c *client) Balance(ctx context.Context, account string) (int64, error) {
	resp, err := c.ledger.Balance(ctx, &pb.BalanceRequest{Account: account})
	if err != nil {
		return 0, err
	}
	return resp.Balance, nil
}

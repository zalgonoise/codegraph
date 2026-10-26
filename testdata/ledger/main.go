// Command ledger is the gRPC sample service: its server gets one journey per
// implemented RPC method, its RPCs to NotifierService are outbound calls, and
// Credit asks billing for an invoice over Kafka (the in-house kafkabus). A
// grpc-gateway serves Credit and Balance over HTTP, forwarding to its own
// gRPC server.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"

	"example.com/inhouse/kafkabus/v2"
	"example.com/inhouse/kafkabus/v2/data/billing"
	"example.com/inhouse/kvcache"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"

	"github.com/zalgonoise/codegraph/testdata/ledger/internal/pb"
)

func main() {
	conn, err := grpc.NewClient("notifier:9090")
	if err != nil {
		log.Fatal(err)
	}
	events, err := kafkabus.NewPublisher()
	if err != nil {
		log.Fatal(err)
	}
	cache, err := kvcache.NewEngine()
	if err != nil {
		log.Fatal(err)
	}
	srv := grpc.NewServer()
	pb.RegisterLedgerServiceServer(srv, &ledger{
		balances: map[string]int64{},
		notifier: pb.NewNotifierServiceClient(conn),
		events:   events,
		cache:    cache,
	})

	go serveGateway()

	lis, err := net.Listen("tcp", ":9091")
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(srv.Serve(lis))
}

// serveGateway serves the gRPC methods over HTTP through grpc-gateway, which
// calls this service's own gRPC server.
func serveGateway() {
	self, err := grpc.NewClient("localhost:9091")
	if err != nil {
		log.Fatal(err)
	}
	mux := runtime.NewServeMux()
	if err := pb.RegisterLedgerServiceHandlerClient(context.Background(), mux, pb.NewLedgerServiceClient(self)); err != nil {
		log.Fatal(err)
	}
	if err := mux.HandlePath("GET", "/healthz", func(w http.ResponseWriter, _ *http.Request, _ map[string]string) {
		w.WriteHeader(http.StatusOK)
	}); err != nil {
		log.Fatal(err)
	}
	log.Fatal(http.ListenAndServe(":8092", mux))
}

type ledger struct {
	pb.UnimplementedLedgerServiceServer // Debit stays unimplemented: no journey

	mu       sync.Mutex
	balances map[string]int64
	notifier pb.NotifierServiceClient
	events   kafkabus.Publisher
	cache    *kvcache.Engine
}

func (l *ledger) Credit(ctx context.Context, req *pb.CreditRequest) (*pb.BalanceReply, error) {
	if _, err := l.cache.Get(ctx, fmt.Sprintf("billing:invoice:%s", req.Account)); err == nil {
		return nil, fmt.Errorf("account %s has an open invoice", req.Account) // billing's cache
	}
	balance := l.add(req.Account, req.Amount)
	if _, err := l.notifier.Notify(ctx, &pb.NotifyRequest{Account: req.Account}); err != nil {
		return nil, err
	}
	if _, err := l.events.Publish(ctx, billing.DataInvoiceRequested{Account: req.Account, Amount: req.Amount}); err != nil {
		return nil, err
	}
	return &pb.BalanceReply{Balance: balance}, nil
}

func (l *ledger) Balance(_ context.Context, req *pb.BalanceRequest) (*pb.BalanceReply, error) {
	return &pb.BalanceReply{Balance: l.add(req.Account, 0)}, nil
}

func (l *ledger) add(account string, amount int64) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.balances[account] += amount
	return l.balances[account]
}

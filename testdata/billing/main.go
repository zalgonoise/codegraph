// Command billing is the in-house messaging sample: it consumes and produces
// Kafka topics through kafkabus and RabbitMQ queues through rabbitbus, the
// way services built on in-house libraries do (testdata/rules/inhouse.json
// describes both libraries), and keeps its
// invoices with gorm.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"example.com/inhouse/kafkabus/v2"
	"example.com/inhouse/kafkabus/v2/data/billing"
	bustypes "example.com/inhouse/kafkabus/v2/types"
	"example.com/inhouse/kvcache"
	ledgersdk "example.com/inhouse/ledger-sdk-go"
	"example.com/inhouse/rabbitbus/ports"
	"example.com/inhouse/rabbitbus/rabbitmq"
	"example.com/inhouse/rabbitbus/types"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func main() {
	sub, err := kafkabus.NewSubscriber()
	if err != nil {
		log.Fatal(err)
	}
	pub, err := kafkabus.NewPublisher()
	if err != nil {
		log.Fatal(err)
	}
	queues, err := rabbitmq.NewPublisher(rabbitmq.PublisherConfig{Application: "billing"})
	if err != nil {
		log.Fatal(err)
	}
	db, err := gorm.Open(mysql.Open("billing:billing@tcp(mysql:3306)/billing"))
	if err != nil {
		log.Fatal(err)
	}
	ledger, err := ledgersdk.NewClient("ledger:9091")
	if err != nil {
		log.Fatal(err)
	}
	// A broker repository configured with its queue: Publish and Consume with
	// "" use it. It is captured by the closer below and handed over in a
	// config struct, the way services often wire their repositories.
	audit, err := rabbitmq.New(context.Background(), &rabbitmq.Config{PriorityQueue: "billing.audit"})
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = audit.Close(context.Background()) }()
	cache, err := kvcache.NewEngine()
	if err != nil {
		log.Fatal(err)
	}
	h := newHandler(handlerConfig{Events: pub, Queues: queues, Audit: audit, Ledger: ledger, Store: &store{db: db}, Cache: cache})
	mw := &middleware{}

	// Both topics go through the same middleware; each journey must still
	// reach only its own handler.
	sub.Handle(billing.TopicInvoiceRequested, mw.Wrap(h.invoiceRequested))
	sub.Handle(billing.TopicInvoicePaid, mw.Wrap(h.invoicePaid))

	mux := types.NewConsumeMux()
	for _, queue := range queueNames(reminderQueues()) {
		mux.Handle(queue, mw.WrapRabbit(types.HandlerFunc(h.remind)))
	}
	consumer, err := rabbitmq.NewConsumer(rabbitmq.ConsumerConfig{Handler: mux, Queues: reminderQueues()})
	if err != nil {
		log.Fatal(err)
	}
	go func() { _ = consumer.Consume() }()
	go func() { _ = audit.Consume(context.Background(), "", h.audited) }()

	// A consumer with a handler of its own and no mux: the queue is only in
	// its config.
	refundsQueue := "billing.refunds"
	refunds, err := rabbitmq.NewConsumer(rabbitmq.ConsumerConfig{
		Handler: &refundHandler{store: h.store},
		Queues:  []rabbitmq.QueueConfig{{PriorityQueue: refundsQueue, RetryQueue: refundsQueue + ".retry"}},
	})
	if err != nil {
		log.Fatal(err)
	}
	go func() { _ = refunds.Consume() }()

	log.Fatal(sub.Consume(context.Background()))
}

// Queue names are built the way rabbitbus users usually do: a QueueConfig per
// queue from a helper, and the names read back from the configs.
func reminderQueues() []rabbitmq.QueueConfig {
	return []rabbitmq.QueueConfig{queueConfig("billing.reminders"), queueConfig("billing.reminders.urgent")}
}

func queueConfig(queue string) rabbitmq.QueueConfig {
	return rabbitmq.QueueConfig{PriorityQueue: queue, RetryQueue: queue + ".retry", DLQQueue: queue + ".dlq"}
}

func queueNames(configs []rabbitmq.QueueConfig) []string {
	names := make([]string, 0, len(configs))
	for _, c := range configs {
		names = append(names, c.PriorityQueue)
	}
	return names
}

// receiptsQueue is named after a kafkabus message's topic.
var receiptsQueue = (&billing.DataReceipt{}).Topic().String()

type handlerConfig struct {
	Events kafkabus.Publisher
	Queues ports.Publisher
	Audit  ports.BrokerRepository
	Ledger ledgersdk.Client
	Store  *store
	Cache  *kvcache.Engine
}

// InvoiceKey is the Redis key billing caches invoices under; ledger reads it.
const InvoiceKey = "billing:invoice:%s"

type handler struct {
	events kafkabus.Publisher
	queues ports.Publisher
	audit  ports.BrokerRepository
	ledger ledgersdk.Client
	store  *store
	cache  *kvcache.Engine
}

func newHandler(c handlerConfig) *handler {
	return &handler{events: c.Events, queues: c.Queues, audit: c.Audit, ledger: c.Ledger, store: c.Store, cache: c.Cache}
}

func (h *handler) audited(_ context.Context, body []byte, queue, _ string) error {
	log.Printf("audit %s: %d bytes", queue, len(body))
	return nil
}

func (h *handler) invoiceRequested(ctx context.Context, m kafkabus.Message) error {
	if err := h.store.save(ctx, &invoice{ID: "inv-1"}); err != nil {
		return err
	}
	if err := h.cache.Set(ctx, fmt.Sprintf(InvoiceKey, "acc-1"), []byte("inv-1"), 0, false); err != nil {
		return err
	}
	return h.emit(ctx, &billing.DataInvoiceIssued{InvoiceID: "inv-1"})
}

// emit is the shared publish helper both handlers use; each journey must get
// only the topic its own handler emits.
func (h *handler) emit(ctx context.Context, d bustypes.Data) error {
	_, err := h.events.Publish(ctx, d)
	return err
}

func (h *handler) invoicePaid(ctx context.Context, m kafkabus.Message) error {
	if err := h.store.markPaid(ctx, "inv-1"); err != nil {
		return err
	}
	if _, err := h.store.receipts(ctx, "inv-1"); err != nil {
		return err
	}
	if err := h.emit(ctx, billing.DataPaymentRecorded{InvoiceID: "inv-1"}); err != nil {
		return err
	}
	if _, err := h.ledger.Balance(ctx, "acc-1"); err != nil { // through the SDK
		return err
	}
	if err := h.audit.Publish(ctx, "", []byte("paid"), 1); err != nil { // the configured queue
		return err
	}
	body, err := json.Marshal(m.Data)
	if err != nil {
		return err
	}
	return h.queues.Publish(ctx, receiptsQueue, body, 1)
}

// invoice names its table, as gorm models usually do.
type invoice struct {
	ID   string
	Paid bool
}

func (invoice) TableName() string { return "invoices" }

// receipt has no TableName: its queries name the table with Table().
type receipt struct{ InvoiceID string }

type store struct{ db *gorm.DB }

func (s *store) save(ctx context.Context, inv *invoice) error {
	return s.db.WithContext(ctx).Create(inv).Error
}

func (s *store) markPaid(ctx context.Context, id string) error {
	return s.db.WithContext(ctx).Model(&invoice{}).Where("id = ?", id).Updates(map[string]any{"paid": true}).Error
}

func (s *store) receipts(ctx context.Context, id string) ([]receipt, error) {
	var rs []receipt
	err := s.db.WithContext(ctx).Table("receipts AS r").Where("r.invoice_id = ?", id).Find(&rs).Error
	return rs, err
}

func (h *handler) remind(_ context.Context, m *types.Message) error {
	log.Printf("reminder from %s", m.Queue)
	return nil
}

// refundHandler is a rabbitbus handler registered directly on a consumer.
type refundHandler struct{ store *store }

func (r *refundHandler) ProcessMessage(ctx context.Context, m *types.Message) error {
	id := string(m.Body)
	if err := r.store.refund(ctx, id); err != nil {
		return err
	}
	return refundPayment(ctx, id)
}

// refundPayment calls the gateway's iris admin API, whose route has a typed
// parameter ({payment_id:string uuid() else 400}).
func refundPayment(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://gateway:8091/backoffice/payments/"+id+"/refund", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (s *store) refund(ctx context.Context, id string) error {
	return s.db.WithContext(ctx).Model(&invoice{}).Where("id = ?", id).Updates(map[string]any{"paid": false}).Error
}

// middleware wraps consumers, as a shared consumer middleware does.
type middleware struct{}

func (mw *middleware) Wrap(next kafkabus.Handler) kafkabus.Handler {
	return func(ctx context.Context, msg kafkabus.Message) error {
		log.Printf("consume offset %d", msg.Offset)
		return next(ctx, msg)
	}
}

func (mw *middleware) WrapRabbit(next types.Handler) types.Handler {
	return types.HandlerFunc(func(ctx context.Context, msg *types.Message) error {
		log.Printf("consume %s", msg.Queue)
		return next.ProcessMessage(ctx, msg)
	})
}

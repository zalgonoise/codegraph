// Package orders holds the domain logic and the contracts it depends on.
package orders

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/zalgonoise/codegraph/testdata/orders/internal/events"
)

const TopicOrderCreated = "orders.created"

type Order struct {
	ID     string `json:"id"`
	SKU    string `json:"sku"`
	Qty    int    `json:"qty"`
	Status string `json:"status"`
}

type OrderCreated struct {
	OrderID string `json:"orderId"`
	SKU     string `json:"sku"`
	Qty     int    `json:"qty"`
}

// Repository persists orders.
type Repository interface {
	Get(ctx context.Context, id string) (Order, error)
	Save(ctx context.Context, o Order) error
}

// Inventory reserves stock.
type Inventory interface {
	Reserve(ctx context.Context, sku string, qty int) error
}

// Mock is never wired at runtime; it must not show up as an implementation.
type Mock struct{}

func (Mock) Reserve(context.Context, string, int) error { return nil }

type Service struct {
	repo Repository
	inv  Inventory
	pub  events.Publisher
}

func NewService(repo Repository, inv Inventory, pub events.Publisher) *Service {
	return &Service{repo: repo, inv: inv, pub: pub}
}

func (s *Service) Create(ctx context.Context, sku string, qty int) (Order, error) {
	if err := s.inv.Reserve(ctx, sku, qty); err != nil {
		return Order{}, fmt.Errorf("reserve: %w", err)
	}
	o := Order{ID: strconv.FormatInt(time.Now().UnixNano(), 36), SKU: sku, Qty: qty, Status: "created"}
	if err := s.repo.Save(ctx, o); err != nil {
		return Order{}, err
	}
	if err := s.pub.Publish(ctx, TopicOrderCreated, OrderCreated{OrderID: o.ID, SKU: sku, Qty: qty}); err != nil {
		return Order{}, err
	}
	return o, nil
}

func (s *Service) Get(ctx context.Context, id string) (Order, error) {
	return s.repo.Get(ctx, id)
}

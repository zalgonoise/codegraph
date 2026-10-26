// Package storage implements orders.Repository on database/sql.
package storage

import (
	"context"
	"database/sql"

	"github.com/zalgonoise/codegraph/testdata/orders/internal/domain/orders"
)

type OrderRepo struct {
	db *sql.DB
}

func NewOrderRepo(db *sql.DB) *OrderRepo { return &OrderRepo{db: db} }

func (r *OrderRepo) Get(ctx context.Context, id string) (orders.Order, error) {
	var o orders.Order
	err := r.db.QueryRowContext(ctx, `SELECT id, sku, qty, status FROM orders WHERE id = $1`, id).
		Scan(&o.ID, &o.SKU, &o.Qty, &o.Status)
	return o, err
}

func (r *OrderRepo) Save(ctx context.Context, o orders.Order) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO orders (id, sku, qty, status) VALUES ($1, $2, $3, $4)`,
		o.ID, o.SKU, o.Qty, o.Status)
	return err
}

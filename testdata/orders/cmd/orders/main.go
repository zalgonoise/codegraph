// Command orders is a sample service used to exercise the codegraph PoC.
//
//codegraph:alias orders-svc
//codegraph:tag team=checkout
package main

import (
	"database/sql"
	"log"
	"net/http"

	"github.com/zalgonoise/codegraph/testdata/orders/internal/clients/inventory"
	"github.com/zalgonoise/codegraph/testdata/orders/internal/config"
	"github.com/zalgonoise/codegraph/testdata/orders/internal/domain/orders"
	"github.com/zalgonoise/codegraph/testdata/orders/internal/events"
	"github.com/zalgonoise/codegraph/testdata/orders/internal/httpapi"
	"github.com/zalgonoise/codegraph/testdata/orders/internal/storage"
)

func main() {
	cfg := config.Load()

	db, err := sql.Open("postgres", cfg.DSN)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	svc := orders.NewService(
		storage.NewOrderRepo(db),
		inventory.NewClient(cfg.InventoryURL),
		events.NewLogPublisher(),
	)

	mux := http.NewServeMux()
	httpapi.NewHandler(svc).Routes(mux)

	log.Printf("orders listening on %s", cfg.Addr)
	log.Fatal(http.ListenAndServe(cfg.Addr, mux))
}

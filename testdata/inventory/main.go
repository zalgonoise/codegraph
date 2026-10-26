// Command inventory is the second sample service.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
)

func main() {
	store := &memStore{items: map[string]int{"sku-1": 10}}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /items/{sku}", itemHandler(store))
	mux.HandleFunc("GET /stock", logged(store.stock))
	mux.HandleFunc("GET /health", logged(health))
	mux.HandleFunc("GET "+cfg.BasePath+"/version", version) // the base path is its envDefault

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go consumeOrders(ctx, store)
	go serveMetrics()

	srv := &http.Server{Addr: ":8081", Handler: mux}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

type memStore struct {
	mu    sync.Mutex
	items map[string]int
}

func (s *memStore) available(sku string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.items[sku]
}

func itemHandler(s *memStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sku := r.PathValue("sku")
		_ = json.NewEncoder(w).Encode(map[string]any{"sku": sku, "available": s.available(sku)})
	}
}

func (s *memStore) stock(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = json.NewEncoder(w).Encode(s.items)
}

func health(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }

func version(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("1")) }

// serveMetrics listens on a configured address whose envDefault names the
// local machine, a placeholder: it stays unknown.
func serveMetrics() { log.Println(http.ListenAndServe(cfg.MetricsAddr, nil)) }

// logged wraps both /stock and /health: each journey must reach only the
// handler wrapped at its own registration, not every handler logged wraps.
func logged(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next(w, r)
	}
}

// consumeOrders would subscribe to the broker in a real deployment; the topic
// comes from configuration, so it is declared for codegraph explicitly.
//
//codegraph:consumes kafka orders.created msg=orders.OrderCreated
func consumeOrders(ctx context.Context, s *memStore) {
	<-ctx.Done()
	_ = s
}

// config is read from the environment the way caarlos0/env reads tagged
// structs; the sample keeps to the standard library, so parseEnv stands in
// for env.Parse.
type config struct {
	Addr        string      `env:"INVENTORY_ADDR,required"`
	BasePath    string      `env:"INVENTORY_BASE_PATH" envDefault:"/v1"`
	MetricsAddr string      `env:"INVENTORY_METRICS_ADDR" envDefault:":9090"`
	Kafka       kafkaConfig `envPrefix:"KAFKA_"`
	Region      string
	Skip        string `env:"-"`
}

type kafkaConfig struct {
	Brokers string `env:"BROKERS"`
}

var cfg = loadConfig()

func loadConfig() config {
	var c config
	parseEnv(&c)
	c.Region = os.Getenv("INVENTORY_REGION")
	return c
}

func parseEnv(v any) { _ = v }

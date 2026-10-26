// Command pipeline is the main journey sample: a program whose work no
// endpoint starts. main pulls readings from a feed over HTTP and stores them
// from a goroutine. It also serves a health route, a journey of its own where
// main's stops, and metrics through a library handler, which has none.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

type reading struct {
	Sensor string
	Value  float64
}

func main() {
	db, err := sql.Open("postgres", "postgres://pipeline")
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", health(db))
	mux.Handle("GET /metrics", http.NotFoundHandler())
	go func() {
		log.Print(http.ListenAndServe(":9100", mux))
	}()

	ctx := context.Background()
	readings := make(chan reading)
	go store(ctx, db, readings)
	pull(ctx, readings)
}

// health checks the database: its own journey, which main's does not enter.
func health(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := db.QueryContext(r.Context(), "SELECT 1 FROM checkpoints"); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}
}

// pull reads the feed forever.
func pull(ctx context.Context, out chan<- reading) {
	for ctx.Err() == nil {
		resp, err := http.Get("http://feed:8080/readings")
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		var rs []reading
		_ = json.NewDecoder(resp.Body).Decode(&rs)
		_ = resp.Body.Close()
		for _, r := range rs {
			out <- r
		}
	}
}

// store writes what pull reads, in a goroutine main starts.
func store(ctx context.Context, db *sql.DB, in <-chan reading) {
	for r := range in {
		if _, err := db.ExecContext(ctx, "INSERT INTO readings (sensor, value) VALUES ($1, $2)", r.Sensor, r.Value); err != nil {
			log.Print(err)
		}
	}
}

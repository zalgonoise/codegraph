// Command api takes payouts: POST /payouts stores one as pending, GET
// /payouts/{id} reads its state. Sending them is not its work: the send job
// runs on a schedule and picks up what is pending, through the table.
package main

import (
	"database/sql"
	"io"
	"log"
	"net/http"
)

func main() {
	db, err := sql.Open("mysql", "payouts")
	if err != nil {
		log.Fatal(err)
	}
	http.HandleFunc("POST /payouts", create(db))
	http.HandleFunc("GET /payouts/{id}", get(db))
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func create(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if _, err := db.ExecContext(r.Context(), "INSERT INTO payouts (id, state) VALUES (?, 'pending')", string(body)); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}
}

func get(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var state string
		if err := db.QueryRowContext(r.Context(), "SELECT state FROM payouts WHERE id = ?", r.PathValue("id")).Scan(&state); err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, state)
	}
}

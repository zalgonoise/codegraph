// Command send is a job: each run sends the pending payouts to the bank and
// marks them sent.
package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"strings"
)

func main() {
	ctx := context.Background()
	db, err := sql.Open("mysql", "payouts")
	if err != nil {
		log.Fatal(err)
	}
	rows, err := db.QueryContext(ctx, "SELECT id FROM payouts WHERE state = 'pending'")
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			log.Fatal(err)
		}
		if err := send(ctx, id); err != nil {
			log.Print(err)
			continue
		}
		if _, err := db.ExecContext(ctx, "UPDATE payouts SET state = 'sent' WHERE id = ?", id); err != nil {
			log.Print(err)
		}
	}
}

// send asks the bank to transfer a payout.
func send(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://bank:8443/transfers", strings.NewReader(id))
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

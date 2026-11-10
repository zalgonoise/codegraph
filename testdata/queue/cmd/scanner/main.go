// Command scanner is the beanstalkd sample's front. POST /scans puts a scan on
// the scans tube, or on scans-priority, through kr/beanstalk tubes; the
// results come back on scan-results, watched through an in-house client that
// adapts its own handlers to the workers mux.
package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"time"

	"example.com/inhouse/tubequeue"
	"github.com/kr/beanstalk"
)

func main() {
	conn, err := beanstalk.Dial("tcp", "beanstalkd:11300")
	if err != nil {
		log.Fatal(err)
	}
	scans := &beanstalk.Tube{Conn: conn, Name: "scans"}
	priority := &beanstalk.Tube{Conn: conn, Name: "scans-priority"}

	queue := tubequeue.New("beanstalkd:11300")
	if err := queue.WatchTube(context.Background(), "scan-results", tubequeue.AdaptHandler(&results{})); err != nil {
		log.Fatal(err)
	}
	go func() { log.Print(queue.Start()) }()

	http.HandleFunc("POST /scans", queueScan(scans, priority))
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// queueScan puts the request's document on a tube.
func queueScan(scans, priority *beanstalk.Tube) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Query().Get("priority") != "" {
			_, _ = priority.Put(body, 0, 0, time.Minute)
			return
		}
		_, _ = scans.Put(body, 1, 0, time.Minute)
	}
}

// results logs each scan's result: a tubequeue.Handler.
type results struct{}

func (*results) Work(id uint64, body []byte) (bool, error) {
	log.Printf("scan %d: %s", id, body)
	return true, nil
}

// Command worker scans what the scanner queues: the scans tube through a
// workers mux, scans-priority from a tube set it reserves from in a loop of its
// own. Each result goes on scan-results through the in-house client.
package main

import (
	"context"
	"log"
	"time"

	"example.com/inhouse/tubequeue"
	"github.com/kr/beanstalk"
	"github.com/mikegleasonjr/workers"
)

func main() {
	ctx := context.Background()
	conn, err := beanstalk.Dial("tcp", "beanstalkd:11300")
	if err != nil {
		log.Fatal(err)
	}
	queue := tubequeue.New("beanstalkd:11300")

	go reservePriority(ctx, conn, queue)

	mux := workers.NewWorkMux()
	mux.Handle("scans", workers.HandlerFunc(func(j *workers.Job) {
		scan(ctx, queue, j.Body)
	}))
	log.Fatal(workers.ConnectAndWork("tcp", "beanstalkd:11300", mux))
}

// reservePriority scans priority documents as they arrive.
func reservePriority(ctx context.Context, conn *beanstalk.Conn, queue tubequeue.Client) {
	ts := beanstalk.NewTubeSet(conn, "scans-priority")
	for ctx.Err() == nil {
		id, body, err := ts.Reserve(time.Minute)
		if err != nil {
			continue
		}
		scan(ctx, queue, body)
		_ = conn.Delete(id)
	}
}

// scan puts a document's result on scan-results.
func scan(ctx context.Context, queue tubequeue.Client, body []byte) {
	if err := queue.Put(ctx, "scan-results", body); err != nil {
		log.Print(err)
	}
}

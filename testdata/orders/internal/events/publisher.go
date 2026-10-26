// Package events is an in-house publishing abstraction (a Kafka wrapper in a
// real system). codegraph learns about it from testdata/orders/codegraph-rules.json.
package events

import (
	"context"
	"encoding/json"
	"log"
)

type Publisher interface {
	Publish(ctx context.Context, topic string, msg any) error
}

type LogPublisher struct{}

func NewLogPublisher() *LogPublisher { return &LogPublisher{} }

func (p *LogPublisher) Publish(ctx context.Context, topic string, msg any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	log.Printf("publish %s: %s", topic, b)
	return nil
}

// Package kafkabus is a minimal offline stand-in for an in-house Kafka
// library: just the API surface the samples and testdata/rules/inhouse.json
// use.
package kafkabus

import (
	"context"

	"example.com/inhouse/kafkabus/v2/types"
)

type Message struct {
	Data   types.Data
	Offset int64
}

type Handler func(ctx context.Context, m Message) error

type ConsumerOpts func()

type Options interface{}

type PublishOption func(options Options)

type Subscriber interface {
	Close(ctx context.Context) error
	Consume(ctx context.Context) error
	Handle(t types.Topic, h Handler, opts ...ConsumerOpts)
}

type Publisher interface {
	Close(ctx context.Context) error
	Publish(ctx context.Context, d types.Data, opts ...PublishOption) (int64, error)
}

func NewSubscriber() (Subscriber, error) {
	return &memSubscriber{handlers: map[types.Topic]Handler{}}, nil
}

func NewPublisher() (Publisher, error) { return &memPublisher{}, nil }

type memSubscriber struct{ handlers map[types.Topic]Handler }

func (s *memSubscriber) Close(context.Context) error { return nil }

// Consume dispatches to the registered handlers, like the real subscriber.
func (s *memSubscriber) Consume(ctx context.Context) error {
	for _, h := range s.handlers {
		if err := h(ctx, Message{}); err != nil {
			return err
		}
	}
	return nil
}

func (s *memSubscriber) Handle(t types.Topic, h Handler, _ ...ConsumerOpts) { s.handlers[t] = h }

type memPublisher struct{ offset int64 }

func (p *memPublisher) Close(context.Context) error { return nil }

func (p *memPublisher) Publish(_ context.Context, d types.Data, _ ...PublishOption) (int64, error) {
	if err := d.Validate(); err != nil {
		return 0, err
	}
	p.offset++
	return p.offset, nil
}

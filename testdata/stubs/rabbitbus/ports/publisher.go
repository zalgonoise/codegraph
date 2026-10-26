package ports

import "context"

type PublishOptions struct{ Headers map[string]any }

type PublishOption interface {
	Apply(opts *PublishOptions) error
}

// BrokerRepository is the lower-level client: Publish and Consume with ""
// use the queue it was configured with.
type BrokerRepository interface {
	Close(ctx context.Context) error
	Consume(ctx context.Context, queueName string, handler func(context.Context, []byte, string, string) error) error
	Publish(ctx context.Context, queueName string, body []byte, priority int, opts ...PublishOption) error
}

type Publisher interface {
	Close() error
	Publish(ctx context.Context, queue string, body []byte, priority int, opts ...PublishOption) error
}

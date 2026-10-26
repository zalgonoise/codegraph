package rabbitmq

import (
	"context"

	"example.com/inhouse/rabbitbus/ports"
	"example.com/inhouse/rabbitbus/types"
)

type QueueConfig struct {
	PriorityQueue string
	RetryQueue    string
	DLQQueue      string
}

type ConsumerConfig struct {
	Handler types.Handler
	Queues  []QueueConfig
}

type PublisherConfig struct {
	Application string
	Queues      []QueueConfig
}

// Config configures a broker repository (RabbitMQ).
type Config struct {
	PriorityQueue string
	RetryQueue    string
	DLQQueue      string
}

type RabbitMQ struct{ priorityQueue string }

var _ ports.BrokerRepository = (*RabbitMQ)(nil)

func New(_ context.Context, c *Config) (*RabbitMQ, error) {
	return &RabbitMQ{priorityQueue: c.PriorityQueue}, nil
}

func (r *RabbitMQ) Close(context.Context) error { return nil }

// Consume delivers to handler, like the real repository ("" = the configured queue).
func (r *RabbitMQ) Consume(ctx context.Context, queueName string, handler func(context.Context, []byte, string, string) error) error {
	if queueName == "" {
		queueName = r.priorityQueue
	}
	return handler(ctx, nil, queueName, "")
}

func (r *RabbitMQ) Publish(_ context.Context, queueName string, _ []byte, _ int, _ ...ports.PublishOption) error {
	if queueName == "" {
		queueName = r.priorityQueue
	}
	_ = queueName
	return nil
}

type Consumer struct{ cfg ConsumerConfig }

func NewConsumer(c ConsumerConfig) (*Consumer, error) { return &Consumer{cfg: c}, nil }

// Consume hands every queue's messages to the configured handler, like the
// real consumer.
func (c *Consumer) Consume() error {
	for _, q := range c.cfg.Queues {
		if err := c.cfg.Handler.ProcessMessage(context.Background(), &types.Message{Queue: q.PriorityQueue}); err != nil {
			return err
		}
	}
	return nil
}

func (c *Consumer) Close() error { return nil }

type Publisher struct{ sent int }

var _ ports.Publisher = (*Publisher)(nil)

func NewPublisher(PublisherConfig) (*Publisher, error) { return &Publisher{}, nil }

func (p *Publisher) Close() error { return nil }

func (p *Publisher) Publish(context.Context, string, []byte, int, ...ports.PublishOption) error {
	p.sent++
	return nil
}

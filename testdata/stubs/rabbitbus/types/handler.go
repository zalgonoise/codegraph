// Package types is a minimal offline stand-in for an in-house RabbitMQ
// library's types: messages, handlers and the queue mux.
package types

import "context"

type Message struct {
	Queue    string
	Body     []byte
	Priority int
}

type Handler interface {
	ProcessMessage(ctx context.Context, m *Message) error
}

type HandlerFunc func(ctx context.Context, m *Message) error

func (f HandlerFunc) ProcessMessage(ctx context.Context, m *Message) error { return f(ctx, m) }

type ConsumeMux struct{ registry map[string]Handler }

func NewConsumeMux() *ConsumeMux { return &ConsumeMux{registry: map[string]Handler{}} }

func (mux *ConsumeMux) Handle(queue string, handler Handler) { mux.registry[queue] = handler }

func (mux *ConsumeMux) HandleFunc(queue string, handler func(ctx context.Context, m *Message) error) {
	mux.Handle(queue, HandlerFunc(handler))
}

func (mux *ConsumeMux) ProcessMessage(ctx context.Context, m *Message) error {
	if h, ok := mux.registry[m.Queue]; ok {
		return h.ProcessMessage(ctx, m)
	}
	return nil
}

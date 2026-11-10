// Package tubequeue is a minimal offline stand-in for an in-house beanstalkd
// client: tubes are named per call, and handlers written against its own
// Handler interface are adapted to the workers mux it runs.
package tubequeue

import (
	"context"

	"github.com/mikegleasonjr/workers"
)

type Client interface {
	Put(ctx context.Context, tube string, body []byte) error
	WatchTube(ctx context.Context, tube string, handler workers.Handler) error
	Start() error
}

// Handler is the client's own handler: true deletes the job.
type Handler interface {
	Work(id uint64, body []byte) (bool, error)
}

// AdaptHandler runs a Handler as a workers.Handler.
func AdaptHandler(h Handler) workers.Handler { return &adapter{h: h} }

type adapter struct{ h Handler }

func (a *adapter) Work(j *workers.Job) { _, _ = a.h.Work(j.ID, j.Body) }

func New(addr string) Client { return &client{mux: workers.NewWorkMux(), addr: addr} }

type client struct {
	mux  *workers.WorkMux
	addr string
}

func (c *client) Put(ctx context.Context, tube string, body []byte) error { return nil }

func (c *client) WatchTube(ctx context.Context, tube string, handler workers.Handler) error {
	c.mux.Handle(tube, handler)
	return nil
}

func (c *client) Start() error { return workers.ConnectAndWork("tcp", c.addr, c.mux) }

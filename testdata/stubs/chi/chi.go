// Package chi is a minimal offline stand-in for github.com/go-chi/chi/v5: the
// routing and scoping methods codegraph's chi rules look at.
package chi

import "net/http"

type Middleware = func(http.Handler) http.Handler

type Router interface {
	http.Handler
	Use(middlewares ...Middleware)
	With(middlewares ...Middleware) Router
	Group(fn func(r Router)) Router
	Route(pattern string, fn func(r Router)) Router
	Mount(pattern string, h http.Handler)
	Get(pattern string, h http.HandlerFunc)
	Post(pattern string, h http.HandlerFunc)
}

type Mux struct {
	handlers []http.HandlerFunc
	mounts   []http.Handler
}

func NewRouter() *Mux { return &Mux{} }

// ServeHTTP dispatches to the registered handlers, like the real router.
func (mx *Mux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	for _, h := range mx.handlers {
		h(w, r)
	}
	for _, m := range mx.mounts {
		m.ServeHTTP(w, r)
	}
}

func (mx *Mux) Use(...Middleware) {}

func (mx *Mux) With(...Middleware) Router { return mx }

func (mx *Mux) Group(fn func(r Router)) Router {
	fn(mx)
	return mx
}

func (mx *Mux) Route(pattern string, fn func(r Router)) Router {
	sub := NewRouter()
	fn(sub)
	mx.Mount(pattern, sub)
	return sub
}

func (mx *Mux) Mount(_ string, h http.Handler) { mx.mounts = append(mx.mounts, h) }

func (mx *Mux) Get(_ string, h http.HandlerFunc) { mx.handlers = append(mx.handlers, h) }

func (mx *Mux) Post(_ string, h http.HandlerFunc) { mx.handlers = append(mx.handlers, h) }

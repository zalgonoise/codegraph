// Package router is the stand-in for iris' route registration: the Party
// interface and its implementation, APIBuilder.
package router

import (
	"net/http"

	"github.com/kataras/iris/v12/context"
)

type Party interface {
	Party(relativePath string, middleware ...context.Handler) Party
	Use(middleware ...context.Handler)
	Get(relativePath string, handlers ...context.Handler) *Route
	Post(relativePath string, handlers ...context.Handler) *Route
	Put(relativePath string, handlers ...context.Handler) *Route
	Delete(relativePath string, handlers ...context.Handler) *Route
}

type Route struct{ Method, Path string }

// APIBuilder registers routes. A party shares its parent's handlers, so the
// application dispatches to every route.
type APIBuilder struct {
	handlers *[]context.Handler
}

func NewAPIBuilder() *APIBuilder { return &APIBuilder{handlers: new([]context.Handler)} }

func (api *APIBuilder) Party(_ string, middleware ...context.Handler) Party {
	*api.handlers = append(*api.handlers, middleware...)
	return &APIBuilder{handlers: api.handlers}
}

func (api *APIBuilder) Use(middleware ...context.Handler) {
	*api.handlers = append(*api.handlers, middleware...)
}

func (api *APIBuilder) Handle(method, relativePath string, handlers ...context.Handler) *Route {
	*api.handlers = append(*api.handlers, handlers...)
	return &Route{Method: method, Path: relativePath}
}

func (api *APIBuilder) Get(relativePath string, handlers ...context.Handler) *Route {
	return api.Handle(http.MethodGet, relativePath, handlers...)
}

func (api *APIBuilder) Post(relativePath string, handlers ...context.Handler) *Route {
	return api.Handle(http.MethodPost, relativePath, handlers...)
}

func (api *APIBuilder) Put(relativePath string, handlers ...context.Handler) *Route {
	return api.Handle(http.MethodPut, relativePath, handlers...)
}

func (api *APIBuilder) Delete(relativePath string, handlers ...context.Handler) *Route {
	return api.Handle(http.MethodDelete, relativePath, handlers...)
}

// Serve runs the registered handlers, like the real router.
func (api *APIBuilder) Serve(ctx *context.Context) {
	for _, h := range *api.handlers {
		h(ctx)
	}
}

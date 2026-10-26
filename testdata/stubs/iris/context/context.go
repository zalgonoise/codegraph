// Package context is the stand-in for iris' request context and handler type.
package context

import "net/http"

type Context struct {
	ResponseWriter http.ResponseWriter
	Request        *http.Request
}

func (ctx *Context) StatusCode(code int) { ctx.ResponseWriter.WriteHeader(code) }

func (ctx *Context) Params(name string) string { return ctx.Request.PathValue(name) }

type Handler func(*Context)

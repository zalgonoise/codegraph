// Package iris is a minimal offline stand-in for github.com/kataras/iris/v12:
// the application, whose routing methods are promoted from the embedded
// router.APIBuilder as in the real library, and Listen.
package iris

import (
	"net/http"

	"github.com/kataras/iris/v12/context"
	"github.com/kataras/iris/v12/core/router"
)

type (
	Context = *context.Context
	Handler = context.Handler
	Party   = router.Party
)

type Application struct {
	*router.APIBuilder
}

func New() *Application { return &Application{APIBuilder: router.NewAPIBuilder()} }

// ServeHTTP dispatches to the registered handlers, like the real router.
func (app *Application) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	app.APIBuilder.Serve(&context.Context{ResponseWriter: w, Request: r})
}

func (app *Application) Listen(hostPort string) error { return http.ListenAndServe(hostPort, app) }

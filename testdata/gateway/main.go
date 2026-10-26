// Command gateway is the router-prefix sample: routes registered under chi
// Route, Group and Mount (of a router a constructor returns) get their full
// path, and an oapi-codegen style options struct whose BaseURL is never set
// contributes "". Routers captured by Group closures and mounted afterwards
// get their mount prefix too. Its admin API runs on iris: routes under nested
// Party prefixes, with typed parameters.
package main

import (
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/kataras/iris/v12"
)

func main() {
	r := chi.NewRouter()
	r.Get("/health", ok)
	r.Route("/v1", func(r chi.Router) {
		r.Get("/balances/{account}", ok)
		r.Group(func(r chi.Router) {
			r.Post("/credits", ok)
		})
	})
	r.Mount("/admin", adminRouter())
	register(Options{Router: r})
	customers(r)
	go adminAPI().Listen(":8091")
	log.Fatal(http.ListenAndServe(":8090", r))
}

func adminRouter() chi.Router {
	r := chi.NewRouter()
	r.Get("/stats", ok)
	return r
}

// Options mirrors oapi-codegen's ChiServerOptions.
type Options struct {
	BaseURL string
	Router  chi.Router
}

func register(o Options) {
	o.Router.Group(func(r chi.Router) {
		r.Get(o.BaseURL+"/version", ok)
	})
}

// customers registers routers the way some services do: each inside a Group
// closure that captures it, and mounted after the closure.
func customers(root *chi.Mux) {
	storefront := chi.NewRouter()
	root.Group(func(chi.Router) {
		storefront.Get("/init", ok)
	})
	root.Mount("/api/storefront", storefront)

	users := chi.NewRouter()
	root.Group(func(chi.Router) {
		users.Get("/", ok)
		documents(users)
	})
	root.Mount("/users/{user_id}", users)
}

// documents mounts its router on the one it is given: a router captured by
// the caller's closure.
func documents(users *chi.Mux) {
	docs := chi.NewRouter()
	docs.Group(func(chi.Router) {
		docs.Post("/", ok)
	})
	users.Mount("/documents", docs)
}

func ok(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

func adminAPI() *iris.Application {
	app := iris.New()
	app.Get("/ping", pong)
	admin := app.Party("/backoffice", auth)
	payments := admin.Party("/payments/{payment_id:string uuid() else 400}")
	h := &adminHandler{}
	payments.Get("/", h.payment)              // iris drops the trailing slash
	payments.Post("/refund", audit, h.refund) // middleware first, the handler last
	admin.Get("/files/{name:path}", h.file)
	return app
}

type adminHandler struct{}

func (h *adminHandler) payment(ctx iris.Context) { ctx.StatusCode(http.StatusOK) }

func (h *adminHandler) refund(ctx iris.Context) {
	log.Printf("refund %s", ctx.Params("payment_id"))
	ctx.StatusCode(http.StatusNoContent)
}

func (h *adminHandler) file(ctx iris.Context) { ctx.StatusCode(http.StatusOK) }

func pong(ctx iris.Context) { ctx.StatusCode(http.StatusOK) }

func auth(ctx iris.Context) {}

func audit(ctx iris.Context) { log.Printf("admin %s", ctx.Request.URL.Path) }

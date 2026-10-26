// Package httpapi exposes the orders service over HTTP.
package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/zalgonoise/codegraph/testdata/orders/internal/domain/orders"
)

type Handler struct {
	svc *orders.Service
}

func NewHandler(svc *orders.Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /orders", h.create)
	mux.HandleFunc("GET /orders/{id}", h.get)
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SKU string `json:"sku"`
		Qty int    `json:"qty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	o, err := h.svc.Create(r.Context(), req.SKU, req.Qty)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(o)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	o, err := h.svc.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(o)
}

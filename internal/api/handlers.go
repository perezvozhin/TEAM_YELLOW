package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/hackathon/inventory-order-management/internal/domain"
	"github.com/hackathon/inventory-order-management/internal/store"
)

type API struct {
	store  *store.Store
	logger *slog.Logger
}

func New(s *store.Store, logger *slog.Logger) *API {
	return &API{store: s, logger: logger}
}

func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", a.health)
	mux.HandleFunc("GET /products", a.listProducts)
	mux.HandleFunc("POST /products", a.createProduct)
	mux.HandleFunc("POST /orders", a.createOrder)
	mux.HandleFunc("GET /orders/{id}", a.getOrder)
	mux.HandleFunc("POST /orders/{id}/cancel", a.cancelOrder)
	return a.requestLog(mux)
}

func (a *API) requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.logger.InfoContext(r.Context(), "request", "method", r.Method, "path", r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) listProducts(w http.ResponseWriter, r *http.Request) {
	products, err := a.store.ListProducts(r.Context())
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, products)
}

func (a *API) createProduct(w http.ResponseWriter, r *http.Request) {
	var input struct {
		SKU   string `json:"sku"`
		Name  string `json:"name"`
		Price int64  `json:"price_cents"`
		Stock int    `json:"stock"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	product, err := a.store.CreateProduct(r.Context(), input.SKU, input.Name, input.Price, input.Stock)
	switch {
	case errors.Is(err, store.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "invalid_input")
	case errors.Is(err, store.ErrDuplicateSKU):
		writeError(w, http.StatusConflict, "duplicate_sku")
	case err != nil:
		a.internalError(w, r, err)
	default:
		writeJSON(w, http.StatusCreated, product)
	}
}

func (a *API) createOrder(w http.ResponseWriter, r *http.Request) {
	var input domain.CreateOrderInput
	if !readJSON(w, r, &input) {
		return
	}
	order, err := a.store.CreateOrder(r.Context(), input)
	switch {
	case errors.Is(err, store.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "invalid_input")
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "product_not_found")
	case errors.Is(err, store.ErrInsufficientStock):
		writeError(w, http.StatusConflict, "insufficient_stock")
	case err != nil:
		a.internalError(w, r, err)
	default:
		writeJSON(w, http.StatusCreated, order)
	}
}

func (a *API) getOrder(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_id")
		return
	}
	order, err := a.store.GetOrder(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "order_not_found")
		return
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, order)
}

func (a *API) cancelOrder(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_id")
		return
	}
	if err := a.store.CancelOrder(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "order_not_found")
		return
	} else if err != nil {
		a.internalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func readJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	const maxBodySize = 1 << 20
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodySize))
	err := decoder.Decode(value)
	if err == nil {
		err = decoder.Decode(new(json.RawMessage))
		if errors.Is(err, io.EOF) {
			return true
		}
	}

	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large")
	} else {
		writeError(w, http.StatusBadRequest, "invalid_json")
	}
	return false
}

func (a *API) internalError(w http.ResponseWriter, r *http.Request, err error) {
	a.logger.ErrorContext(r.Context(), "request failed", "method", r.Method, "path", r.URL.Path, "error", err)
	writeError(w, http.StatusInternalServerError, "internal_error")
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

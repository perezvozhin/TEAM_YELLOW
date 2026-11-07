package api

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBadInputReturns400(t *testing.T) {
	a := New(nil, log.New(io.Discard, "", 0))
	cases := []struct{ name, method, path, body string }{
		{"empty sku", "POST", "/products", `{"sku":"","name":"x","price_cents":1,"stock":1}`},
		{"zero price", "POST", "/products", `{"sku":"A","name":"x","price_cents":0,"stock":1}`},
		{"negative stock", "POST", "/products", `{"sku":"A","name":"x","price_cents":1,"stock":-1}`},
		{"bad json", "POST", "/products", `{`},
		{"empty items", "POST", "/orders", `{"customer_email":"a@b.c","items":[]}`},
		{"zero quantity", "POST", "/orders", `{"customer_email":"a@b.c","items":[{"product_id":1,"quantity":0}]}`},
		{"bad order id", "GET", "/orders/abc", ``},
		{"bad cancel id", "POST", "/orders/0/cancel", ``},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
			rec := httptest.NewRecorder()
			a.Routes().ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("got %d, want 400", rec.Code)
			}
		})
	}
}
func TestRecoverFromPanic(t *testing.T) {
	var buf bytes.Buffer
	a := New(nil, log.New(&buf, "", 0))
	h := a.requestLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("got %d, want 500", rec.Code)
	}
	if !strings.Contains(buf.String(), "panic=boom") {
		t.Errorf("panic was not logged: %q", buf.String())
	}
}

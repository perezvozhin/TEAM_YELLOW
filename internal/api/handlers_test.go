package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hackathon/inventory-order-management/internal/domain"
	"github.com/hackathon/inventory-order-management/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var testLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestHTTPInput(t *testing.T) {
	handler := New(nil, testLogger).Routes()
	for _, tc := range []struct {
		method, path, body string
		status             int
		code               string
	}{
		{"GET", "/health", "", 200, ""},
		{"GET", "/orders/0", "", 400, "invalid_id"},
		{"POST", "/orders/no/cancel", "", 400, "invalid_id"},
		{"POST", "/products", `{`, 400, "invalid_json"},
		{"POST", "/orders", `{} {}`, 400, "invalid_json"},
		{"POST", "/orders", strings.Repeat(" ", 1<<20) + `{}`, 413, "body_too_large"},
	} {
		response := request(context.Background(), handler, tc.method, tc.path, tc.body)
		expectResponse(t, response, tc.status, tc.code)
	}
}

func TestProducts(t *testing.T) {
	ctx, _, handler := setupAPI(t)
	for _, body := range []string{
		`{"sku":"A","name":"  ","price_cents":1,"stock":0}`,
		`{"sku":"A","name":"Item","price_cents":0,"stock":0}`,
		`{"sku":"A","name":"Item","price_cents":1,"stock":-1}`,
	} {
		expectResponse(t, request(ctx, handler, "POST", "/products", body), 400, "invalid_input")
	}
	product := addProduct(t, ctx, handler, "A", 125, 0)
	expectResponse(t, request(ctx, handler, "POST", "/products", `{"sku":"A","name":"Other","price_cents":2,"stock":1}`), 409, "duplicate_sku")
	response := request(ctx, handler, "GET", "/products", "")
	expectResponse(t, response, 200, "")
	products := decode[[]domain.Product](t, response)
	if len(products) != 1 || products[0].ID != product.ID || products[0].Price != 125 || products[0].Stock != 0 {
		t.Fatalf("products = %+v, want only the original product", products)
	}
}

func TestOrderKeepsOriginalPrices(t *testing.T) {
	ctx, db, handler := setupAPI(t)
	first := addProduct(t, ctx, handler, "A", 125, 3)
	second := addProduct(t, ctx, handler, "B", 50, 1)
	// Duplicate lines share a stock limit and keep their original order in the response.
	items := fmt.Sprintf(`{"product_id":%d,"quantity":1},{"product_id":%d,"quantity":2},{"product_id":%d,"quantity":1}`, second.ID, first.ID, first.ID)
	response := request(ctx, handler, "POST", "/orders", orderBody(items))
	expectResponse(t, response, 201, "")
	created := decode[domain.Order](t, response)
	if created.Total != 425 || created.Status != "created" {
		t.Fatalf("created order = %+v", created)
	}

	if _, err := db.Exec(ctx, `UPDATE products SET price_cents = 999 WHERE id = $1`, first.ID); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/orders/%d", created.ID)
	response = request(ctx, handler, "GET", path, "")
	expectResponse(t, response, 200, "")
	order := decode[domain.Order](t, response)
	if order.Total != 425 || len(order.Items) != 3 ||
		order.Items[0].ProductID != second.ID || order.Items[0].UnitPrice != 50 ||
		order.Items[1].ProductID != first.ID || order.Items[1].UnitPrice != 125 || order.Items[1].Quantity != 2 ||
		order.Items[2].ProductID != first.ID || order.Items[2].UnitPrice != 125 || order.Items[2].Quantity != 1 {
		t.Fatalf("saved order = %+v", order)
	}
	expectResponse(t, request(ctx, handler, "POST", path+"/cancel", ""), 204, "")
	expectStock(t, ctx, db, first.ID, 3)
	expectStock(t, ctx, db, second.ID, 1)
}

func TestOrderFailureKeepsStockAndOrders(t *testing.T) {
	ctx, db, handler := setupAPI(t)
	first := addProduct(t, ctx, handler, "A", 125, 1)
	second := addProduct(t, ctx, handler, "B", 50, 2)
	expensive := addProduct(t, ctx, handler, "MAX", math.MaxInt64, 2)
	// Force a DB error when an otherwise valid position is saved.
	if _, err := db.Exec(ctx, `ALTER TABLE order_items ADD CHECK (quantity <> 2)`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		id     int64
		qty    int
		status int
		code   string
	}{
		{"invalid quantity", second.ID, 0, 400, "invalid_input"},
		{"missing product", expensive.ID + 1, 1, 404, "product_not_found"},
		{"repeated product exceeds stock", first.ID, 1, 409, "insufficient_stock"},
		{"short stock", second.ID, 3, 409, "insufficient_stock"},
		{"line price overflows", expensive.ID, 2, 400, "invalid_input"},
		{"order total overflows", expensive.ID, 1, 400, "invalid_input"},
		{"failed insert", second.ID, 2, 500, "internal_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items := fmt.Sprintf(`{"product_id":%d,"quantity":1},{"product_id":%d,"quantity":%d}`, first.ID, tc.id, tc.qty)
			expectResponse(t, request(ctx, handler, "POST", "/orders", orderBody(items)), tc.status, tc.code)
			expectStock(t, ctx, db, first.ID, 1)
			expectStock(t, ctx, db, second.ID, 2)
			expectStock(t, ctx, db, expensive.ID, 2)
			var orders, lines int
			err := db.QueryRow(ctx, `SELECT (SELECT COUNT(*) FROM orders), (SELECT COUNT(*) FROM order_items)`).Scan(&orders, &lines)
			if err != nil || orders != 0 || lines != 0 {
				t.Fatalf("after rejected order: orders=%d, lines=%d, err=%v", orders, lines, err)
			}
		})
	}
}

func TestConcurrentOrdersAndCancels(t *testing.T) {
	ctx, db, handler := setupAPI(t)
	product := addProduct(t, ctx, handler, "LAST", 125, 1)
	body := orderBody(fmt.Sprintf(`{"product_id":%d,"quantity":1}`, product.ID))

	var id int64
	bodies := make([]string, 8)
	for i := range bodies {
		bodies[i] = body
	}
	for _, response := range parallelRequests(ctx, handler, "/orders", bodies) {
		if response.Code == http.StatusCreated {
			if id != 0 {
				t.Fatal("the last unit was sold twice")
			}
			id = decode[domain.Order](t, response).ID
		} else {
			expectResponse(t, response, 409, "insufficient_stock")
		}
	}
	if id == 0 {
		t.Fatal("no order reserved the last unit")
	}
	expectStock(t, ctx, db, product.ID, 0)

	path := fmt.Sprintf("/orders/%d", id)
	clear(bodies)
	for _, response := range parallelRequests(ctx, handler, path+"/cancel", bodies) {
		expectResponse(t, response, 204, "")
	}
	expectResponse(t, request(ctx, handler, "POST", path+"/cancel", ""), 204, "")
	expectStock(t, ctx, db, product.ID, 1)
	response := request(ctx, handler, "GET", path, "")
	expectResponse(t, response, 200, "")
	if order := decode[domain.Order](t, response); order.Status != "cancelled" {
		t.Fatalf("status = %q, want cancelled", order.Status)
	}
}

func TestConcurrentOrdersWithSameEmail(t *testing.T) {
	ctx, db, handler := setupAPI(t)
	first := addProduct(t, ctx, handler, "A", 100, 3)
	second := addProduct(t, ctx, handler, "B", 10, 3)
	bodies := []string{
		orderBody(fmt.Sprintf(`{"product_id":%d,"quantity":2},{"product_id":%d,"quantity":1}`, first.ID, second.ID)),
		orderBody(fmt.Sprintf(`{"product_id":%d,"quantity":2},{"product_id":%d,"quantity":1}`, second.ID, first.ID)),
	}
	totals := []int64{210, 120}
	var previousID int64
	for i, response := range parallelRequests(ctx, handler, "/orders", bodies) {
		expectResponse(t, response, 201, "")
		created := decode[domain.Order](t, response)
		if created.ID == 0 || created.ID == previousID || created.Total != totals[i] {
			t.Fatalf("created order = %+v, want a separate order with total %d", created, totals[i])
		}
		previousID = created.ID
		response = request(ctx, handler, "GET", fmt.Sprintf("/orders/%d", created.ID), "")
		expectResponse(t, response, 200, "")
		if saved := decode[domain.Order](t, response); saved.Total != totals[i] || len(saved.Items) != 2 {
			t.Fatalf("saved order = %+v, want 2 items with total %d", saved, totals[i])
		}
	}
	expectStock(t, ctx, db, first.ID, 0)
	expectStock(t, ctx, db, second.ID, 0)
}

func TestFailedCancelKeepsOrderAndStock(t *testing.T) {
	ctx, db, handler := setupAPI(t)
	first := addProduct(t, ctx, handler, "A", 125, 1)
	second := addProduct(t, ctx, handler, "B", 50, 1)
	items := fmt.Sprintf(`{"product_id":%d,"quantity":1},{"product_id":%d,"quantity":1}`, first.ID, second.ID)
	response := request(ctx, handler, "POST", "/orders", orderBody(items))
	expectResponse(t, response, 201, "")
	order := decode[domain.Order](t, response)

	// Force the second stock update to fail after the first one can succeed.
	sql := fmt.Sprintf(`ALTER TABLE products ADD CHECK (id <> %d OR stock = 0)`, second.ID)
	if _, err := db.Exec(ctx, sql); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/orders/%d", order.ID)
	expectResponse(t, request(ctx, handler, "POST", path+"/cancel", ""), 500, "internal_error")
	expectStock(t, ctx, db, first.ID, 0)
	expectStock(t, ctx, db, second.ID, 0)
	response = request(ctx, handler, "GET", path, "")
	expectResponse(t, response, 200, "")
	if order := decode[domain.Order](t, response); order.Status != "created" {
		t.Fatalf("status = %q after failed cancellation, want created", order.Status)
	}
}

func setupAPI(t *testing.T) (context.Context, *pgxpool.Pool, http.Handler) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	schema := fmt.Sprintf("inventory_test_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := admin.Exec(cleanupCtx, `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Error(err)
		}
	})
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	for _, path := range []string{"001_init.up.sql", "002_constraints.up.sql"} {
		sql, err := os.ReadFile("../../migrations/" + path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", path, err)
		}
	}
	return ctx, db, New(store.New(db), testLogger).Routes()
}

func request(ctx context.Context, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func parallelRequests(ctx context.Context, handler http.Handler, path string, bodies []string) []*httptest.ResponseRecorder {
	responses := make([]*httptest.ResponseRecorder, len(bodies))
	start := make(chan struct{})
	done := make(chan struct{}, len(bodies))
	for i, body := range bodies {
		go func() {
			<-start
			responses[i] = request(ctx, handler, "POST", path, body)
			done <- struct{}{}
		}()
	}
	close(start)
	for range bodies {
		<-done
	}
	return responses
}

func expectResponse(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("HTTP %d (%s), want %d", w.Code, w.Body, status)
	}
	if code != "" && decode[map[string]string](t, w)["error"] != code {
		t.Fatalf("HTTP error body = %s, want %q", w.Body, code)
	}
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var result T
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode %s: %v", w.Body, err)
	}
	return result
}

func addProduct(t *testing.T, ctx context.Context, handler http.Handler, sku string, price int64, stock int) domain.Product {
	t.Helper()
	body := fmt.Sprintf(`{"sku":%q,"name":%q,"price_cents":%d,"stock":%d}`, sku, sku, price, stock)
	response := request(ctx, handler, "POST", "/products", body)
	expectResponse(t, response, 201, "")
	return decode[domain.Product](t, response)
}

func orderBody(items string) string {
	return `{"customer_email":"buyer@example.com","items":[` + items + `]}`
}

func expectStock(t *testing.T, ctx context.Context, db *pgxpool.Pool, id int64, want int) {
	t.Helper()
	var stock int
	if err := db.QueryRow(ctx, `SELECT stock FROM products WHERE id = $1`, id).Scan(&stock); err != nil {
		t.Fatal(err)
	}
	if stock != want {
		t.Fatalf("product %d stock = %d, want %d", id, stock, want)
	}
}

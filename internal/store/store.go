package store

import (
	"context"
	"errors"
	"math"
	"net/mail"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hackathon/inventory-order-management/internal/domain"
)

var ErrNotFound = errors.New("not found")
var ErrInsufficientStock = errors.New("insufficient stock")
var ErrInvalidInput = errors.New("invalid input")
var ErrDuplicateSKU = errors.New("duplicate SKU")

type Store struct {
	db *pgxpool.Pool
}

func New(db *pgxpool.Pool) *Store {
	return &Store{db: db}
}

func (s *Store) CreateProduct(ctx context.Context, sku, name string, price int64, stock int) (domain.Product, error) {
	if strings.TrimSpace(sku) == "" || strings.TrimSpace(name) == "" ||
		strings.ContainsRune(sku, 0) || strings.ContainsRune(name, 0) ||
		price <= 0 || stock < 0 || stock > math.MaxInt32 {
		return domain.Product{}, ErrInvalidInput
	}

	var p domain.Product
	err := s.db.QueryRow(ctx, `
		INSERT INTO products (sku, name, price_cents, stock)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (sku) DO NOTHING
		RETURNING id, sku, name, price_cents, stock, created_at
	`, sku, name, price, stock).Scan(&p.ID, &p.SKU, &p.Name, &p.Price, &p.Stock, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Product{}, ErrDuplicateSKU
	}
	return p, err
}

func (s *Store) ListProducts(ctx context.Context) ([]domain.Product, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, sku, name, price_cents, stock, created_at
		FROM products ORDER BY id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	products := make([]domain.Product, 0)
	for rows.Next() {
		var p domain.Product
		if err := rows.Scan(&p.ID, &p.SKU, &p.Name, &p.Price, &p.Stock, &p.CreatedAt); err != nil {
			return nil, err
		}
		products = append(products, p)
	}
	return products, rows.Err()
}

func (s *Store) CreateOrder(ctx context.Context, input domain.CreateOrderInput) (domain.Order, error) {
	email := strings.TrimSpace(input.CustomerEmail)
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email || len(input.Items) == 0 {
		return domain.Order{}, ErrInvalidInput
	}

	quantities := make(map[int64]int64)
	var productIDs []int64
	for _, item := range input.Items {
		if item.ProductID <= 0 || item.Quantity <= 0 || item.Quantity > math.MaxInt32 {
			return domain.Order{}, ErrInvalidInput
		}
		if quantities[item.ProductID] == 0 {
			productIDs = append(productIDs, item.ProductID)
		}
		quantities[item.ProductID] += int64(item.Quantity)
	}
	// Take product locks in the same order in creation and cancellation.
	slices.Sort(productIDs)

	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return domain.Order{}, err
	}
	defer tx.Rollback(ctx)

	products := make(map[int64]domain.Product, len(productIDs))
	var total int64
	for _, id := range productIDs {
		var product domain.Product
		err := tx.QueryRow(ctx, `
			SELECT name, price_cents, stock FROM products WHERE id = $1 FOR NO KEY UPDATE
		`, id).Scan(&product.Name, &product.Price, &product.Stock)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Order{}, ErrNotFound
		}
		if err != nil {
			return domain.Order{}, err
		}
		quantity := quantities[id]
		if int64(product.Stock) < quantity {
			return domain.Order{}, ErrInsufficientStock
		}
		if product.Price > (math.MaxInt64-total)/quantity {
			return domain.Order{}, ErrInvalidInput
		}
		total += product.Price * quantity
		products[id] = product
	}

	for _, id := range productIDs {
		if _, err := tx.Exec(ctx, `UPDATE products SET stock = stock - $1 WHERE id = $2`, quantities[id], id); err != nil {
			return domain.Order{}, err
		}
	}

	var result domain.Order
	err = tx.QueryRow(ctx, `
		INSERT INTO orders (customer_email, status, total_cents)
		VALUES ($1, 'created', $2)
		RETURNING id, customer_email, status, total_cents, created_at
	`, email, total).Scan(&result.ID, &result.CustomerEmail, &result.Status, &result.Total, &result.CreatedAt)
	if err != nil {
		return domain.Order{}, err
	}

	for _, item := range input.Items {
		product := products[item.ProductID]
		_, err = tx.Exec(ctx, `
			INSERT INTO order_items (order_id, product_id, product_name, quantity, unit_price_cents)
			VALUES ($1, $2, $3, $4, $5)
		`, result.ID, item.ProductID, product.Name, item.Quantity, product.Price)
		if err != nil {
			return domain.Order{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.Order{}, err
	}
	return result, nil
}

func (s *Store) GetOrder(ctx context.Context, id int64) (domain.Order, error) {
	var order domain.Order
	err := s.db.QueryRow(ctx, `
		SELECT id, customer_email, status, total_cents, created_at
		FROM orders WHERE id = $1
	`, id).Scan(&order.ID, &order.CustomerEmail, &order.Status, &order.Total, &order.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Order{}, ErrNotFound
	}
	if err != nil {
		return domain.Order{}, err
	}

	rows, err := s.db.Query(ctx, `
		SELECT product_id, product_name, quantity, unit_price_cents
		FROM order_items WHERE order_id = $1 ORDER BY id
	`, id)
	if err != nil {
		return domain.Order{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var item domain.OrderItem
		if err := rows.Scan(&item.ProductID, &item.ProductName, &item.Quantity, &item.UnitPrice); err != nil {
			return domain.Order{}, err
		}
		order.Items = append(order.Items, item)
	}
	return order, rows.Err()
}

func (s *Store) CancelOrder(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM orders WHERE id = $1 FOR NO KEY UPDATE`, id).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if status == "cancelled" {
		return nil
	}

	rows, err := tx.Query(ctx, `
		SELECT product_id, SUM(quantity) FROM order_items WHERE order_id = $1
		GROUP BY product_id ORDER BY product_id
	`, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	type line struct {
		productID int64
		quantity  int64
	}
	var lines []line
	for rows.Next() {
		var l line
		if err := rows.Scan(&l.productID, &l.quantity); err != nil {
			return err
		}
		lines = append(lines, l)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, l := range lines {
		_, err := tx.Exec(ctx, `
			UPDATE products SET stock = stock + $1 WHERE id = $2
		`, l.quantity, l.productID)
		if err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE orders SET status = 'cancelled' WHERE id = $1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

ALTER TABLE products
    ADD CONSTRAINT products_valid_values CHECK (
        sku ~ '[^[:space:]]' AND name ~ '[^[:space:]]' AND price_cents > 0 AND stock >= 0
    );

ALTER TABLE orders
    ADD CONSTRAINT orders_valid_values CHECK (
        customer_email ~ '[^[:space:]]' AND status IN ('created', 'cancelled') AND total_cents >= 0
    );

ALTER TABLE order_items
    ADD CONSTRAINT order_items_valid_values CHECK (quantity > 0 AND unit_price_cents > 0);

CREATE INDEX order_items_order_id_id_idx ON order_items (order_id, id);

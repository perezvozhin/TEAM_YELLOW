DROP INDEX order_items_order_id_id_idx;

ALTER TABLE order_items DROP CONSTRAINT order_items_valid_values;
ALTER TABLE orders DROP CONSTRAINT orders_valid_values;
ALTER TABLE products DROP CONSTRAINT products_valid_values;

BEGIN;

CREATE SCHEMA platform;
CREATE SCHEMA catalog;
CREATE SCHEMA warehouse;
CREATE SCHEMA inventory;
CREATE SCHEMA contracts_v1;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
ALTER DEFAULT PRIVILEGES REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC;

CREATE DOMAIN inventory.quantity AS numeric(24,6)
  CHECK (VALUE >= 0 AND VALUE <= 999999999999999999.999999);

CREATE TABLE platform.organizations (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  code text NOT NULL UNIQUE,
  name text NOT NULL
);
CREATE TABLE platform.actors (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  issuer text NOT NULL,
  subject text NOT NULL,
  UNIQUE (issuer, subject)
);
CREATE TABLE catalog.stock_owners (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES platform.organizations(id),
  code text NOT NULL,
  UNIQUE (organization_id, code),
  UNIQUE (organization_id, id)
);
CREATE TABLE catalog.units (
  code text PRIMARY KEY,
  dimension text NOT NULL CHECK (dimension IN ('count', 'mass', 'length', 'volume', 'area'))
);
CREATE TABLE catalog.items (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES platform.organizations(id),
  sku text NOT NULL,
  base_unit text NOT NULL REFERENCES catalog.units(code),
  quantity_scale smallint NOT NULL CHECK (quantity_scale BETWEEN 0 AND 6),
  tracking text NOT NULL CHECK (tracking IN ('none', 'lot', 'serial', 'lot_serial')),
  CHECK (tracking NOT IN ('serial', 'lot_serial') OR quantity_scale = 0),
  UNIQUE (organization_id, sku),
  UNIQUE (organization_id, id)
);
CREATE INDEX items_base_unit_idx ON catalog.items(base_unit);
CREATE TABLE catalog.packagings (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL,
  item_id uuid NOT NULL,
  code text NOT NULL,
  base_quantity inventory.quantity NOT NULL CHECK (base_quantity > 0),
  FOREIGN KEY (organization_id, item_id) REFERENCES catalog.items(organization_id, id),
  UNIQUE (organization_id, item_id, code)
);
CREATE TABLE catalog.lots (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL,
  item_id uuid NOT NULL,
  code text NOT NULL,
  expires_on date,
  FOREIGN KEY (organization_id, item_id) REFERENCES catalog.items(organization_id, id),
  UNIQUE (organization_id, item_id, code),
  UNIQUE (organization_id, item_id, id)
);
CREATE INDEX lots_expiry_idx ON catalog.lots(organization_id, item_id, expires_on, id)
  WHERE expires_on IS NOT NULL;
CREATE TABLE catalog.serials (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL,
  item_id uuid NOT NULL,
  lot_id uuid,
  code text NOT NULL,
  FOREIGN KEY (organization_id, item_id) REFERENCES catalog.items(organization_id, id),
  FOREIGN KEY (organization_id, item_id, lot_id) REFERENCES catalog.lots(organization_id, item_id, id),
  UNIQUE (organization_id, item_id, code),
  UNIQUE (organization_id, item_id, id)
);
CREATE INDEX serials_lot_idx ON catalog.serials(organization_id, item_id, lot_id) WHERE lot_id IS NOT NULL;

CREATE TABLE warehouse.warehouses (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES platform.organizations(id),
  code text NOT NULL,
  timezone text NOT NULL DEFAULT 'UTC',
  UNIQUE (organization_id, code),
  UNIQUE (organization_id, id)
);
CREATE TABLE warehouse.locations (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL,
  warehouse_id uuid NOT NULL,
  code text NOT NULL,
  kind text NOT NULL CHECK (kind IN ('receiving', 'storage', 'staging', 'shipping', 'returns', 'quarantine')),
  enabled boolean NOT NULL DEFAULT true,
  FOREIGN KEY (organization_id, warehouse_id) REFERENCES warehouse.warehouses(organization_id, id),
  UNIQUE (organization_id, warehouse_id, code),
  UNIQUE (organization_id, warehouse_id, id)
);
CREATE TABLE warehouse.handling_units (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL,
  warehouse_id uuid NOT NULL,
  location_id uuid NOT NULL,
  code text NOT NULL,
  version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
  FOREIGN KEY (organization_id, warehouse_id, location_id)
    REFERENCES warehouse.locations(organization_id, warehouse_id, id),
  UNIQUE (organization_id, warehouse_id, code),
  UNIQUE (organization_id, warehouse_id, id)
);
CREATE INDEX handling_units_location_idx ON warehouse.handling_units(organization_id, warehouse_id, location_id);
CREATE TABLE warehouse.positions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL,
  warehouse_id uuid NOT NULL,
  location_id uuid,
  handling_unit_id uuid,
  CHECK (num_nonnulls(location_id, handling_unit_id) = 1),
  FOREIGN KEY (organization_id, warehouse_id) REFERENCES warehouse.warehouses(organization_id, id),
  FOREIGN KEY (organization_id, warehouse_id, location_id)
    REFERENCES warehouse.locations(organization_id, warehouse_id, id),
  FOREIGN KEY (organization_id, warehouse_id, handling_unit_id)
    REFERENCES warehouse.handling_units(organization_id, warehouse_id, id),
  UNIQUE NULLS NOT DISTINCT (organization_id, warehouse_id, location_id, handling_unit_id),
  UNIQUE (organization_id, warehouse_id, id)
);
CREATE INDEX positions_hu_idx ON warehouse.positions(organization_id, warehouse_id, handling_unit_id)
  WHERE handling_unit_id IS NOT NULL;

-- The identity keys below are explicitly published FK targets for modules.
-- Mutable document columns are not a module storage API.
CREATE TABLE warehouse.documents (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL,
  warehouse_id uuid NOT NULL,
  owner_id uuid NOT NULL,
  kind text NOT NULL CHECK (kind IN ('receipt', 'shipment', 'transfer', 'adjustment')),
  number text NOT NULL,
  state text NOT NULL CHECK (state IN ('draft', 'active', 'closed', 'cancelled')),
  version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
  content_revision bigint NOT NULL DEFAULT 1 CHECK (content_revision > 0),
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  FOREIGN KEY (organization_id, warehouse_id) REFERENCES warehouse.warehouses(organization_id, id),
  FOREIGN KEY (organization_id, owner_id) REFERENCES catalog.stock_owners(organization_id, id),
  UNIQUE (organization_id, warehouse_id, kind, number),
  UNIQUE (organization_id, warehouse_id, owner_id, id),
  UNIQUE (organization_id, warehouse_id, id)
);
CREATE INDEX documents_owner_idx ON warehouse.documents(organization_id, owner_id);
CREATE TABLE warehouse.document_lines (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  document_id uuid NOT NULL,
  organization_id uuid NOT NULL,
  warehouse_id uuid NOT NULL,
  owner_id uuid NOT NULL,
  item_id uuid NOT NULL,
  line_no integer NOT NULL CHECK (line_no > 0),
  requested_qty inventory.quantity NOT NULL CHECK (requested_qty > 0),
  reserved_qty inventory.quantity NOT NULL DEFAULT 0,
  fulfilled_qty inventory.quantity NOT NULL DEFAULT 0,
  CHECK (reserved_qty + fulfilled_qty <= requested_qty),
  FOREIGN KEY (organization_id, warehouse_id, owner_id, document_id)
    REFERENCES warehouse.documents(organization_id, warehouse_id, owner_id, id),
  FOREIGN KEY (organization_id, item_id) REFERENCES catalog.items(organization_id, id),
  UNIQUE (document_id, line_no),
  UNIQUE (organization_id, warehouse_id, owner_id, item_id, id)
);
CREATE INDEX document_lines_scope_idx ON warehouse.document_lines(organization_id, warehouse_id, owner_id, document_id);
CREATE INDEX document_lines_item_idx ON warehouse.document_lines(organization_id, item_id);

CREATE TABLE inventory.stock_states (
  code text PRIMARY KEY,
  allocatable boolean NOT NULL,
  shippable boolean NOT NULL
);
INSERT INTO inventory.stock_states VALUES ('available', true, true), ('quarantine', false, false), ('damaged', false, false);
CREATE TABLE inventory.stock_balances (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL,
  warehouse_id uuid NOT NULL,
  owner_id uuid NOT NULL,
  item_id uuid NOT NULL,
  position_id uuid NOT NULL,
  stock_state text NOT NULL REFERENCES inventory.stock_states(code),
  lot_id uuid,
  serial_id uuid,
  on_hand inventory.quantity NOT NULL DEFAULT 0,
  reserved_qty inventory.quantity NOT NULL DEFAULT 0,
  version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
  CHECK (reserved_qty <= on_hand),
  CHECK (serial_id IS NULL OR on_hand <= 1),
  FOREIGN KEY (organization_id, warehouse_id, position_id)
    REFERENCES warehouse.positions(organization_id, warehouse_id, id),
  FOREIGN KEY (organization_id, owner_id) REFERENCES catalog.stock_owners(organization_id, id),
  FOREIGN KEY (organization_id, item_id) REFERENCES catalog.items(organization_id, id),
  FOREIGN KEY (organization_id, item_id, lot_id) REFERENCES catalog.lots(organization_id, item_id, id),
  FOREIGN KEY (organization_id, item_id, serial_id) REFERENCES catalog.serials(organization_id, item_id, id),
  UNIQUE NULLS NOT DISTINCT (organization_id, warehouse_id, owner_id, item_id, position_id, stock_state, lot_id, serial_id),
  UNIQUE (organization_id, warehouse_id, owner_id, item_id, id)
);
CREATE UNIQUE INDEX stock_serial_occupancy_uq ON inventory.stock_balances(serial_id)
  WHERE serial_id IS NOT NULL AND on_hand > 0;
CREATE INDEX stock_allocation_idx ON inventory.stock_balances(organization_id, warehouse_id, owner_id, item_id, stock_state, id)
  WHERE on_hand > reserved_qty;
CREATE INDEX stock_position_idx ON inventory.stock_balances(organization_id, warehouse_id, position_id);
CREATE INDEX stock_owner_idx ON inventory.stock_balances(organization_id, owner_id);
CREATE INDEX stock_item_lot_idx ON inventory.stock_balances(organization_id, item_id, lot_id);
CREATE INDEX stock_state_idx ON inventory.stock_balances(stock_state);
CREATE INDEX stock_serial_fk_idx ON inventory.stock_balances(organization_id, item_id, serial_id) WHERE serial_id IS NOT NULL;

CREATE TABLE platform.operations (
  id uuid PRIMARY KEY,
  actor_id uuid NOT NULL REFERENCES platform.actors(id),
  organization_id uuid NOT NULL,
  warehouse_id uuid NOT NULL,
  operation_type text NOT NULL,
  request jsonb NOT NULL,
  result jsonb,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  completed_at timestamptz,
  CHECK ((result IS NULL) = (completed_at IS NULL)),
  FOREIGN KEY (organization_id, warehouse_id) REFERENCES warehouse.warehouses(organization_id, id)
);
CREATE INDEX operations_actor_idx ON platform.operations(actor_id, created_at, id);
CREATE INDEX operations_scope_idx ON platform.operations(organization_id, warehouse_id, created_at, id);
CREATE TABLE inventory.reservations (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL,
  warehouse_id uuid NOT NULL,
  owner_id uuid NOT NULL,
  item_id uuid NOT NULL,
  document_line_id uuid NOT NULL,
  balance_id uuid NOT NULL,
  quantity inventory.quantity NOT NULL CHECK (quantity > 0),
  consumed_qty inventory.quantity NOT NULL DEFAULT 0,
  released_qty inventory.quantity NOT NULL DEFAULT 0,
  active_qty numeric(24,6) GENERATED ALWAYS AS (quantity - consumed_qty - released_qty) STORED,
  expires_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  CHECK (consumed_qty + released_qty <= quantity),
  FOREIGN KEY (organization_id, warehouse_id, owner_id, item_id, document_line_id)
    REFERENCES warehouse.document_lines(organization_id, warehouse_id, owner_id, item_id, id),
  FOREIGN KEY (organization_id, warehouse_id, owner_id, item_id, balance_id)
    REFERENCES inventory.stock_balances(organization_id, warehouse_id, owner_id, item_id, id)
);
CREATE INDEX reservations_line_idx ON inventory.reservations(organization_id, warehouse_id, owner_id, item_id, document_line_id);
CREATE INDEX reservations_balance_fk_idx ON inventory.reservations(organization_id, warehouse_id, owner_id, item_id, balance_id);
CREATE INDEX reservations_active_balance_idx ON inventory.reservations(balance_id, id) WHERE active_qty > 0;
CREATE INDEX reservations_expiry_idx ON inventory.reservations(expires_at, id) WHERE active_qty > 0 AND expires_at IS NOT NULL;

CREATE TABLE inventory.movements (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  operation_id uuid NOT NULL UNIQUE REFERENCES platform.operations(id),
  document_line_id uuid NOT NULL,
  organization_id uuid NOT NULL,
  warehouse_id uuid NOT NULL,
  owner_id uuid NOT NULL,
  item_id uuid NOT NULL,
  kind text NOT NULL CHECK (kind IN ('receipt', 'transfer', 'issue', 'adjustment_in', 'adjustment_out')),
  reason text,
  correction_of uuid REFERENCES inventory.movements(id),
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  CHECK (kind NOT IN ('adjustment_in', 'adjustment_out') OR nullif(btrim(reason), '') IS NOT NULL),
  FOREIGN KEY (organization_id, warehouse_id, owner_id, item_id, document_line_id)
    REFERENCES warehouse.document_lines(organization_id, warehouse_id, owner_id, item_id, id)
);
CREATE INDEX movements_line_idx ON inventory.movements(organization_id, warehouse_id, owner_id, item_id, document_line_id);
CREATE INDEX movements_scope_idx ON inventory.movements(organization_id, warehouse_id, created_at, id);
CREATE INDEX movements_correction_idx ON inventory.movements(correction_of) WHERE correction_of IS NOT NULL;
CREATE TABLE inventory.movement_lines (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  movement_id uuid NOT NULL REFERENCES inventory.movements(id),
  line_no integer NOT NULL DEFAULT 1 CHECK (line_no > 0),
  source_balance_id uuid REFERENCES inventory.stock_balances(id),
  target_balance_id uuid REFERENCES inventory.stock_balances(id),
  quantity inventory.quantity NOT NULL CHECK (quantity > 0),
  CHECK (num_nonnulls(source_balance_id, target_balance_id) >= 1),
  CHECK (source_balance_id IS DISTINCT FROM target_balance_id),
  UNIQUE (movement_id, line_no)
);
CREATE INDEX movement_lines_source_idx ON inventory.movement_lines(source_balance_id, id) WHERE source_balance_id IS NOT NULL;
CREATE INDEX movement_lines_target_idx ON inventory.movement_lines(target_balance_id, id) WHERE target_balance_id IS NOT NULL;
CREATE TABLE inventory.reservation_events (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  operation_id uuid NOT NULL REFERENCES platform.operations(id),
  reservation_id uuid NOT NULL REFERENCES inventory.reservations(id),
  kind text NOT NULL CHECK (kind IN ('reserved', 'released', 'consumed')),
  quantity inventory.quantity NOT NULL CHECK (quantity > 0),
  reason text,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  UNIQUE (operation_id, reservation_id, kind)
);
CREATE INDEX reservation_events_reservation_idx ON inventory.reservation_events(reservation_id, id);

CREATE TABLE platform.outbox (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  operation_id uuid NOT NULL REFERENCES platform.operations(id),
  event_type text NOT NULL,
  event_version integer NOT NULL DEFAULT 1 CHECK (event_version > 0),
  aggregate_id uuid NOT NULL,
  payload jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  available_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  leased_until timestamptz,
  leased_by text,
  attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  delivered_at timestamptz,
  dead_at timestamptz,
  UNIQUE (operation_id, event_type)
);
CREATE INDEX outbox_ready_idx ON platform.outbox(available_at, id) WHERE delivered_at IS NULL AND dead_at IS NULL;
CREATE TABLE platform.inbox (
  consumer_id text NOT NULL,
  event_id uuid NOT NULL,
  processed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (consumer_id, event_id)
);
CREATE TABLE platform.audit_entries (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  operation_id uuid NOT NULL REFERENCES platform.operations(id),
  action text NOT NULL,
  details jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX audit_operation_idx ON platform.audit_entries(operation_id, id);

CREATE TABLE platform.modules (
  id text PRIMARY KEY CHECK (id ~ '^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$'),
  version text NOT NULL,
  schema_name name NOT NULL UNIQUE,
  state text NOT NULL CHECK (state IN ('installed', 'migrated', 'validated', 'active', 'draining', 'disabled')),
  manifest jsonb NOT NULL
);
CREATE TABLE platform.module_migrations (
  module_id text NOT NULL REFERENCES platform.modules(id),
  migration_id text NOT NULL,
  checksum text NOT NULL CHECK (checksum ~ '^[0-9a-f]{64}$'),
  applied_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (module_id, migration_id)
);

CREATE VIEW contracts_v1.stock_balances AS
SELECT b.id, b.organization_id, b.warehouse_id, b.owner_id, b.item_id, b.position_id,
       b.stock_state, b.lot_id, b.serial_id, b.on_hand, b.reserved_qty,
       CASE WHEN s.allocatable THEN b.on_hand - b.reserved_qty ELSE 0::numeric END AS available_qty,
       b.version
FROM inventory.stock_balances b JOIN inventory.stock_states s ON s.code = b.stock_state;
CREATE VIEW contracts_v1.documents AS
SELECT id, organization_id, warehouse_id, owner_id, kind, number, state, version, content_revision
FROM warehouse.documents;

REVOKE ALL ON ALL TABLES IN SCHEMA platform, catalog, warehouse, inventory FROM PUBLIC;
COMMIT;

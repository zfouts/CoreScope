-- Version 4: the global ducting grid (ducting.source "gfs-grid").
--
-- One row per (model, valid_time): data is the encoded forecast grid
-- (internal/ducting), all times are unix seconds. The ingestor replaces a row
-- only with a strictly newer run_time and prunes by valid_time; the server
-- only reads it. No GRANT here: like every other table, the read-only role
-- gets SELECT from the writer's default privileges (scripts/postgres-roles.sh).
--
-- Never edit this file once shipped: add a new NNNN_name.sql instead.

CREATE TABLE ducting_grid (
	model      TEXT COLLATE "C" NOT NULL,
	valid_time BIGINT NOT NULL,
	run_time   BIGINT NOT NULL,
	lead_hours BIGINT NOT NULL,
	data       BYTEA NOT NULL,
	created_at BIGINT NOT NULL,
	PRIMARY KEY (model, valid_time)
);

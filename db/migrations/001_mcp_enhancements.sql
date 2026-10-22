-- Migration 001: MCP enhancements
-- Add auth_token, http transport type, and per-tool enable/disable table.
-- Run once on existing databases: psql $DATABASE_URL -f db/migrations/001_mcp_enhancements.sql

ALTER TABLE mcp_servers
    ADD COLUMN IF NOT EXISTS auth_token TEXT;

ALTER TABLE mcp_servers
    DROP CONSTRAINT IF EXISTS mcp_servers_type_check;

ALTER TABLE mcp_servers
    ADD CONSTRAINT mcp_servers_type_check CHECK (type IN ('stdio', 'sse', 'http'));

CREATE TABLE IF NOT EXISTS mcp_tools (
    id          SERIAL PRIMARY KEY,
    server_id   INT  NOT NULL REFERENCES mcp_servers(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    description TEXT,
    enabled     BOOLEAN DEFAULT TRUE,
    UNIQUE (server_id, name)
);

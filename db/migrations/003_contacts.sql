-- Migration 003: phone directory (contacts)
-- Run once on existing databases: psql $DATABASE_URL -f db/migrations/003_contacts.sql

CREATE TABLE IF NOT EXISTS contacts (
    id           SERIAL PRIMARY KEY,
    name         TEXT NOT NULL,
    phone_number TEXT NOT NULL,
    notes        TEXT,
    enabled      BOOLEAN DEFAULT TRUE,
    created_at   TIMESTAMPTZ DEFAULT NOW()
);

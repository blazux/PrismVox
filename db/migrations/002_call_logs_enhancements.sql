-- Migration 002: call_logs — caller number and end reason
-- Run once on existing databases: psql $DATABASE_URL -f db/migrations/002_call_logs_enhancements.sql

ALTER TABLE call_logs
    ADD COLUMN IF NOT EXISTS caller_number TEXT,
    ADD COLUMN IF NOT EXISTS end_reason    TEXT;

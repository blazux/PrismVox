-- Also applied automatically by the UI on startup; safe for existing histories.
ALTER TABLE chat_sessions ADD COLUMN IF NOT EXISTS context_start INTEGER NOT NULL DEFAULT 0;

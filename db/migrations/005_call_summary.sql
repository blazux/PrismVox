ALTER TABLE call_logs ADD COLUMN IF NOT EXISTS summary TEXT;

INSERT INTO config (key, value)
VALUES (
    'admin_chat_prompt',
    'You supervise the reception agent. Answer concisely and factually in the operator’s language.'
)
ON CONFLICT (key) DO NOTHING;

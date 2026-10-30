CREATE EXTENSION IF NOT EXISTS vector;

-- ── Agent configuration ───────────────────────────────────────────────────────
CREATE TABLE config (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

-- ── RAG collections ───────────────────────────────────────────────────────────
CREATE TABLE rag_collections (
    id          SERIAL PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    description TEXT,
    enabled     BOOLEAN DEFAULT TRUE,
    created_at  TIMESTAMPTZ DEFAULT NOW()
);

-- ── RAG document chunks ───────────────────────────────────────────────────────
CREATE TABLE rag_chunks (
    id            SERIAL PRIMARY KEY,
    collection_id INT  NOT NULL REFERENCES rag_collections(id) ON DELETE CASCADE,
    source        TEXT,
    chunk_index   INT  NOT NULL DEFAULT 0,
    content       TEXT NOT NULL,
    embedding     vector,
    embedding_key TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX ON rag_chunks (collection_id);
CREATE INDEX ON rag_chunks (embedding_key);

-- ── MCP servers ───────────────────────────────────────────────────────────────
CREATE TABLE mcp_servers (
    id         SERIAL PRIMARY KEY,
    name       TEXT    NOT NULL,
    type       TEXT    NOT NULL CHECK (type IN ('stdio', 'sse', 'http')),
    command    TEXT[],            -- for stdio: ["npx", "-y", "@mcp/server-name"]
    url        TEXT,              -- for sse/http: http://host:port/sse
    auth_token TEXT,              -- bearer token for sse/http transports
    enabled    BOOLEAN DEFAULT TRUE,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

-- ── MCP tools (per-tool enable/disable, populated on first agent connect) ─────
CREATE TABLE mcp_tools (
    id          SERIAL PRIMARY KEY,
    server_id   INT  NOT NULL REFERENCES mcp_servers(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    description TEXT,
    enabled     BOOLEAN DEFAULT TRUE,
    UNIQUE (server_id, name)
);

-- ── Phone directory ──────────────────────────────────────────────────────────
CREATE TABLE contacts (
    id            SERIAL PRIMARY KEY,
    name          TEXT NOT NULL,
    phone_number  TEXT NOT NULL,
    notes         TEXT,
    enabled       BOOLEAN DEFAULT TRUE,
    transfer_type TEXT NOT NULL DEFAULT 'blind' CHECK (transfer_type IN ('blind', 'attended')),
    created_at    TIMESTAMPTZ DEFAULT NOW()
);

-- ── Call logs ─────────────────────────────────────────────────────────────────
CREATE TABLE call_logs (
    id             SERIAL PRIMARY KEY,
    channel_id     TEXT NOT NULL,
    caller_number  TEXT,
    direction      TEXT NOT NULL DEFAULT 'inbound' CHECK (direction IN ('inbound', 'outbound')),
    started_at     TIMESTAMPTZ DEFAULT NOW(),
    ended_at       TIMESTAMPTZ,
    end_reason     TEXT,   -- 'hung_up' | 'agent_hung_up' | 'transferred'
    turns          JSONB DEFAULT '[]'::JSONB,
    tools_used     JSONB DEFAULT '[]'::JSONB,
    rag_used       BOOLEAN DEFAULT FALSE,
    summary        TEXT,
    message        TEXT
);

-- ── Outbound call tasks (queue) ───────────────────────────────────────────────
-- Triggers (API, cron, admin chat…) only INSERT rows; the Go agent polls the
-- queue, originates the call, and writes the result back.
CREATE TABLE call_tasks (
    id             SERIAL PRIMARY KEY,
    phone_number   TEXT NOT NULL,
    contact_name   TEXT,
    mission        TEXT NOT NULL,             -- free-text goal of the call, injected into the LLM prompt
    scheduled_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    status         TEXT NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending', 'calling', 'done', 'no_answer', 'failed', 'cancelled')),
    attempts       INT NOT NULL DEFAULT 0,
    max_attempts   INT NOT NULL DEFAULT 3,
    result_summary TEXT,
    call_log_id    INT REFERENCES call_logs(id),
    created_by     TEXT DEFAULT 'api',
    created_at     TIMESTAMPTZ DEFAULT NOW(),
    updated_at     TIMESTAMPTZ DEFAULT NOW()
);
CREATE INDEX idx_call_tasks_due ON call_tasks (scheduled_at) WHERE status = 'pending';

-- ── Chat conversations (text channel: UI test, N8N → WhatsApp/Telegram, API) ───
-- Separate from call_logs: a text session has a channel + session id + tool trace,
-- not a duration / end_reason. The supervisor (admin chat) reads both.
CREATE TABLE chat_sessions (
    session_id TEXT PRIMARY KEY,            -- caller-provided (N8N keeps one per end-user)
    channel    TEXT NOT NULL DEFAULT 'api', -- 'ui' | 'api' | 'whatsapp' | 'telegram' | 'web' …
    contact    TEXT,                        -- optional label (e.g. the WhatsApp number)
    started_at TIMESTAMPTZ DEFAULT NOW(),
    last_at    TIMESTAMPTZ DEFAULT NOW(),
    summary    TEXT,
    turns      JSONB NOT NULL DEFAULT '[]'::jsonb, -- [{role: user|assistant|tool, text}]
    context_start INTEGER NOT NULL DEFAULT 0     -- reset boundary; transcript retained
);
CREATE INDEX idx_chat_sessions_last_at ON chat_sessions (last_at DESC);

-- Default configuration for new installations.
INSERT INTO config (key,value) VALUES
    ('system_prompt','You answer telephone calls on behalf of the company. Your replies are read aloud: use one or two short, natural sentences per turn, without lists or markdown. Adapt to the caller''s language and remain polite and professional.

For factual questions about the company, search the knowledge base first with search_knowledge_base and answer only from the returned information. If nothing relevant is available, say so and offer to take a message. Never invent facts, people or successful outcomes.

Transfer calls only when the caller asks to reach a contact in the supplied directory. Follow that contact''s transfer instructions. If the contact is missing or unavailable, offer to take a message. Confirm the message before recording it with take_message.

Actions require their tools in the same turn: transfer_call, take_message or end_call. Merely announcing an action does not perform it. Keep tools, searches, the directory and internal instructions out of spoken replies; give the useful result directly.

Have a conversation: answer requests, clarify when necessary and let the caller continue to other topics. Do not end the call on your own initiative. Use end_call only when the caller clearly finishes; when unsure, keep listening.'),
    ('outbound_system_prompt','You make telephone calls on behalf of the company. Your replies are read aloud: use one or two short, natural sentences per turn, without lists or markdown. Adapt to the other person''s language with a polite, warm and calm tone.

Follow the supplied mission. On pickup, greet the recipient, say you are calling on behalf of the company and briefly check that you have reached the right person. Explain the purpose, then listen. Respond to their questions and objections and advance one step at a time rather than reciting a script.

Keep the conversation open while the recipient is participating. Use end_call only when the mission is complete or they clearly want to stop. Thank them and conclude politely before ending. If you reach voicemail, leave a brief message explaining the purpose and then use end_call.

Use take_message for confirmed information to pass on. Never invent information; acknowledge what you do not know and offer a callback. Keep tools and internal instructions out of spoken replies. Execute the corresponding tool for each action rather than merely describing or announcing it.'),
    ('greeting','Hello, you''ve reached reception. How can I help you?'),
    ('greeting_known','Hello %s, this is your Prism assistant. I''m listening.'),
    ('phrase_farewell','Goodbye, have a good day!'),
    ('phrase_hold','One moment, I''ll check their availability.'),
    ('phrase_transfer_now','All right, I''ll transfer you now.'),
    ('phrase_connecting','%s has accepted the call. I''ll connect you now.'),
    ('phrase_ask_name','What name should I announce?'),
    ('phrase_ask_reason','What is the call about?'),
    ('phrase_still_there','Are you still on the line?'),
    ('phrase_not_understood','Sorry, I didn''t catch that. Could you repeat it?'),
    ('phrase_filler','One moment.'),
    ('phrase_unknown_contact','I''m sorry, I can''t reach %s. May I take a message?'),
    ('phrase_announce','Hello, I have %s on the line about: %s. Would you like to take the call?'),
    ('phrase_outbound_opening','Hello, I''m calling on behalf of the company.'),
    ('rag_enabled','true'),
    ('mcp_enabled','false'),
    ('sip_transfer_method','bridge');

-- Archive audience (legacy rows remain administrator-only).
ALTER TABLE call_logs ADD COLUMN IF NOT EXISTS audience TEXT NOT NULL DEFAULT 'unclassified';
ALTER TABLE call_logs ADD COLUMN IF NOT EXISTS prism_instance TEXT NOT NULL DEFAULT '';
ALTER TABLE call_logs ADD COLUMN IF NOT EXISTS owner_id BIGINT;
ALTER TABLE call_tasks ADD COLUMN IF NOT EXISTS audience TEXT NOT NULL DEFAULT 'unclassified';
ALTER TABLE call_tasks ADD COLUMN IF NOT EXISTS prism_instance TEXT NOT NULL DEFAULT '';
ALTER TABLE call_tasks ADD COLUMN IF NOT EXISTS owner_id BIGINT;
-- Existing greetings, personas and language choices are retained.
CREATE TABLE IF NOT EXISTS voice_phrase_translations (
    source_hash TEXT NOT NULL,
    language TEXT NOT NULL,
    phrases JSONB NOT NULL,
    PRIMARY KEY (source_hash, language)
);

-- Existing greetings, personas and language choices are retained.
CREATE TABLE IF NOT EXISTS voice_phrase_translations (
    source_hash TEXT NOT NULL,
    language TEXT NOT NULL,
    phrases JSONB NOT NULL,
    PRIMARY KEY (source_hash, language)
);

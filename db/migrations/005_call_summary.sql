ALTER TABLE call_logs ADD COLUMN IF NOT EXISTS summary TEXT;

INSERT INTO config (key, value)
VALUES (
    'admin_chat_prompt',
    'Tu es l''assistant administratif de Prism Vox, un standard téléphonique IA. Tu as accès aux statistiques et à l''historique résumé des appels récents. Réponds en français, de manière concise et factuelle.'
)
ON CONFLICT (key) DO NOTHING;

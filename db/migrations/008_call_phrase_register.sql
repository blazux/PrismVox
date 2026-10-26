-- Harmonize stock call phrases while preserving administrator customizations.
-- Apply to existing databases with psql -f db/migrations/008_call_phrase_register.sql.
BEGIN;
UPDATE config AS c
SET value = p.new_value, updated_at = NOW()
FROM (VALUES
    ('greeting_known', 'Bonjour %s, c''est votre assistant. Que puis-je faire pour vous ?', 'Bonjour %s, c''est l''assistant Prism. Je suis à l''écoute.'),
    ('phrase_farewell', 'Merci de votre appel, au revoir !', 'Au revoir, bonne journée !'),
    ('phrase_hold', 'Je vérifie sa disponibilité, je vous mets en attente un instant.', 'Un instant, je vérifie sa disponibilité.'),
    ('phrase_transfer_now', 'Très bien, je vous transfère.', 'Très bien, je lance le transfert.'),
    ('phrase_connecting', '%s prend votre appel, je vous le passe.', '%s a accepté l''appel, je lance la mise en relation.'),
    ('phrase_ask_name', 'Puis-je avoir votre nom, s''il vous plaît ?', 'Quel nom dois-je annoncer ?'),
    ('phrase_ask_reason', 'Et quel est l''objet de votre appel ?', 'Quel est le motif de l''appel ?'),
    ('phrase_not_understood', 'Pardon, je n''ai pas bien entendu. Pouvez-vous répéter ?', 'Pardon, je n''ai pas bien entendu. Est-ce possible de répéter ?')
) AS p(key, old_value, new_value)
WHERE c.key = p.key AND c.value = p.old_value;
COMMIT;

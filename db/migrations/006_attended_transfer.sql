ALTER TABLE contacts
  ADD COLUMN IF NOT EXISTS transfer_type TEXT NOT NULL DEFAULT 'blind'
    CONSTRAINT contacts_transfer_type_check CHECK (transfer_type IN ('blind', 'attended'));

ALTER TABLE call_logs
  ADD COLUMN IF NOT EXISTS message TEXT;

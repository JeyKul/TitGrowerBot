DROP INDEX IF EXISTS tits_idx_chat_id;
CREATE INDEX IF NOT EXISTS tits_idx_chat_id_length ON Tits(chat_id, length DESC);

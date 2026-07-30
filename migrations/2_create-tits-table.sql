CREATE TABLE IF NOT EXISTS Tits (
    uid bigint REFERENCES Users(uid) ON DELETE CASCADE,
    chat_id bigint,
    length integer NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT current_timestamp,

    PRIMARY KEY (uid, chat_id)
);

CREATE OR REPLACE FUNCTION check_and_update_tits_timestamp()
    RETURNS TRIGGER
    LANGUAGE PLPGSQL
AS $$
BEGIN
    IF current_date = date(OLD.updated_at) THEN
        RAISE EXCEPTION 'Your tit has been already grown today!'
            USING ERRCODE = 'GD0E1';
    END IF;

    NEW.updated_at := current_timestamp;
    RETURN NEW;
END
$$;

CREATE OR REPLACE TRIGGER trg_check_and_update_tits_timestamp BEFORE INSERT OR UPDATE ON Tits
    FOR EACH ROW EXECUTE FUNCTION check_and_update_tits_timestamp();

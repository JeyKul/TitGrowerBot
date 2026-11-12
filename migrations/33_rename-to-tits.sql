-- TitGrowerBot names its tables after tits. Only renames on a fresh database.
DO $$ BEGIN
    IF to_regclass('public.tits') IS NULL THEN
        ALTER TABLE Dicks RENAME TO Tits;
        ALTER TABLE Dick_of_Day RENAME TO Titties_of_Day;
        ALTER TABLE Stale_Dick_Shrinks RENAME TO Stale_Tit_Shrinks;
    END IF;
END $$;

-- The DoD trigger must look at the renamed table.
CREATE OR REPLACE FUNCTION check_dod_timestamp()
    RETURNS TRIGGER
    LANGUAGE PLPGSQL
AS $$
DECLARE
    dod_name varchar;
BEGIN
    SELECT name INTO dod_name FROM Titties_of_Day dod
        JOIN Users u ON dod.winner_uid = u.uid
        WHERE dod.created_at = current_date AND chat_id = NEW.chat_id;
    IF dod_name IS NOT NULL THEN
        RAISE EXCEPTION '%', dod_name
            USING ERRCODE = 'GD0E2';
    END IF;
    NEW.created_at := current_timestamp;
    RETURN NEW;
END
$$;

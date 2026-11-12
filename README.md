TitGrowerBot
============

A Telegram game bot for group chats. Players grow their virtual length once a day
and compete with the rest of the chat. Written in Go; based on
[DickGrowerBot](https://github.com/kozalosev/DickGrowerBot), and uses the same database.

Credits
-------
The idea, game design and texts come from **[DickGrowerBot](https://github.com/kozalosev/DickGrowerBot)**
by [Leonid Kozarin (kozalosev)](https://github.com/kozalosev). This project is a fork, rewritten in Go.
It stays under the original MIT + Commons Clause license (see `LICENSE`): the software may not be sold.

Commands
--------
* `/grow` — grow once a day (random, can be negative)
* `/top` — the chat leaderboard
* `/dod` — elect the Dick of the Day, who gets a bonus
* `/pvp <bet>` — challenge the chat; the winner takes the bet
* `/stats` — your stats
* `/loan` — negative length? reset it to zero and repay from future growth
* `/language` — chat admins pick the chat language
* `/help`

Project
-------
| File | What it does |
|------|--------------|
| `main.go` | settings, startup, routing |
| `game.go` | all commands and buttons |
| `db.go` | all SQL |
| `texts.go` | loads the locale files |
| `locales/*.txt` | texts, one `key = text` per line |
| `migrations/` | database schema (applied only to an empty database) |

### Editing texts
Open `locales/en.txt` (or another language). `\n` is a line break, `{name}` is filled in
by the bot, HTML tags like `<b>` work. A key missing in a language falls back to English.
To add a language, copy `en.txt` to `<code>.txt` and translate it.

Running
-------
```bash
cp .env.example .env   # then set BOT_TOKEN and the password
docker compose up -d --build
```

Without Docker: `go run .` with `BOT_TOKEN` and `DATABASE_URL` set.

### Backups
The `backup` container writes `backups/db-<date>.sql.gz` on start and once a day,
and deletes files older than `BACKUP_KEEP_DAYS`. Backup by hand:
```bash
docker compose exec postgres sh -c 'pg_dump -U "$POSTGRES_USER" "$POSTGRES_DB"' | gzip > manual.sql.gz
```
Restore into an empty database (a fresh `data/` folder), with the bot stopped:
```bash
gunzip -c backups/db-XXXX.sql.gz | docker compose exec -T postgres sh -c 'psql -U "$POSTGRES_USER" "$POSTGRES_DB"'
```

### Moving from the old Rust bot
Point `DATABASE_URL` at the old database. No migration is needed; the bot sees the
existing tables and does not touch the schema. Old PvP and loan buttons keep working.

Dropped from the old bot: inline mode, `/import`, `/promo`, daily shrinks,
announcements, metrics, the user-service, message self-destruction and webhooks
(the bot uses long polling). Their tables stay in the database untouched.

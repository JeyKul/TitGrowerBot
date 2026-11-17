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
* `/gender` — she / he / they, and 🍒 tits or 🍆 dick
* `/app` — open the web app
* `/language` — chat admins pick the chat language
* `/help`

**Inline mode:** type `@TitGrowerBot` in any chat to grow, see the top, elect, check stats or
take a loan; `@TitGrowerBot 5` starts a fight for 5. In supergroups this counts towards the same
score as the normal commands. Turn it on once in @BotFather: `/setinline`.

Project
-------
| File | What it does |
|------|--------------|
| `main.go` | settings, startup, routing |
| `game.go` | all commands and buttons |
| `db.go` | all SQL |
| `inline.go` | inline mode |
| `web.go`, `web/index.html` | the web app and admin panel |
| `backup.go` | daily backups |
| `texts.go` | loads the locale files |
| `locales/*.txt` | texts, one `key = text` per line |
| `migrations/` | database schema (applied only to an empty database) |

### Web app
A Telegram Mini App. Players see their size in every chat, the tops, and pick their gender.
Admins (`ADMIN_IDS`) also get an admin tab: search users, set lengths, allow another grow today,
clear loans, block/unblock, delete. In a group, admins can also reply to someone with `/resetgrow`
(grow once more today, size stays) or `/clearloan`, or give the user id: `/resetgrow 123456789`.
Blocked users are ignored by the bot and hidden from tops and elections.

Telegram needs **https**. Set in `.env`:
```
WEB_URL=https://jey.kulovac.de/tits
WEB_ADDR=127.0.0.1:8085
ADMIN_IDS=123456789
```
Then forward `/tits` on your web server to the bot. **Caddy** (gets the certificate by itself):
```
jey.kulovac.de {
    handle /tits* {
        reverse_proxy 127.0.0.1:8085
    }
}
```
**nginx** (inside the `server { ... }` block for jey.kulovac.de that already has https):
```
location /tits {
    proxy_pass http://127.0.0.1:8085;
    proxy_set_header Host $host;
}
```
The bot puts the app on its menu button in private chats; `/app` in a group links there.

### Editing texts
Open `locales/en.txt` (or another language). `\n` is a line break, `{name}` is filled in
by the bot, HTML tags like `<b>` work. A key missing in a language falls back to English.
A key can have gender variants: `key.dick` for 🍆 players, `key.he` / `key.they` for pronouns,
and `key.dick.she` / `key.dick.they`. Without a variant the plain key is used (🍒 + she).
In texts about a player, `{pet}` becomes girl, boy or pet by their pronoun ("good {pet}").
For variety, give any key numbered alternatives: `key.1`, `key.2`, `key.3`, ... (also for
gender variants, e.g. `grow.direction.grown.dick.2`). Each time one of them, or the plain key, is
picked at random. Add as many as you like, the bot counts them when it starts.
`grow.tease.grown` / `grow.tease.shrunk` add an optional teasing line after a grow; a language
without these keys just doesn't show one.
`size.dick.under5`, `size.tits.over20`, ... add a line for the new size after a grow: `underN` means
below N, `overN` above N (N may be negative). "under" tiers are checked first (smallest N first),
then "over" (biggest N first). Pronoun and number suffixes work as above: `size.dick.under5.they.1`.
To add a language, copy `en.txt` to `<code>.txt` and translate it.

Running
-------
```bash
cp .env.example .env   # then set BOT_TOKEN and the password
docker compose up -d --build
```

Without Docker: put `BOT_TOKEN` and `DATABASE_URL` in `.env` (or the environment) and run `go run .`

### Backups
The bot backs up the database itself: on start and every 24 hours it runs `pg_dump`
(needs the PostgreSQL client installed) into `BACKUP_DIR` (default `backups/`) and
deletes files older than `BACKUP_KEEP_DAYS` (default 14, `0` = off).
With Docker, the `backup` container does this instead: it writes `backups/db-<date>.sql.gz` on start and once a day,
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

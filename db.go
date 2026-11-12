package main

// All SQL lives here. The schema is the old one (see migrations/), so the old database works as is.
//
// "chat" in this file is always the internal id (Chats.id), not the Telegram chat id.
//
// Note on the Tits table: a database trigger refuses a second UPDATE of a row on the same day
// unless bonus_attempts > 0, and every UPDATE uses up one bonus attempt. So any change that is
// not the daily /grow must do "bonus_attempts = bonus_attempts + 1".

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	errGrownToday = "GD0E1" // raised by the trigger on Tits
	errDodChosen  = "GD0E2" // raised by the trigger on Titties_of_Day
)

// notBlocked is added to queries so blocked users vanish from tops and elections.
const notBlocked = "uid NOT IN (SELECT uid FROM Blocked_Users)"

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// querier is a pool or a transaction.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// migrate creates the schema on an empty database. An existing database is left alone.
func migrate(ctx context.Context, db *pgxpool.Pool, dir string) error {
	var exists bool
	if err := db.QueryRow(ctx, "SELECT to_regclass('public.tits') IS NOT NULL").Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.sql"))
	num := func(f string) int { n, _ := strconv.Atoi(strings.SplitN(filepath.Base(f), "_", 2)[0]); return n }
	sort.Slice(files, func(i, j int) bool { return num(files[i]) < num(files[j]) })
	for _, f := range files {
		sql, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if _, err := db.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
		log.Printf("applied %s", filepath.Base(f))
	}
	return nil
}

// ensureTables adds the tables of the Go bot. Safe to run every start.
func ensureTables(ctx context.Context, db *pgxpool.Pool) error {
	_, err := db.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS User_Settings (
			uid     bigint PRIMARY KEY,
			pronoun varchar(8) NOT NULL DEFAULT 'she',
			body    varchar(8) NOT NULL DEFAULT 'tits'
		);
		-- Sizes parked while a player grows the other body (see setGender).
		CREATE TABLE IF NOT EXISTS Body_Saves (
			uid     bigint NOT NULL,
			chat_id bigint NOT NULL,
			body    varchar(8) NOT NULL,
			length  bigint NOT NULL,
			PRIMARY KEY (uid, chat_id, body)
		);
		CREATE TABLE IF NOT EXISTS Blocked_Users (
			uid        bigint PRIMARY KEY,
			blocked_at timestamptz NOT NULL DEFAULT now()
		);`)
	return err
}

// ---- users ----

// Gender is what a player picked with /gender. Body: "tits" or "dick". Pron: "she", "he" or "they".
type Gender struct {
	Pron string `json:"pron"`
	Body string `json:"body"`
}

var defaultGender = Gender{Pron: "she", Body: "tits"}

func validGender(g Gender) bool {
	return (g.Pron == "she" || g.Pron == "he" || g.Pron == "they") && (g.Body == "tits" || g.Body == "dick")
}

func getGender(ctx context.Context, db *pgxpool.Pool, uid int64) Gender {
	g := defaultGender
	_ = db.QueryRow(ctx, "SELECT pronoun, body FROM User_Settings WHERE uid = $1", uid).Scan(&g.Pron, &g.Body)
	return g
}

// setGender saves the choice. Switching tits <-> dick keeps separate progress: the current sizes
// of the old body are parked in Body_Saves and the new body's parked sizes come back (or 0).
func setGender(ctx context.Context, db *pgxpool.Pool, uid int64, g Gender) error {
	old := getGender(ctx, db, uid)
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if old.Body != g.Body {
		steps := []struct {
			sql  string
			body string
		}{
			{`INSERT INTO Body_Saves(uid, chat_id, body, length) SELECT uid, chat_id, $2, length FROM Tits WHERE uid = $1
				ON CONFLICT (uid, chat_id, body) DO UPDATE SET length = EXCLUDED.length`, old.Body},
			{`UPDATE Tits t SET bonus_attempts = bonus_attempts + 1, length = coalesce((SELECT b.length FROM Body_Saves b
				WHERE b.uid = t.uid AND b.chat_id = t.chat_id AND b.body = $2), 0) WHERE t.uid = $1`, g.Body},
			{`DELETE FROM Body_Saves WHERE uid = $1 AND body = $2`, g.Body},
		}
		for _, st := range steps {
			if _, err := tx.Exec(ctx, st.sql, uid, st.body); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO User_Settings(uid, pronoun, body) VALUES ($1, $2, $3)
		ON CONFLICT (uid) DO UPDATE SET pronoun = $2, body = $3`, uid, g.Pron, g.Body); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func isBlocked(ctx context.Context, db *pgxpool.Pool, uid int64) bool {
	var b bool
	_ = db.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM Blocked_Users WHERE uid = $1)", uid).Scan(&b)
	return b
}

func setBlocked(ctx context.Context, db *pgxpool.Pool, uid int64, blocked bool) error {
	var err error
	if blocked {
		_, err = db.Exec(ctx, "INSERT INTO Blocked_Users(uid) VALUES ($1) ON CONFLICT DO NOTHING", uid)
	} else {
		_, err = db.Exec(ctx, "DELETE FROM Blocked_Users WHERE uid = $1", uid)
	}
	return err
}

// saveUser creates or renames a user and returns when they registered.
func saveUser(ctx context.Context, db *pgxpool.Pool, uid int64, name string) (time.Time, error) {
	var created time.Time
	err := db.QueryRow(ctx, `INSERT INTO Users(uid, name) VALUES ($1, $2)
		ON CONFLICT (uid) DO UPDATE SET name = $2 RETURNING created_at`, uid, name).Scan(&created)
	return created, err
}

func userName(ctx context.Context, db *pgxpool.Pool, uid int64) (string, error) {
	var name string
	err := db.QueryRow(ctx, "SELECT name FROM Users WHERE uid = $1", uid).Scan(&name)
	return name, err
}

// deleteUser removes a user and everything they have, in one transaction.
func deleteUser(ctx context.Context, db *pgxpool.Pool, uid int64) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, q := range []string{
		"DELETE FROM Titties_of_Day WHERE winner_uid = $1",
		"DELETE FROM Stale_Tit_Shrinks WHERE uid = $1",
		"DELETE FROM Loans WHERE uid = $1",
		"DELETE FROM Promo_Code_Activations WHERE uid = $1",
		"DELETE FROM Imports WHERE uid = $1",
		"DELETE FROM Battle_Stats WHERE uid = $1",
		"DELETE FROM Tits WHERE uid = $1",
		"DELETE FROM User_Settings WHERE uid = $1",
		"DELETE FROM Body_Saves WHERE uid = $1",
		"DELETE FROM Users WHERE uid = $1",
	} {
		if _, err := tx.Exec(ctx, q, uid); err != nil && pgCode(err) != "42P01" { // 42P01: table missing
			return fmt.Errorf("%s: %w", q, err)
		}
	}
	return tx.Commit(ctx)
}

// ---- chats ----

// chatByTG returns the internal id of a Telegram chat and creates the row if needed.
func chatByTG(ctx context.Context, db *pgxpool.Pool, tgChat int64) (int64, error) {
	var id int64
	err := db.QueryRow(ctx, `INSERT INTO Chats(chat_id) VALUES ($1)
		ON CONFLICT (chat_id) DO UPDATE SET chat_id = EXCLUDED.chat_id RETURNING id`, tgChat).Scan(&id)
	return id, err
}

// chatByInstance finds a chat by the chat_instance of an inline message, or creates it.
func chatByInstance(ctx context.Context, db *pgxpool.Pool, inst string) (int64, error) {
	var id int64
	err := db.QueryRow(ctx, `INSERT INTO Chats(chat_instance) VALUES ($1)
		ON CONFLICT (chat_instance) DO UPDATE SET chat_instance = EXCLUDED.chat_instance RETURNING id`, inst).Scan(&id)
	return id, err
}

// linkInstance remembers that a chat_instance belongs to a group, so inline play there
// counts towards the group. It does nothing if that instance already has its own row.
func linkInstance(ctx context.Context, db *pgxpool.Pool, tgChat int64, inst string) {
	if inst == "" {
		return
	}
	_, err := db.Exec(ctx, `UPDATE Chats SET chat_instance = $2 WHERE chat_id = $1 AND chat_instance IS NULL
		AND NOT EXISTS (SELECT 1 FROM Chats WHERE chat_instance = $2)`, tgChat, inst)
	if err != nil {
		log.Printf("link chat instance: %v", err)
	}
}

func setChatTitle(ctx context.Context, db *pgxpool.Pool, chat int64, title string) {
	if title == "" {
		return
	}
	_, _ = db.Exec(ctx, `UPDATE Chats SET settings = jsonb_set(settings, '{title}', to_jsonb($2::text))
		WHERE id = $1 AND settings->>'title' IS DISTINCT FROM $2`, chat, title)
}

func chatLanguage(ctx context.Context, db *pgxpool.Pool, chat int64) string {
	var lang *string
	_ = db.QueryRow(ctx, "SELECT settings->>'language' FROM Chats WHERE id = $1", chat).Scan(&lang)
	if lang == nil {
		return ""
	}
	return *lang
}

// setChatLanguage stores the chat language; "" clears it.
func setChatLanguage(ctx context.Context, db *pgxpool.Pool, chat int64, lang string) error {
	var err error
	if lang == "" {
		_, err = db.Exec(ctx, "UPDATE Chats SET settings = settings - 'language' WHERE id = $1", chat)
	} else {
		_, err = db.Exec(ctx, "UPDATE Chats SET settings = jsonb_set(settings, '{language}', to_jsonb($2::text)) WHERE id = $1", chat, lang)
	}
	return err
}

// migrateChat follows a group that became a supergroup and got a new id.
func migrateChat(ctx context.Context, db *pgxpool.Pool, oldID, newID int64) error {
	tag, err := db.Exec(ctx, `UPDATE Chats SET chat_id = $2 WHERE chat_id = $1
		AND NOT EXISTS (SELECT 1 FROM Chats WHERE chat_id = $2)`, oldID, newID)
	if err == nil && tag.RowsAffected() == 1 {
		_, err = db.Exec(ctx, `INSERT INTO Chat_Migrations(internal_id, old_chat_id, new_chat_id)
			SELECT id, $1, $2 FROM Chats WHERE chat_id = $2 ON CONFLICT DO NOTHING`, oldID, newID)
	}
	return err
}

// ---- lengths and top ----

type Player struct {
	UID      int64     `json:"uid"`
	Name     string    `json:"name"`
	Length   int64     `json:"length"`
	GrownAt  time.Time `json:"grown_at"`
	Position int64     `json:"pos"`
	Gender   Gender    `json:"gender"`
}

// ranked is every unblocked player of a chat with their place in the top.
const ranked = `SELECT t.uid, u.name, t.length, t.updated_at,
		ROW_NUMBER() OVER (ORDER BY t.length DESC, t.updated_at DESC, u.name) AS position,
		coalesce(s.pronoun, 'she') AS pronoun, coalesce(s.body, 'tits') AS body
	FROM Tits t JOIN Users u USING (uid) LEFT JOIN User_Settings s USING (uid)
	WHERE t.chat_id = $1 AND t.` + notBlocked

func scanPlayer(r pgx.Row) (Player, error) {
	var p Player
	err := r.Scan(&p.UID, &p.Name, &p.Length, &p.GrownAt, &p.Position, &p.Gender.Pron, &p.Gender.Body)
	return p, err
}

// position is the place of a player in the chat top (1 = biggest).
func position(ctx context.Context, q querier, chat, uid int64) (int64, error) {
	var pos int64
	err := q.QueryRow(ctx, "SELECT position FROM ("+ranked+") r WHERE uid = $2", chat, uid).Scan(&pos)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return pos, err
}

func getPlayer(ctx context.Context, db *pgxpool.Pool, chat, uid int64) (*Player, error) {
	p, err := scanPlayer(db.QueryRow(ctx, "SELECT * FROM ("+ranked+") r WHERE uid = $2", chat, uid))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &p, err
}

func getTop(ctx context.Context, db *pgxpool.Pool, cfg Config, chat int64, offset, limit int) ([]Player, error) {
	rows, err := db.Query(ctx, "SELECT * FROM ("+ranked+`) r
		WHERE NOT $4 OR length != 0 OR updated_at > now() - make_interval(days => $5)
		OFFSET $2 LIMIT $3`, chat, offset, limit, cfg.HideInactiveZero, cfg.InactivityDays)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Player, error) { return scanPlayer(r) })
}

// lockLength reads a length and locks the row until the transaction ends. ok=false if no row.
func lockLength(ctx context.Context, tx pgx.Tx, chat, uid int64) (length int64, grownToday bool, ok bool, err error) {
	err = tx.QueryRow(ctx, `SELECT length, date(updated_at) = current_date AND bonus_attempts = 0
		FROM Tits WHERE chat_id = $1 AND uid = $2 FOR UPDATE`, chat, uid).Scan(&length, &grownToday)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, false, nil
	}
	return length, grownToday, err == nil, err
}

// addLength changes a length outside the daily /grow (DoD, PvP, loans, admin).
func addLength(ctx context.Context, tx pgx.Tx, chat, uid, change int64) (int64, error) {
	var l int64
	err := tx.QueryRow(ctx, `UPDATE Tits SET length = length + $3, bonus_attempts = bonus_attempts + 1
		WHERE chat_id = $1 AND uid = $2 RETURNING length`, chat, uid, change).Scan(&l)
	return l, err
}

// ---- loans ----

type Loan struct {
	ID    int64
	Debt  int64
	Ratio float64
}

func activeLoan(ctx context.Context, tx pgx.Tx, chat, uid int64) (*Loan, error) {
	var l Loan
	var ratio float32
	err := tx.QueryRow(ctx, `SELECT id, debt, payout_ratio FROM Loans
		WHERE chat_id = $1 AND uid = $2 AND repaid_at IS NULL`, chat, uid).Scan(&l.ID, &l.Debt, &ratio)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	l.Ratio = float64(ratio)
	return &l, err
}

// payLoan takes ratio*amount (max: the debt) to repay an active loan. Returns what was paid.
func payLoan(ctx context.Context, tx pgx.Tx, chat, uid, amount int64) (int64, error) {
	if amount <= 0 {
		return 0, nil
	}
	loan, err := activeLoan(ctx, tx, chat, uid)
	if err != nil || loan == nil {
		return 0, err
	}
	pay := min(int64(loan.Ratio*float64(amount)+0.5), loan.Debt)
	if pay <= 0 {
		return 0, nil
	}
	_, err = tx.Exec(ctx, "UPDATE Loans SET debt = debt - $2 WHERE id = $1", loan.ID, pay)
	return pay, err
}

// ---- PvP stats ----

type Stats struct {
	Total, Won, Streak, StreakMax int64
	Acquired, Lost                int64
}

func (s Stats) WinRate() string {
	if s.Total == 0 {
		return "0.00%"
	}
	return fmt.Sprintf("%.2f%%", float64(s.Won)*100/float64(s.Total))
}

const statsCols = "battles_total, battles_won, win_streak_current, win_streak_max, acquired_length, lost_length"

func scanStats(row pgx.Row) (Stats, error) {
	var s Stats
	var streak, streakMax int16
	err := row.Scan(&s.Total, &s.Won, &streak, &streakMax, &s.Acquired, &s.Lost)
	s.Streak, s.StreakMax = int64(streak), int64(streakMax)
	if errors.Is(err, pgx.ErrNoRows) {
		return Stats{}, nil
	}
	return s, err
}

func getStats(ctx context.Context, db *pgxpool.Pool, chat, uid int64) (Stats, error) {
	return scanStats(db.QueryRow(ctx, "SELECT "+statsCols+" FROM Battle_Stats WHERE chat_id = $1 AND uid = $2", chat, uid))
}

// saveBattle records a fight. Returns both new stats and the loser's lost win streak.
func saveBattle(ctx context.Context, tx pgx.Tx, chat, winner, loser, bet int64) (w, l Stats, lostStreak int64, err error) {
	w, err = scanStats(tx.QueryRow(ctx, `INSERT INTO Battle_Stats(uid, chat_id, battles_total, battles_won, win_streak_current, acquired_length)
		VALUES ($1, $2, 1, 1, 1, $3) ON CONFLICT (uid, chat_id) DO UPDATE SET
			battles_total = Battle_Stats.battles_total + 1, battles_won = Battle_Stats.battles_won + 1,
			win_streak_current = Battle_Stats.win_streak_current + 1,
			acquired_length = Battle_Stats.acquired_length + $3
		RETURNING `+statsCols, winner, chat, bet))
	if err != nil {
		return
	}
	var prev int16
	err = tx.QueryRow(ctx, "SELECT win_streak_current FROM Battle_Stats WHERE chat_id = $1 AND uid = $2", chat, loser).Scan(&prev)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return
	}
	lostStreak = int64(prev)
	l, err = scanStats(tx.QueryRow(ctx, `INSERT INTO Battle_Stats(uid, chat_id, battles_total, battles_won, win_streak_current, lost_length)
		VALUES ($1, $2, 1, 0, 0, $3) ON CONFLICT (uid, chat_id) DO UPDATE SET
			battles_total = Battle_Stats.battles_total + 1, win_streak_current = 0,
			lost_length = Battle_Stats.lost_length + $3
		RETURNING `+statsCols, loser, chat, bet))
	return
}

// ---- Titties of the Day ----

func todaysDod(ctx context.Context, db *pgxpool.Pool, chat int64) (string, error) {
	var name string
	err := db.QueryRow(ctx, `SELECT u.name FROM Titties_of_Day d JOIN Users u ON u.uid = d.winner_uid
		WHERE d.chat_id = $1 AND d.created_at = current_date`, chat).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return name, err
}

// randomActivePlayer picks a random player who grew recently, skipping the richest part of the chat.
func randomActivePlayer(ctx context.Context, db *pgxpool.Pool, chat int64, inactivityDays int, richExclusion float64) (uid int64, name string, err error) {
	err = db.QueryRow(ctx, `WITH r AS (
			SELECT uid, name, PERCENT_RANK() OVER (ORDER BY length) AS rank
			FROM Tits t JOIN Users USING (uid)
			WHERE t.chat_id = $1 AND t.updated_at > now() - make_interval(days => $2) AND t.`+notBlocked+`
		) SELECT uid, name FROM r WHERE rank <= $3 ORDER BY random() LIMIT 1`,
		chat, inactivityDays, 1-richExclusion).Scan(&uid, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", nil
	}
	return
}

func personalStats(ctx context.Context, db *pgxpool.Pool, uid int64) (chats, maxLen, total int64, err error) {
	err = db.QueryRow(ctx, "SELECT count(*), coalesce(max(length), 0), coalesce(sum(length), 0)::bigint FROM Tits WHERE uid = $1", uid).
		Scan(&chats, &maxLen, &total)
	return
}

// ---- web app ----

// UserChat is one chat a user plays in, for the web app.
type UserChat struct {
	Chat     int64  `json:"chat"`
	Title    string `json:"title"`
	Length   int64  `json:"length"`
	Position int64  `json:"pos"`
	Players  int64  `json:"players"`
}

func userChats(ctx context.Context, db *pgxpool.Pool, uid int64) ([]UserChat, error) {
	rows, err := db.Query(ctx, `SELECT t.chat_id, coalesce(c.settings->>'title', ''), t.length,
			(SELECT count(*) + 1 FROM Tits o WHERE o.chat_id = t.chat_id AND o.length > t.length AND o.`+notBlocked+`),
			(SELECT count(*) FROM Tits o WHERE o.chat_id = t.chat_id AND o.`+notBlocked+`)
		FROM Tits t JOIN Chats c ON c.id = t.chat_id WHERE t.uid = $1 ORDER BY t.length DESC`, uid)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (UserChat, error) {
		var c UserChat
		err := r.Scan(&c.Chat, &c.Title, &c.Length, &c.Position, &c.Players)
		return c, err
	})
}

func playsIn(ctx context.Context, db *pgxpool.Pool, uid, chat int64) bool {
	var ok bool
	_ = db.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM Tits WHERE uid = $1 AND chat_id = $2)", uid, chat).Scan(&ok)
	return ok
}

// AdminUser is a row of the admin user search.
type AdminUser struct {
	UID     int64  `json:"uid"`
	Name    string `json:"name"`
	Blocked bool   `json:"blocked"`
	Chats   int64  `json:"chats"`
	Total   int64  `json:"total"`
	Gender  Gender `json:"gender"`
}

// searchUsers finds users by name part or exact id. An empty query lists the biggest players.
func searchUsers(ctx context.Context, db *pgxpool.Pool, q string) ([]AdminUser, error) {
	id, _ := strconv.ParseInt(strings.TrimPrefix(q, "@"), 10, 64)
	rows, err := db.Query(ctx, `SELECT u.uid, u.name, b.uid IS NOT NULL, count(t.chat_id), coalesce(sum(t.length), 0)::bigint,
			coalesce(s.pronoun, 'she'), coalesce(s.body, 'tits')
		FROM Users u LEFT JOIN Tits t USING (uid) LEFT JOIN Blocked_Users b USING (uid) LEFT JOIN User_Settings s USING (uid)
		WHERE $1 = '' OR u.name ILIKE '%' || $1 || '%' OR u.uid = $2
		GROUP BY u.uid, u.name, b.uid, s.pronoun, s.body
		ORDER BY coalesce(sum(t.length), 0) DESC LIMIT 50`, q, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AdminUser, error) {
		var u AdminUser
		err := r.Scan(&u.UID, &u.Name, &u.Blocked, &u.Chats, &u.Total, &u.Gender.Pron, &u.Gender.Body)
		return u, err
	})
}

// setLength sets a player's length in one chat to an exact value.
func setLength(ctx context.Context, db *pgxpool.Pool, chat, uid, length int64) error {
	tag, err := db.Exec(ctx, `UPDATE Tits SET length = $3, bonus_attempts = bonus_attempts + 1
		WHERE chat_id = $1 AND uid = $2`, chat, uid, length)
	if err == nil && tag.RowsAffected() == 0 {
		return errors.New("this user doesn't play in that chat")
	}
	return err
}

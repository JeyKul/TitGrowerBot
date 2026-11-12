package main

// All SQL lives here. The schema is the old one (see migrations/), so the old database works as is.
//
// Note on the Dicks table: a database trigger refuses a second UPDATE of a row on the same day
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
	errGrownToday = "GD0E1" // raised by the trigger on Dicks
	errDodChosen  = "GD0E2" // raised by the trigger on Dick_of_Day
)

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// migrate creates the schema on an empty database. An existing database is left alone.
func migrate(ctx context.Context, db *pgxpool.Pool, dir string) error {
	var exists bool
	if err := db.QueryRow(ctx, "SELECT to_regclass('public.dicks') IS NOT NULL").Scan(&exists); err != nil {
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

// querier is a pool or a transaction.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Player struct {
	UID      int64
	Name     string
	Length   int64
	GrownAt  time.Time
	Position int64
}

// ---- users and chats ----

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

// chatID returns the internal id of a Telegram chat and creates the row if needed.
func chatID(ctx context.Context, db *pgxpool.Pool, tgChat int64) (int64, error) {
	var id int64
	err := db.QueryRow(ctx, `INSERT INTO Chats(chat_id) VALUES ($1)
		ON CONFLICT (chat_id) DO UPDATE SET chat_id = EXCLUDED.chat_id RETURNING id`, tgChat).Scan(&id)
	return id, err
}

func chatLanguage(ctx context.Context, db *pgxpool.Pool, tgChat int64) string {
	var lang *string
	_ = db.QueryRow(ctx, "SELECT settings->>'language' FROM Chats WHERE chat_id = $1", tgChat).Scan(&lang)
	if lang == nil {
		return ""
	}
	return *lang
}

// setChatLanguage stores the chat language; "" clears it.
func setChatLanguage(ctx context.Context, db *pgxpool.Pool, tgChat int64, lang string) error {
	id, err := chatID(ctx, db, tgChat)
	if err != nil {
		return err
	}
	if lang == "" {
		_, err = db.Exec(ctx, "UPDATE Chats SET settings = settings - 'language' WHERE id = $1", id)
	} else {
		_, err = db.Exec(ctx, "UPDATE Chats SET settings = jsonb_set(settings, '{language}', to_jsonb($2::text)) WHERE id = $1", id, lang)
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

// position is the place of a player in the chat top (1 = biggest).
func position(ctx context.Context, q querier, chat, uid int64) (int64, error) {
	var pos int64
	err := q.QueryRow(ctx, `SELECT position FROM (
			SELECT uid, ROW_NUMBER() OVER (ORDER BY length DESC, updated_at DESC, name) AS position
			FROM Dicks JOIN Users USING (uid) WHERE chat_id = $1
		) t WHERE uid = $2`, chat, uid).Scan(&pos)
	return pos, err
}

func getPlayer(ctx context.Context, db *pgxpool.Pool, tgChat, uid int64) (*Player, error) {
	p := Player{UID: uid}
	err := db.QueryRow(ctx, `SELECT name, length, updated_at, position FROM (
			SELECT uid, name, d.length, d.updated_at,
				ROW_NUMBER() OVER (ORDER BY d.length DESC, d.updated_at DESC, name) AS position
			FROM Dicks d JOIN Users USING (uid) JOIN Chats c ON c.id = d.chat_id
			WHERE c.chat_id = $1
		) t WHERE uid = $2`, tgChat, uid).Scan(&p.Name, &p.Length, &p.GrownAt, &p.Position)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &p, err
}

func getTop(ctx context.Context, db *pgxpool.Pool, cfg Config, tgChat int64, offset, limit int) ([]Player, error) {
	rows, err := db.Query(ctx, `SELECT uid, name, d.length, d.updated_at,
			ROW_NUMBER() OVER (ORDER BY d.length DESC, d.updated_at DESC, name)
		FROM Dicks d JOIN Users USING (uid) JOIN Chats c ON c.id = d.chat_id
		WHERE c.chat_id = $1
		  AND (NOT $4 OR d.length != 0 OR d.updated_at > now() - make_interval(days => $5))
		OFFSET $2 LIMIT $3`, tgChat, offset, limit, cfg.HideInactiveZero, cfg.InactivityDays)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Player, error) {
		var p Player
		err := r.Scan(&p.UID, &p.Name, &p.Length, &p.GrownAt, &p.Position)
		return p, err
	})
}

// lockLength reads a length and locks the row until the transaction ends. ok=false if no row.
func lockLength(ctx context.Context, tx pgx.Tx, chat, uid int64) (length int64, grownToday bool, ok bool, err error) {
	err = tx.QueryRow(ctx, `SELECT length, date(updated_at) = current_date AND bonus_attempts = 0
		FROM Dicks WHERE chat_id = $1 AND uid = $2 FOR UPDATE`, chat, uid).Scan(&length, &grownToday)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, false, nil
	}
	return length, grownToday, err == nil, err
}

// addLength changes a length outside the daily /grow (DoD, PvP, loans).
func addLength(ctx context.Context, tx pgx.Tx, chat, uid, change int64) (int64, error) {
	var l int64
	err := tx.QueryRow(ctx, `UPDATE Dicks SET length = length + $3, bonus_attempts = bonus_attempts + 1
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

func getStats(ctx context.Context, db *pgxpool.Pool, tgChat, uid int64) (Stats, error) {
	return scanStats(db.QueryRow(ctx, "SELECT "+statsCols+` FROM Battle_Stats
		WHERE uid = $2 AND chat_id = (SELECT id FROM Chats WHERE chat_id = $1)`, tgChat, uid))
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

// ---- Dick of the Day ----

func todaysDod(ctx context.Context, db *pgxpool.Pool, chat int64) (string, error) {
	var name string
	err := db.QueryRow(ctx, `SELECT u.name FROM Dick_of_Day d JOIN Users u ON u.uid = d.winner_uid
		WHERE d.chat_id = $1 AND d.created_at = current_date`, chat).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return name, err
}

// randomActivePlayer picks a random player who grew recently, skipping the richest part of the chat.
func randomActivePlayer(ctx context.Context, db *pgxpool.Pool, chat int64, inactivityDays int, richExclusion float64) (uid int64, name string, err error) {
	err = db.QueryRow(ctx, `WITH ranked AS (
			SELECT uid, name, PERCENT_RANK() OVER (ORDER BY length) AS rank
			FROM Dicks d JOIN Users USING (uid)
			WHERE d.chat_id = $1 AND d.updated_at > now() - make_interval(days => $2)
		) SELECT uid, name FROM ranked WHERE rank <= $3 ORDER BY random() LIMIT 1`,
		chat, inactivityDays, 1-richExclusion).Scan(&uid, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", nil
	}
	return
}

func personalStats(ctx context.Context, db *pgxpool.Pool, uid int64) (chats, maxLen, total int64, err error) {
	err = db.QueryRow(ctx, "SELECT count(*), coalesce(max(length), 0), coalesce(sum(length), 0)::bigint FROM Dicks WHERE uid = $1", uid).
		Scan(&chats, &maxLen, &total)
	return
}

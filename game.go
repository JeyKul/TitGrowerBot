package main

import (
	"context"
	"fmt"
	"html"
	"log"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	tg "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/jackc/pgx/v5"
)

const anonymousAdminBot = 1087968824 // @GroupAnonymousBot

// langOf: the chat language if an admin set one, otherwise the user's Telegram language.
func (a *App) langOf(ctx context.Context, chat *tg.Chat, user *tg.User) string {
	if chat != nil && chat.ID < 0 {
		if l := chatLanguage(ctx, a.db, chat.ID); a.texts.Has(l) {
			return l
		}
	}
	if user != nil {
		return a.texts.pickLang(user.LanguageCode)
	}
	return "en"
}

func fullName(u *tg.User) string {
	return strings.TrimSpace(u.FirstName + " " + u.LastName)
}

func esc(s string) string { return html.EscapeString(s) }

func (a *App) reply(m *tg.Message, text string, kb *tg.InlineKeyboardMarkup) {
	msg := tg.NewMessage(m.Chat.ID, text)
	msg.ParseMode = tg.ModeHTML
	msg.ReplyToMessageID = m.MessageID
	msg.AllowSendingWithoutReply = true
	msg.DisableWebPagePreview = true
	if kb != nil {
		msg.ReplyMarkup = kb
	}
	if _, err := a.bot.Send(msg); err != nil {
		log.Printf("send to %d: %v", m.Chat.ID, err)
	}
}

func (a *App) edit(q *tg.CallbackQuery, text string, kb *tg.InlineKeyboardMarkup) {
	e := tg.NewEditMessageText(q.Message.Chat.ID, q.Message.MessageID, text)
	e.ParseMode = tg.ModeHTML
	e.DisableWebPagePreview = true
	e.ReplyMarkup = kb
	if _, err := a.bot.Send(e); err != nil && !strings.Contains(err.Error(), "not modified") {
		log.Printf("edit in %d: %v", q.Message.Chat.ID, err)
	}
}

// answer closes the button spinner; a non-empty text pops up for the user.
func (a *App) answer(q *tg.CallbackQuery, text string) {
	c := tg.NewCallback(q.ID, text)
	c.ShowAlert = text != "" && len(text) > 60
	_, _ = a.bot.Request(c)
}

// timeLeft is the "next attempt in Xh Ym" line. The day changes at midnight of the DB server (UTC).
func (a *App) timeLeft(lang string) string {
	now := time.Now().UTC()
	left := now.Truncate(24 * time.Hour).Add(24 * time.Hour).Sub(now)
	return a.texts.T(lang, "next_day.some", "hours", int(left.Hours()), "minutes", int(left.Minutes())%60)
}

func (a *App) onChatMigrated(ctx context.Context, oldID, newID int64) {
	if err := migrateChat(ctx, a.db, oldID, newID); err != nil {
		log.Printf("migrate chat %d -> %d: %v", oldID, newID, err)
	}
}

// ---------------- commands ----------------

func (a *App) onCommand(ctx context.Context, m *tg.Message) {
	lang := a.langOf(ctx, m.Chat, m.From)
	cmd := strings.ToLower(m.Command())

	switch cmd {
	case "start", "help":
		a.reply(m, a.texts.T(lang, "help.text", "min", a.cfg.GrowthMin, "max", a.cfg.GrowthMax), nil)
		return
	}

	if m.Chat.ID >= 0 {
		a.reply(m, a.texts.T(lang, "errors.not_group_chat"), nil)
		return
	}
	if m.From == nil || m.From.ID == anonymousAdminBot || m.SenderChat != nil {
		a.reply(m, a.texts.T(lang, "errors.group_account"), nil)
		return
	}

	var err error
	switch cmd {
	case "grow":
		err = a.cmdGrow(ctx, m, lang)
	case "top":
		var text string
		var kb *tg.InlineKeyboardMarkup
		text, kb, err = a.topPage(ctx, m.Chat.ID, m.From.ID, lang, 0)
		if err == nil {
			a.reply(m, text, kb)
		}
	case "dod", "dick_of_day":
		err = a.cmdDod(ctx, m, lang)
	case "pvp", "battle", "attack", "fight":
		err = a.cmdPvp(ctx, m, lang)
	case "stats":
		err = a.cmdStats(ctx, m, lang)
	case "loan":
		err = a.cmdLoan(ctx, m, lang)
	case "language":
		err = a.cmdLanguage(ctx, m, lang)
	}
	if err != nil {
		log.Printf("/%s in %d: %v", cmd, m.Chat.ID, err)
	}
}

// baseGrowth is a random non-zero number in [min, max]. positiveChance is the chance it is > 0.
func baseGrowth(lo, hi int64, positiveChance float64) int64 {
	if lo > 0 {
		return lo + rand.Int64N(hi-lo+1)
	}
	if hi > 0 && rand.Float64() < positiveChance {
		return 1 + rand.Int64N(hi)
	}
	if lo < 0 {
		return lo + rand.Int64N(-lo) // lo..-1
	}
	return 0
}

// perks adds the bonuses to a base change. It runs inside tx and may repay a loan.
// Returns the total change and the text block listing the perks.
func (a *App) perks(ctx context.Context, tx pgx.Tx, chat, uid, length, base int64, lang string) (int64, string, error) {
	var lines []string
	total := base
	if length < 0 && a.cfg.HelpPussiesCoef > 0 {
		if help := int64(a.cfg.HelpPussiesCoef*float64(-length) + 0.5); help != 0 {
			total += help
			lines = append(lines, fmt.Sprintf("— %s (%+d)", a.texts.T(lang, "perks.help-pussies"), help))
		}
	}
	paid, err := payLoan(ctx, tx, chat, uid, base)
	if err != nil {
		return 0, "", err
	}
	if paid > 0 {
		total -= paid
		lines = append(lines, fmt.Sprintf("— %s (%+d)", a.texts.T(lang, "perks.loan-payout"), -paid))
	}
	if len(lines) == 0 {
		return total, "", nil
	}
	return total, "\n\n" + a.texts.T(lang, "perks.top_line") + ":\n" + strings.Join(lines, "\n"), nil
}

func (a *App) cmdGrow(ctx context.Context, m *tg.Message, lang string) error {
	registered, err := saveUser(ctx, a.db, m.From.ID, fullName(m.From))
	if err != nil {
		return err
	}
	chat, err := chatID(ctx, a.db, m.Chat.ID)
	if err != nil {
		return err
	}

	tx, err := a.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	length, grownToday, _, err := lockLength(ctx, tx, chat, m.From.ID)
	if err != nil {
		return err
	}
	if grownToday {
		a.reply(m, a.texts.T(lang, "grow.tomorrow")+a.timeLeft(lang), nil)
		return nil
	}

	chance := a.cfg.GrowShrinkRatio
	if time.Since(registered) <= time.Duration(a.cfg.NewcomersGrace)*24*time.Hour {
		chance = 1 // newcomers only grow
	}
	base := baseGrowth(a.cfg.GrowthMin, a.cfg.GrowthMax, chance)
	change, perksText, err := a.perks(ctx, tx, chat, m.From.ID, length, base, lang)
	if err != nil {
		return err
	}

	var newLength int64
	err = tx.QueryRow(ctx, `INSERT INTO Tits(uid, chat_id, length, updated_at) VALUES ($1, $2, $3, now())
		ON CONFLICT (uid, chat_id) DO UPDATE SET length = Tits.length + $3, updated_at = now()
		RETURNING length`, m.From.ID, chat, change).Scan(&newLength)
	if pgCode(err) == errGrownToday {
		a.reply(m, a.texts.T(lang, "grow.tomorrow")+a.timeLeft(lang), nil)
		return nil
	}
	if err != nil {
		return err
	}
	pos, err := position(ctx, tx, chat, m.From.ID)
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	event := "grow.direction.grown"
	if change < 0 {
		event = "grow.direction.shrunk"
	}
	abs := change
	if abs < 0 {
		abs = -abs
	}
	text := a.texts.T(lang, "grow.result", "event", a.texts.T(lang, event), "incr", abs, "length", newLength) +
		"\n" + a.texts.T(lang, "grow.position", "pos", pos) + perksText + a.timeLeft(lang)
	a.reply(m, text, nil)
	return nil
}

func (a *App) topPage(ctx context.Context, tgChat, viewer int64, lang string, page int) (string, *tg.InlineKeyboardMarkup, error) {
	limit := a.cfg.TopLimit
	players, err := getTop(ctx, a.db, a.cfg, tgChat, page*limit, limit+1)
	if err != nil {
		return "", nil, err
	}
	if len(players) == 0 {
		return a.texts.T(lang, "top.empty"), nil, nil
	}
	more := len(players) > limit
	if more {
		players = players[:limit]
	}

	now := time.Now().UTC()
	today := now.Truncate(24 * time.Hour)
	anyInactive := false
	var lines []string
	for _, p := range players {
		name := esc(p.Name)
		if p.UID == viewer {
			name = "<u>" + name + "</u>"
		}
		line := a.texts.T(lang, "top.line", "n", p.Position, "name", name, "length", p.Length)
		switch {
		case now.Sub(p.GrownAt) > time.Duration(a.cfg.InactivityDays)*24*time.Hour:
			anyInactive = true
			line += " [~]"
		case p.GrownAt.UTC().Before(today):
			line += " [+]"
		}
		lines = append(lines, line)
	}
	text := a.texts.T(lang, "top.title") + "\n\n" + strings.Join(lines, "\n") + "\n\n" + a.texts.T(lang, "top.ending")
	if anyInactive {
		text += "\n" + a.texts.T(lang, "top.ending_inactive", "days", a.cfg.InactivityDays)
	}

	var row []tg.InlineKeyboardButton
	if page > 0 {
		row = append(row, tg.NewInlineKeyboardButtonData("⬅️", fmt.Sprintf("top:page:%d", page-1)))
	}
	if more {
		row = append(row, tg.NewInlineKeyboardButtonData("➡️", fmt.Sprintf("top:page:%d", page+1)))
	}
	if len(row) == 0 {
		return text, nil, nil
	}
	kb := tg.NewInlineKeyboardMarkup(row)
	return text, &kb, nil
}

func (a *App) cmdDod(ctx context.Context, m *tg.Message, lang string) error {
	chat, err := chatID(ctx, a.db, m.Chat.ID)
	if err != nil {
		return err
	}
	if name, err := todaysDod(ctx, a.db, chat); err != nil {
		return err
	} else if name != "" {
		a.reply(m, a.texts.T(lang, "dod.already_chosen", "name", esc(name))+a.timeLeft(lang), nil)
		return nil
	}

	uid, name, err := randomActivePlayer(ctx, a.db, chat, a.cfg.InactivityDays, a.cfg.DodRichExclusion)
	if err != nil {
		return err
	}
	if uid == 0 {
		a.reply(m, a.texts.T(lang, "dod.no_candidates"), nil)
		return nil
	}

	tx, err := a.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	length, _, _, err := lockLength(ctx, tx, chat, uid)
	if err != nil {
		return err
	}
	base := 1 + rand.Int64N(max(a.cfg.DodBonusMax, 1))
	change, perksText, err := a.perks(ctx, tx, chat, uid, length, base, lang)
	if err != nil {
		return err
	}
	newLength, err := addLength(ctx, tx, chat, uid, change)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "INSERT INTO Titties_of_Day(chat_id, winner_uid) VALUES ($1, $2)", chat, uid)
	if pgCode(err) == errDodChosen {
		// someone else ran /dod at the same moment
		winner, _ := todaysDod(ctx, a.db, chat)
		a.reply(m, a.texts.T(lang, "dod.already_chosen", "name", esc(winner))+a.timeLeft(lang), nil)
		return nil
	}
	if err != nil {
		return err
	}
	pos, err := position(ctx, tx, chat, uid)
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	text := a.texts.T(lang, "dod.result", "uid", uid, "name", esc(name), "growth", change, "length", newLength) +
		"\n" + a.texts.T(lang, "dod.position", "pos", pos) + perksText + a.timeLeft(lang)
	a.reply(m, text, nil)
	return nil
}

func (a *App) cmdPvp(ctx context.Context, m *tg.Message, lang string) error {
	bet, err := strconv.ParseInt(strings.TrimSpace(m.CommandArguments()), 10, 32)
	if err != nil || bet <= 0 {
		a.reply(m, a.texts.T(lang, "pvp.errors.no_args"), nil)
		return nil
	}
	p, err := getPlayer(ctx, a.db, m.Chat.ID, m.From.ID)
	if err != nil {
		return err
	}
	if p == nil || p.Length < bet {
		a.reply(m, a.texts.T(lang, "pvp.errors.not_enough.initiator"), nil)
		return nil
	}
	// Same button format as the old bot, so old buttons keep working.
	data := fmt.Sprintf("pvp:%d:%d:%d", m.From.ID, bet, time.Now().UnixMilli()-1719014400000)
	kb := tg.NewInlineKeyboardMarkup(tg.NewInlineKeyboardRow(tg.NewInlineKeyboardButtonData(a.texts.T(lang, "pvp.button"), data)))
	a.reply(m, a.texts.T(lang, "pvp.results.start", "name", esc(fullName(m.From)), "bet", bet), &kb)
	return nil
}

func (a *App) cmdStats(ctx context.Context, m *tg.Message, lang string) error {
	var parts []string
	p, err := getPlayer(ctx, a.db, m.Chat.ID, m.From.ID)
	if err != nil {
		return err
	}
	if p != nil {
		parts = append(parts, a.texts.T(lang, "stats.length", "length", p.Length, "pos", p.Position))
	}
	s, err := getStats(ctx, a.db, m.Chat.ID, m.From.ID)
	if err != nil {
		return err
	}
	parts = append(parts, a.texts.T(lang, "stats.pvp", "win_rate", s.WinRate(), "battles", s.Total, "wins", s.Won,
		"win_streak", s.StreakMax, "acquired", s.Acquired, "lost", s.Lost))
	chats, maxLen, total, err := personalStats(ctx, a.db, m.From.ID)
	if err != nil {
		return err
	}
	parts = append(parts, a.texts.T(lang, "stats.personal", "chats", chats, "max_length", maxLen, "total_length", total))
	parts = append(parts, "<i>"+a.texts.T(lang, "stats.notice")+"</i>")
	a.reply(m, strings.Join(parts, "\n\n"), nil)
	return nil
}

func (a *App) cmdLoan(ctx context.Context, m *tg.Message, lang string) error {
	chat, err := chatID(ctx, a.db, m.Chat.ID)
	if err != nil {
		return err
	}
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if loan, err := activeLoan(ctx, tx, chat, m.From.ID); err != nil {
		return err
	} else if loan != nil {
		a.reply(m, a.texts.T(lang, "loan.debt", "debt", loan.Debt), nil)
		return nil
	}
	if a.cfg.LoanPayoutCoef <= 0 || a.cfg.LoanPayoutCoef >= 1 {
		return nil
	}
	p, err := getPlayer(ctx, a.db, m.Chat.ID, m.From.ID)
	if err != nil {
		return err
	}
	if p == nil || p.Length >= 0 {
		a.reply(m, a.texts.T(lang, "loan.errors.positive_length"), nil)
		return nil
	}
	debt := -p.Length
	ratio := strconv.FormatFloat(a.cfg.LoanPayoutCoef, 'f', -1, 32)
	kb := tg.NewInlineKeyboardMarkup(tg.NewInlineKeyboardRow(
		tg.NewInlineKeyboardButtonData(a.texts.T(lang, "loan.confirmation.buttons.agree"), fmt.Sprintf("loan:%d:confirmed:%d:%s", m.From.ID, debt, ratio)),
		tg.NewInlineKeyboardButtonData(a.texts.T(lang, "loan.confirmation.buttons.disagree"), fmt.Sprintf("loan:%d:refused", m.From.ID)),
	))
	pct := fmt.Sprintf("%.2f%%", a.cfg.LoanPayoutCoef*100)
	a.reply(m, a.texts.T(lang, "loan.confirmation.text", "payout_percentage", pct, "debt", debt), &kb)
	return nil
}

// langNames are the buttons of /language, in this order.
var langNames = [][2]string{{"en", "🇬🇧 English"}, {"ru", "🇷🇺 Русский"}, {"it", "🇮🇹 Italiano"},
	{"fa", "🇮🇷 فارسی"}, {"zh", "🇨🇳 简体中文"}, {"zh-TW", "🇹🇼 繁體中文"}}

func (a *App) isAdmin(chatID, uid int64) bool {
	member, err := a.bot.GetChatMember(tg.GetChatMemberConfig{ChatConfigWithUser: tg.ChatConfigWithUser{ChatID: chatID, UserID: uid}})
	return err == nil && (member.IsCreator() || member.IsAdministrator())
}

func (a *App) cmdLanguage(ctx context.Context, m *tg.Message, lang string) error {
	if !a.isAdmin(m.Chat.ID, m.From.ID) {
		a.reply(m, a.texts.T(lang, "language.errors.admins_only"), nil)
		return nil
	}
	var rows [][]tg.InlineKeyboardButton
	var row []tg.InlineKeyboardButton
	for _, l := range langNames {
		if !a.texts.Has(l[0]) {
			continue
		}
		row = append(row, tg.NewInlineKeyboardButtonData(l[1], "lang:"+l[0]))
		if len(row) == 2 {
			rows = append(rows, row)
			row = nil
		}
	}
	if row != nil {
		rows = append(rows, row)
	}
	rows = append(rows, tg.NewInlineKeyboardRow(tg.NewInlineKeyboardButtonData(a.texts.T(lang, "language.chat.auto_button"), "lang:auto")))
	kb := tg.NewInlineKeyboardMarkup(rows...)
	a.reply(m, a.texts.T(lang, "language.chat.prompt"), &kb)
	return nil
}

// ---------------- buttons ----------------

func (a *App) onButton(ctx context.Context, q *tg.CallbackQuery) {
	if q.Message == nil {
		a.answer(q, "")
		return
	}
	lang := a.langOf(ctx, q.Message.Chat, q.From)
	parts := strings.Split(q.Data, ":")
	var err error
	switch parts[0] {
	case "top":
		page, _ := strconv.Atoi(parts[len(parts)-1])
		var text string
		var kb *tg.InlineKeyboardMarkup
		if text, kb, err = a.topPage(ctx, q.Message.Chat.ID, q.From.ID, lang, max(page, 0)); err == nil {
			a.edit(q, text, kb)
		}
		a.answer(q, "")
	case "pvp":
		err = a.btnPvp(ctx, q, parts, lang)
	case "loan":
		err = a.btnLoan(ctx, q, parts, lang)
	case "lang":
		err = a.btnLanguage(ctx, q, parts)
	default:
		a.answer(q, "")
	}
	if err != nil {
		log.Printf("button %q in %d: %v", q.Data, q.Message.Chat.ID, err)
	}
}

func (a *App) btnPvp(ctx context.Context, q *tg.CallbackQuery, parts []string, lang string) error {
	if len(parts) < 3 {
		a.answer(q, "")
		return nil
	}
	initiator, _ := strconv.ParseInt(parts[1], 10, 64)
	bet, _ := strconv.ParseInt(parts[2], 10, 64)
	if initiator == q.From.ID {
		a.answer(q, a.texts.T(lang, "pvp.errors.same_person"))
		return nil
	}
	key := fmt.Sprintf("%d:%d", q.Message.Chat.ID, q.Message.MessageID)
	if _, busy := a.locks.LoadOrStore(key, true); busy {
		a.answer(q, a.texts.T(lang, "pvp.errors.battle_already_in_progress"))
		return nil
	}
	defer a.locks.Delete(key)

	if _, err := saveUser(ctx, a.db, q.From.ID, fullName(q.From)); err != nil {
		return err
	}
	chat, err := chatID(ctx, a.db, q.Message.Chat.ID)
	if err != nil {
		return err
	}
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// lock both rows in a fixed order so two fights can't deadlock
	first, second := initiator, q.From.ID
	if first > second {
		first, second = second, first
	}
	l1, _, ok1, err := lockLength(ctx, tx, chat, first)
	if err != nil {
		return err
	}
	l2, _, ok2, err := lockLength(ctx, tx, chat, second)
	if err != nil {
		return err
	}
	initLen, initOK, accLen, accOK := l1, ok1, l2, ok2
	if first != initiator {
		initLen, initOK, accLen, accOK = l2, ok2, l1, ok1
	}
	needAcc := int64(0)
	if a.cfg.PvpCheckAcceptor {
		needAcc = bet
	}
	if !accOK || accLen < needAcc {
		a.answer(q, a.texts.T(lang, "pvp.errors.not_enough.acceptor"))
		return nil
	}
	a.answer(q, "")
	if !initOK || initLen < bet {
		a.edit(q, a.texts.T(lang, "pvp.errors.not_enough.initiator"), nil)
		return nil
	}

	winner, loser := initiator, q.From.ID
	if rand.IntN(2) == 0 {
		winner, loser = loser, winner
	}
	loserLen, err := addLength(ctx, tx, chat, loser, -bet)
	if err != nil {
		return err
	}
	winnerLen, err := addLength(ctx, tx, chat, winner, bet)
	if err != nil {
		return err
	}
	ws, ls, lostStreak, err := saveBattle(ctx, tx, chat, winner, loser, bet)
	if err != nil {
		return err
	}
	withheld, err := payLoan(ctx, tx, chat, winner, bet)
	if err != nil {
		return err
	}
	if withheld > 0 {
		if winnerLen, err = addLength(ctx, tx, chat, winner, -withheld); err != nil {
			return err
		}
	}
	winPos, err := position(ctx, tx, chat, winner)
	if err != nil {
		return err
	}
	losePos, err := position(ctx, tx, chat, loser)
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	winName, _ := userName(ctx, a.db, winner)
	loseName, _ := userName(ctx, a.db, loser)
	text := a.texts.T(lang, "pvp.results.finish", "winner_name", esc(winName), "winner_length", winnerLen,
		"loser_name", esc(loseName), "loser_length", loserLen, "bet", bet) +
		"\n\n" + a.texts.T(lang, "pvp.results.position.winner", "name", esc(winName), "pos", winPos) +
		"\n" + a.texts.T(lang, "pvp.results.position.loser", "name", esc(loseName), "pos", losePos)
	if withheld > 0 {
		text += "\n\n" + a.texts.T(lang, "pvp.results.withheld", "payout", withheld)
	}
	if a.cfg.PvpShowStats {
		text += "\n\n" + a.texts.T(lang, "pvp.results.stats.text", "winner_win_rate", ws.WinRate(), "loser_win_rate", ls.WinRate(),
			"winner_win_streak", ws.Streak, "winner_win_streak_max", ws.StreakMax)
		if lostStreak > 1 {
			text += "\n" + a.texts.T(lang, "pvp.results.stats.lost_win_streak", "lost_win_streak", lostStreak)
		}
	}
	a.edit(q, text, nil)
	return nil
}

func (a *App) btnLoan(ctx context.Context, q *tg.CallbackQuery, parts []string, lang string) error {
	if len(parts) < 3 {
		a.answer(q, "")
		return nil
	}
	if uid, _ := strconv.ParseInt(parts[1], 10, 64); uid != q.From.ID {
		a.answer(q, a.texts.T(lang, "errors.another_user"))
		return nil
	}
	a.answer(q, "")
	if parts[2] != "confirmed" {
		a.edit(q, a.texts.T(lang, "loan.callback.refused"), nil)
		return nil
	}
	if len(parts) >= 5 {
		if r, _ := strconv.ParseFloat(parts[4], 64); float32(r) != float32(a.cfg.LoanPayoutCoef) {
			a.edit(q, a.texts.T(lang, "loan.callback.payout_ratio_changed"), nil)
			return nil
		}
	}

	chat, err := chatID(ctx, a.db, q.Message.Chat.ID)
	if err != nil {
		return err
	}
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	length, _, ok, err := lockLength(ctx, tx, chat, q.From.ID)
	if err != nil {
		return err
	}
	if !ok || length >= 0 {
		a.edit(q, a.texts.T(lang, "loan.errors.positive_length"), nil)
		return nil
	}
	debt := -length // the loan resets the length to zero, so it takes the length as it is now
	loan, err := activeLoan(ctx, tx, chat, q.From.ID)
	if err != nil {
		return err
	}
	if loan != nil {
		_, err = tx.Exec(ctx, "UPDATE Loans SET debt = debt + $2, payout_ratio = $3 WHERE id = $1", loan.ID, debt, a.cfg.LoanPayoutCoef)
	} else {
		_, err = tx.Exec(ctx, "INSERT INTO Loans(chat_id, uid, debt, payout_ratio) VALUES ($1, $2, $3, $4)", chat, q.From.ID, debt, a.cfg.LoanPayoutCoef)
	}
	if err != nil {
		return err
	}
	if _, err := addLength(ctx, tx, chat, q.From.ID, debt); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	a.edit(q, a.texts.T(lang, "loan.callback.success"), nil)
	return nil
}

func (a *App) btnLanguage(ctx context.Context, q *tg.CallbackQuery, parts []string) error {
	lang := a.langOf(ctx, q.Message.Chat, q.From)
	if !a.isAdmin(q.Message.Chat.ID, q.From.ID) {
		a.answer(q, a.texts.T(lang, "language.errors.admins_only"))
		return nil
	}
	a.answer(q, "")
	choice := parts[len(parts)-1]
	if choice == "auto" {
		if err := setChatLanguage(ctx, a.db, q.Message.Chat.ID, ""); err != nil {
			return err
		}
		a.edit(q, a.texts.T(a.texts.pickLang(q.From.LanguageCode), "language.chat.reset"), nil)
		return nil
	}
	if !a.texts.Has(choice) {
		return nil
	}
	if err := setChatLanguage(ctx, a.db, q.Message.Chat.ID, choice); err != nil {
		return err
	}
	a.edit(q, a.texts.T(choice, "language.chat.success"), nil)
	return nil
}

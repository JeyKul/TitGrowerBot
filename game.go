package main

import (
	"context"
	"encoding/json"
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

// Act is one player doing something in one chat. It works the same for a group
// command and for a button under an inline message.
type Act struct {
	Chat int64 // internal chat id
	User *tg.User
	Lang string
	G    Gender // the acting player's gender
}

// Reply is what an action shows: text and maybe buttons.
type Reply struct {
	Text string
	KB   *tg.InlineKeyboardMarkup
}

func text(s string) Reply { return Reply{Text: s} }

func fullName(u *tg.User) string {
	return strings.TrimSpace(u.FirstName + " " + u.LastName)
}

func esc(s string) string { return html.EscapeString(s) }

func kb(rows ...[]tg.InlineKeyboardButton) *tg.InlineKeyboardMarkup {
	m := tg.NewInlineKeyboardMarkup(rows...)
	return &m
}

// newAct loads language and gender for a user in a chat.
func (a *App) newAct(ctx context.Context, chat int64, user *tg.User) Act {
	lang := chatLanguage(ctx, a.db, chat)
	if !a.texts.Has(lang) {
		lang = a.texts.pickLang(user.LanguageCode)
	}
	return Act{Chat: chat, User: user, Lang: lang, G: getGender(ctx, a.db, user.ID)}
}

// t is a text that doesn't depend on anyone's gender.
func (a *App) t(act Act, key string, args ...any) string { return a.texts.T(act.Lang, key, args...) }

// me is a text about the acting player, picked by their gender.
func (a *App) me(act Act, key string, args ...any) string {
	return a.texts.G(act.Lang, act.G, key, args...)
}

// ---------------- sending ----------------

func (a *App) reply(m *tg.Message, r Reply) {
	msg := tg.NewMessage(m.Chat.ID, r.Text)
	msg.ParseMode = tg.ModeHTML
	msg.ReplyToMessageID = m.MessageID
	msg.AllowSendingWithoutReply = true
	msg.DisableWebPagePreview = true
	if r.KB != nil {
		msg.ReplyMarkup = r.KB
	}
	if _, err := a.bot.Send(msg); err != nil {
		log.Printf("send to %d: %v", m.Chat.ID, err)
	}
}

// edit changes the message a button belongs to: a normal one or an inline one.
func (a *App) edit(q *tg.CallbackQuery, r Reply) {
	var e tg.EditMessageTextConfig
	if q.Message != nil {
		e = tg.NewEditMessageText(q.Message.Chat.ID, q.Message.MessageID, r.Text)
	} else {
		e = tg.EditMessageTextConfig{BaseEdit: tg.BaseEdit{InlineMessageID: q.InlineMessageID}, Text: r.Text}
	}
	e.ParseMode = tg.ModeHTML
	e.DisableWebPagePreview = true
	e.ReplyMarkup = r.KB
	if _, err := a.bot.Send(e); err != nil && !strings.Contains(err.Error(), "not modified") {
		log.Printf("edit: %v", err)
	}
}

// answer closes the button spinner; a non-empty text pops up for the user.
func (a *App) answer(q *tg.CallbackQuery, text string) {
	c := tg.NewCallback(q.ID, text)
	c.ShowAlert = len(text) > 60
	_, _ = a.bot.Request(c)
}

// timeLeft is the "next attempt in Xh Ym" line. The day changes at midnight UTC.
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
	cmd := strings.ToLower(m.Command())
	lang := "en"
	if m.From != nil {
		lang = a.texts.pickLang(m.From.LanguageCode)
	}

	// commands that also work in a private chat
	switch cmd {
	case "start":
		if m.CommandArguments() == "app" && m.Chat.ID > 0 {
			a.sendAppButton(m.Chat.ID, lang)
			return
		}
		a.reply(m, text(a.texts.T(lang, "help.text", "min", a.cfg.GrowthMin, "max", a.cfg.GrowthMax)))
		return
	case "help":
		a.reply(m, text(a.texts.T(lang, "help.text", "min", a.cfg.GrowthMin, "max", a.cfg.GrowthMax)))
		return
	case "gender":
		if m.From != nil {
			a.reply(m, a.genderMenu(Act{User: m.From, Lang: lang, G: getGender(ctx, a.db, m.From.ID)}))
		}
		return
	case "app":
		a.cmdApp(m, lang)
		return
	}

	if m.Chat.ID >= 0 {
		a.reply(m, text(a.texts.T(lang, "errors.not_group_chat")))
		return
	}
	if m.From == nil || m.From.ID == anonymousAdminBot || m.SenderChat != nil {
		a.reply(m, text(a.texts.T(lang, "errors.group_account")))
		return
	}

	chat, err := chatByTG(ctx, a.db, m.Chat.ID)
	if err != nil {
		log.Printf("chat %d: %v", m.Chat.ID, err)
		return
	}
	setChatTitle(ctx, a.db, chat, m.Chat.Title)
	act := a.newAct(ctx, chat, m.From)

	var r Reply
	switch cmd {
	case "grow":
		r, err = a.grow(ctx, act)
	case "top":
		r, err = a.top(ctx, act, 0)
	case "dod", "dick_of_day", "tod":
		r, err = a.dod(ctx, act)
	case "pvp", "battle", "attack", "fight":
		bet, perr := strconv.ParseInt(strings.TrimSpace(m.CommandArguments()), 10, 32)
		if perr != nil || bet <= 0 {
			r = text(a.t(act, "pvp.errors.no_args"))
		} else {
			r, err = a.pvpStart(ctx, act, bet)
		}
	case "stats":
		r, err = a.stats(ctx, act)
	case "loan":
		r, err = a.loanOffer(ctx, act)
	case "language":
		r = a.languageMenu(m.Chat.ID, act)
	default:
		return
	}
	if err != nil {
		log.Printf("/%s in %d: %v", cmd, m.Chat.ID, err)
		return
	}
	a.reply(m, r)
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
func (a *App) perks(ctx context.Context, tx pgx.Tx, act Act, uid, length, base int64) (int64, string, error) {
	g := getGender(ctx, a.db, uid)
	var lines []string
	total := base
	if length < 0 && a.cfg.HelpPussiesCoef > 0 {
		if help := int64(a.cfg.HelpPussiesCoef*float64(-length) + 0.5); help != 0 {
			total += help
			lines = append(lines, fmt.Sprintf("— %s (%+d)", a.texts.G(act.Lang, g, "perks.help-pussies"), help))
		}
	}
	paid, err := payLoan(ctx, tx, act.Chat, uid, base)
	if err != nil {
		return 0, "", err
	}
	if paid > 0 {
		total -= paid
		lines = append(lines, fmt.Sprintf("— %s (%+d)", a.texts.G(act.Lang, g, "perks.loan-payout"), -paid))
	}
	if len(lines) == 0 {
		return total, "", nil
	}
	return total, "\n\n" + a.texts.G(act.Lang, g, "perks.top_line") + ":\n" + strings.Join(lines, "\n"), nil
}

func (a *App) grow(ctx context.Context, act Act) (Reply, error) {
	uid := act.User.ID
	registered, err := saveUser(ctx, a.db, uid, fullName(act.User))
	if err != nil {
		return Reply{}, err
	}
	tomorrow := text(a.me(act, "grow.tomorrow") + a.timeLeft(act.Lang))

	tx, err := a.db.Begin(ctx)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(ctx)

	length, grownToday, _, err := lockLength(ctx, tx, act.Chat, uid)
	if err != nil {
		return Reply{}, err
	}
	if grownToday {
		return tomorrow, nil
	}
	chance := a.cfg.GrowShrinkRatio
	if time.Since(registered) <= time.Duration(a.cfg.NewcomersGrace)*24*time.Hour {
		chance = 1 // newcomers only grow
	}
	base := baseGrowth(a.cfg.GrowthMin, a.cfg.GrowthMax, chance)
	change, perksText, err := a.perks(ctx, tx, act, uid, length, base)
	if err != nil {
		return Reply{}, err
	}

	var newLength int64
	err = tx.QueryRow(ctx, `INSERT INTO Tits(uid, chat_id, length, updated_at) VALUES ($1, $2, $3, now())
		ON CONFLICT (uid, chat_id) DO UPDATE SET length = Tits.length + $3, updated_at = now()
		RETURNING length`, uid, act.Chat, change).Scan(&newLength)
	if pgCode(err) == errGrownToday {
		return tomorrow, nil
	}
	if err != nil {
		return Reply{}, err
	}
	pos, err := position(ctx, tx, act.Chat, uid)
	if err != nil {
		return Reply{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Reply{}, err
	}

	event := "grow.direction.grown"
	if change < 0 {
		event = "grow.direction.shrunk"
	}
	abs := change
	if abs < 0 {
		abs = -abs
	}
	return text(a.me(act, "grow.result", "event", a.me(act, event), "incr", abs, "length", newLength) +
		"\n" + a.me(act, "grow.position", "pos", pos) + perksText + a.timeLeft(act.Lang)), nil
}

func (a *App) top(ctx context.Context, act Act, page int) (Reply, error) {
	limit := a.cfg.TopLimit
	players, err := getTop(ctx, a.db, a.cfg, act.Chat, page*limit, limit+1)
	if err != nil {
		return Reply{}, err
	}
	if len(players) == 0 {
		return text(a.t(act, "top.empty")), nil
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
		if p.UID == act.User.ID {
			name = "<u>" + name + "</u>"
		}
		line := a.texts.G(act.Lang, p.Gender, "top.line", "n", p.Position, "name", name, "length", p.Length)
		switch {
		case now.Sub(p.GrownAt) > time.Duration(a.cfg.InactivityDays)*24*time.Hour:
			anyInactive = true
			line += " [~]"
		case p.GrownAt.UTC().Before(today):
			line += " [+]"
		}
		lines = append(lines, line)
	}
	out := a.t(act, "top.title") + "\n\n" + strings.Join(lines, "\n") + "\n\n" + a.t(act, "top.ending")
	if anyInactive {
		out += "\n" + a.t(act, "top.ending_inactive", "days", a.cfg.InactivityDays)
	}

	var row []tg.InlineKeyboardButton
	if page > 0 {
		row = append(row, tg.NewInlineKeyboardButtonData("⬅️", fmt.Sprintf("top:page:%d", page-1)))
	}
	if more {
		row = append(row, tg.NewInlineKeyboardButtonData("➡️", fmt.Sprintf("top:page:%d", page+1)))
	}
	if len(row) == 0 {
		return text(out), nil
	}
	return Reply{out, kb(row)}, nil
}

func (a *App) dod(ctx context.Context, act Act) (Reply, error) {
	if name, err := todaysDod(ctx, a.db, act.Chat); err != nil {
		return Reply{}, err
	} else if name != "" {
		return text(a.t(act, "dod.already_chosen", "name", esc(name)) + a.timeLeft(act.Lang)), nil
	}
	uid, name, err := randomActivePlayer(ctx, a.db, act.Chat, a.cfg.InactivityDays, a.cfg.DodRichExclusion)
	if err != nil {
		return Reply{}, err
	}
	if uid == 0 {
		return text(a.t(act, "dod.no_candidates")), nil
	}
	winner := getGender(ctx, a.db, uid)

	tx, err := a.db.Begin(ctx)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(ctx)
	length, _, _, err := lockLength(ctx, tx, act.Chat, uid)
	if err != nil {
		return Reply{}, err
	}
	base := 1 + rand.Int64N(max(a.cfg.DodBonusMax, 1))
	change, perksText, err := a.perks(ctx, tx, act, uid, length, base)
	if err != nil {
		return Reply{}, err
	}
	newLength, err := addLength(ctx, tx, act.Chat, uid, change)
	if err != nil {
		return Reply{}, err
	}
	_, err = tx.Exec(ctx, "INSERT INTO Titties_of_Day(chat_id, winner_uid) VALUES ($1, $2)", act.Chat, uid)
	if pgCode(err) == errDodChosen { // someone else ran /dod at the same moment
		tx.Rollback(ctx)
		w, _ := todaysDod(ctx, a.db, act.Chat)
		return text(a.t(act, "dod.already_chosen", "name", esc(w)) + a.timeLeft(act.Lang)), nil
	}
	if err != nil {
		return Reply{}, err
	}
	pos, err := position(ctx, tx, act.Chat, uid)
	if err != nil {
		return Reply{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Reply{}, err
	}
	return text(a.texts.G(act.Lang, winner, "dod.result", "uid", uid, "name", esc(name), "growth", change, "length", newLength) +
		"\n" + a.texts.G(act.Lang, winner, "dod.position", "pos", pos) + perksText + a.timeLeft(act.Lang)), nil
}

func (a *App) pvpStart(ctx context.Context, act Act, bet int64) (Reply, error) {
	p, err := getPlayer(ctx, a.db, act.Chat, act.User.ID)
	if err != nil {
		return Reply{}, err
	}
	if p == nil || p.Length < bet {
		return text(a.me(act, "pvp.errors.not_enough.initiator")), nil
	}
	return Reply{a.me(act, "pvp.results.start", "name", esc(fullName(act.User)), "bet", bet), a.pvpButton(act, bet)}, nil
}

// pvpButton has the same data format as the old bot, so old buttons keep working.
func (a *App) pvpButton(act Act, bet int64) *tg.InlineKeyboardMarkup {
	data := fmt.Sprintf("pvp:%d:%d:%d", act.User.ID, bet, time.Now().UnixMilli()-1719014400000)
	return kb(tg.NewInlineKeyboardRow(tg.NewInlineKeyboardButtonData(a.t(act, "pvp.button"), data)))
}

func (a *App) stats(ctx context.Context, act Act) (Reply, error) {
	var parts []string
	p, err := getPlayer(ctx, a.db, act.Chat, act.User.ID)
	if err != nil {
		return Reply{}, err
	}
	if p != nil {
		parts = append(parts, a.me(act, "stats.length", "length", p.Length, "pos", p.Position))
	}
	s, err := getStats(ctx, a.db, act.Chat, act.User.ID)
	if err != nil {
		return Reply{}, err
	}
	parts = append(parts, a.me(act, "stats.pvp", "win_rate", s.WinRate(), "battles", s.Total, "wins", s.Won,
		"win_streak", s.StreakMax, "acquired", s.Acquired, "lost", s.Lost))
	chats, maxLen, total, err := personalStats(ctx, a.db, act.User.ID)
	if err != nil {
		return Reply{}, err
	}
	parts = append(parts, a.me(act, "stats.personal", "chats", chats, "max_length", maxLen, "total_length", total))
	parts = append(parts, "<i>"+a.t(act, "stats.notice")+"</i>")
	return text(strings.Join(parts, "\n\n")), nil
}

func (a *App) loanOffer(ctx context.Context, act Act) (Reply, error) {
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(ctx)
	if loan, err := activeLoan(ctx, tx, act.Chat, act.User.ID); err != nil {
		return Reply{}, err
	} else if loan != nil {
		return text(a.me(act, "loan.debt", "debt", loan.Debt)), nil
	}
	p, err := getPlayer(ctx, a.db, act.Chat, act.User.ID)
	if err != nil {
		return Reply{}, err
	}
	if p == nil || p.Length >= 0 || a.cfg.LoanPayoutCoef <= 0 || a.cfg.LoanPayoutCoef >= 1 {
		return text(a.me(act, "loan.errors.positive_length")), nil
	}
	debt := -p.Length
	ratio := strconv.FormatFloat(a.cfg.LoanPayoutCoef, 'f', -1, 32)
	buttons := kb(tg.NewInlineKeyboardRow(
		tg.NewInlineKeyboardButtonData(a.t(act, "loan.confirmation.buttons.agree"), fmt.Sprintf("loan:%d:confirmed:%d:%s", act.User.ID, debt, ratio)),
		tg.NewInlineKeyboardButtonData(a.t(act, "loan.confirmation.buttons.disagree"), fmt.Sprintf("loan:%d:refused", act.User.ID)),
	))
	pct := fmt.Sprintf("%.2f%%", a.cfg.LoanPayoutCoef*100)
	return Reply{a.me(act, "loan.confirmation.text", "payout_percentage", pct, "debt", debt), buttons}, nil
}

// langNames are the buttons of /language, in this order.
var langNames = [][2]string{{"en", "🇬🇧 English"}, {"ru", "🇷🇺 Русский"}, {"it", "🇮🇹 Italiano"},
	{"fa", "🇮🇷 فارسی"}, {"zh", "🇨🇳 简体中文"}, {"zh-TW", "🇹🇼 繁體中文"}}

func (a *App) isChatAdmin(chatID, uid int64) bool {
	member, err := a.bot.GetChatMember(tg.GetChatMemberConfig{ChatConfigWithUser: tg.ChatConfigWithUser{ChatID: chatID, UserID: uid}})
	return err == nil && (member.IsCreator() || member.IsAdministrator())
}

func (a *App) languageMenu(tgChat int64, act Act) Reply {
	if !a.isChatAdmin(tgChat, act.User.ID) {
		return text(a.t(act, "language.errors.admins_only"))
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
	rows = append(rows, tg.NewInlineKeyboardRow(tg.NewInlineKeyboardButtonData(a.t(act, "language.chat.auto_button"), "lang:auto")))
	return Reply{a.t(act, "language.chat.prompt"), kb(rows...)}
}

// genderChoices are the /gender buttons: pronoun x body.
var genderChoices = []Gender{
	{"she", "tits"}, {"he", "dick"}, {"they", "tits"}, {"they", "dick"}, {"she", "dick"}, {"he", "tits"},
}

func genderLabel(g Gender) string {
	icon := map[string]string{"tits": "🍒", "dick": "🍆"}[g.Body]
	pron := map[string]string{"she": "she/her", "he": "he/him", "they": "they/them"}[g.Pron]
	return icon + " " + pron
}

func (a *App) genderMenu(act Act) Reply {
	var rows [][]tg.InlineKeyboardButton
	for i := 0; i < len(genderChoices); i += 2 {
		var row []tg.InlineKeyboardButton
		for _, g := range genderChoices[i : i+2] {
			label := genderLabel(g)
			if g == act.G {
				label = "✅ " + label
			}
			row = append(row, tg.NewInlineKeyboardButtonData(label, fmt.Sprintf("gen:%d:%s:%s", act.User.ID, g.Pron, g.Body)))
		}
		rows = append(rows, row)
	}
	return Reply{a.me(act, "gender.prompt", "current", genderLabel(act.G)), kb(rows...)}
}

// ---------------- buttons ----------------

// chatOfButton finds the chat of a pressed button.
func (a *App) chatOfButton(ctx context.Context, q *tg.CallbackQuery) (int64, error) {
	if q.Message != nil {
		linkInstance(ctx, a.db, q.Message.Chat.ID, q.ChatInstance)
		return chatByTG(ctx, a.db, q.Message.Chat.ID)
	}
	return a.chatOfInline(ctx, q.InlineMessageID, q.ChatInstance)
}

func (a *App) onButton(ctx context.Context, q *tg.CallbackQuery) {
	parts := strings.Split(q.Data, ":")
	lang := a.texts.pickLang(q.From.LanguageCode)

	// buttons that don't need a chat
	if parts[0] == "gen" {
		a.btnGender(ctx, q, parts, lang)
		return
	}
	if q.Message != nil && q.Message.Chat.ID > 0 {
		a.answer(q, "")
		return
	}

	chat, err := a.chatOfButton(ctx, q)
	if err != nil {
		log.Printf("button chat: %v", err)
		a.answer(q, "")
		return
	}
	act := a.newAct(ctx, chat, q.From)

	switch parts[0] {
	case "top":
		page, _ := strconv.Atoi(parts[len(parts)-1])
		a.answer(q, "")
		if r, err := a.top(ctx, act, max(page, 0)); err == nil {
			a.edit(q, r)
		}
	case "inl":
		err = a.btnInline(ctx, q, act, parts)
	case "pvp":
		err = a.btnPvp(ctx, q, act, parts)
	case "loan":
		err = a.btnLoan(ctx, q, act, parts)
	case "lang":
		err = a.btnLanguage(ctx, q, act, parts)
	default:
		a.answer(q, "")
	}
	if err != nil {
		log.Printf("button %q: %v", q.Data, err)
	}
}

func (a *App) btnGender(ctx context.Context, q *tg.CallbackQuery, parts []string, lang string) {
	if len(parts) != 4 {
		a.answer(q, "")
		return
	}
	if uid, _ := strconv.ParseInt(parts[1], 10, 64); uid != q.From.ID {
		a.answer(q, a.texts.T(lang, "errors.another_user"))
		return
	}
	g := Gender{Pron: parts[2], Body: parts[3]}
	if !validGender(g) {
		a.answer(q, "")
		return
	}
	if err := setGender(ctx, a.db, q.From.ID, g); err != nil {
		log.Printf("set gender: %v", err)
		a.answer(q, "")
		return
	}
	a.answer(q, "")
	a.edit(q, text(a.texts.G(lang, g, "gender.saved", "current", genderLabel(g))))
}

func (a *App) btnPvp(ctx context.Context, q *tg.CallbackQuery, act Act, parts []string) error {
	if len(parts) < 3 {
		a.answer(q, "")
		return nil
	}
	initiator, _ := strconv.ParseInt(parts[1], 10, 64)
	bet, _ := strconv.ParseInt(parts[2], 10, 64)
	if initiator == q.From.ID {
		a.answer(q, a.me(act, "pvp.errors.same_person"))
		return nil
	}
	key := q.InlineMessageID
	if q.Message != nil {
		key = fmt.Sprintf("%d:%d", q.Message.Chat.ID, q.Message.MessageID)
	}
	if _, busy := a.locks.LoadOrStore(key, true); busy {
		a.answer(q, a.t(act, "pvp.errors.battle_already_in_progress"))
		return nil
	}
	defer a.locks.Delete(key)

	if _, err := saveUser(ctx, a.db, q.From.ID, fullName(q.From)); err != nil {
		return err
	}
	chat := act.Chat
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
		a.answer(q, a.me(act, "pvp.errors.not_enough.acceptor"))
		return nil
	}
	a.answer(q, "")
	if !initOK || initLen < bet {
		a.edit(q, text(a.texts.G(act.Lang, getGender(ctx, a.db, initiator), "pvp.errors.not_enough.initiator")))
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
	wg, lg := getGender(ctx, a.db, winner), getGender(ctx, a.db, loser)
	l := act.Lang
	out := a.texts.G(l, wg, "pvp.results.finish", "winner_name", esc(winName), "winner_length", winnerLen,
		"loser_name", esc(loseName), "loser_length", loserLen, "bet", bet) +
		"\n\n" + a.texts.G(l, wg, "pvp.results.position.winner", "name", esc(winName), "pos", winPos) +
		"\n" + a.texts.G(l, lg, "pvp.results.position.loser", "name", esc(loseName), "pos", losePos)
	if withheld > 0 {
		out += "\n\n" + a.texts.G(l, wg, "pvp.results.withheld", "payout", withheld)
	}
	if a.cfg.PvpShowStats {
		out += "\n\n" + a.texts.T(l, "pvp.results.stats.text", "winner_win_rate", ws.WinRate(), "loser_win_rate", ls.WinRate(),
			"winner_win_streak", ws.Streak, "winner_win_streak_max", ws.StreakMax)
		if lostStreak > 1 {
			out += "\n" + a.texts.T(l, "pvp.results.stats.lost_win_streak", "lost_win_streak", lostStreak)
		}
	}
	a.edit(q, text(out))
	return nil
}

func (a *App) btnLoan(ctx context.Context, q *tg.CallbackQuery, act Act, parts []string) error {
	if len(parts) < 3 {
		a.answer(q, "")
		return nil
	}
	if uid, _ := strconv.ParseInt(parts[1], 10, 64); uid != q.From.ID {
		a.answer(q, a.t(act, "errors.another_user"))
		return nil
	}
	a.answer(q, "")
	if parts[2] != "confirmed" {
		a.edit(q, text(a.me(act, "loan.callback.refused")))
		return nil
	}
	if len(parts) >= 5 {
		if r, _ := strconv.ParseFloat(parts[4], 64); float32(r) != float32(a.cfg.LoanPayoutCoef) {
			a.edit(q, text(a.t(act, "loan.callback.payout_ratio_changed")))
			return nil
		}
	}

	tx, err := a.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	length, _, ok, err := lockLength(ctx, tx, act.Chat, q.From.ID)
	if err != nil {
		return err
	}
	if !ok || length >= 0 {
		a.edit(q, text(a.me(act, "loan.errors.positive_length")))
		return nil
	}
	debt := -length // the loan resets the length to zero
	loan, err := activeLoan(ctx, tx, act.Chat, q.From.ID)
	if err != nil {
		return err
	}
	if loan != nil {
		_, err = tx.Exec(ctx, "UPDATE Loans SET debt = debt + $2, payout_ratio = $3 WHERE id = $1", loan.ID, debt, a.cfg.LoanPayoutCoef)
	} else {
		_, err = tx.Exec(ctx, "INSERT INTO Loans(chat_id, uid, debt, payout_ratio) VALUES ($1, $2, $3, $4)", act.Chat, q.From.ID, debt, a.cfg.LoanPayoutCoef)
	}
	if err != nil {
		return err
	}
	if _, err := addLength(ctx, tx, act.Chat, q.From.ID, debt); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	a.edit(q, text(a.me(act, "loan.callback.success")))
	return nil
}

func (a *App) btnLanguage(ctx context.Context, q *tg.CallbackQuery, act Act, parts []string) error {
	if q.Message == nil || !a.isChatAdmin(q.Message.Chat.ID, q.From.ID) {
		a.answer(q, a.t(act, "language.errors.admins_only"))
		return nil
	}
	a.answer(q, "")
	choice := parts[len(parts)-1]
	if choice == "auto" {
		if err := setChatLanguage(ctx, a.db, act.Chat, ""); err != nil {
			return err
		}
		a.edit(q, text(a.texts.T(a.texts.pickLang(q.From.LanguageCode), "language.chat.reset")))
		return nil
	}
	if !a.texts.Has(choice) {
		return nil
	}
	if err := setChatLanguage(ctx, a.db, act.Chat, choice); err != nil {
		return err
	}
	a.edit(q, text(a.texts.T(choice, "language.chat.success")))
	return nil
}

// ---------------- web app buttons ----------------

// cmdApp: in a private chat it opens the web app, in a group it links to the private chat.
func (a *App) cmdApp(m *tg.Message, lang string) {
	if a.cfg.WebURL == "" {
		return
	}
	if m.Chat.ID > 0 {
		a.sendAppButton(m.Chat.ID, lang)
		return
	}
	link := fmt.Sprintf("https://t.me/%s?start=app", a.bot.Self.UserName)
	a.reply(m, Reply{a.texts.T(lang, "app.group"), kb(tg.NewInlineKeyboardRow(tg.NewInlineKeyboardButtonURL(a.texts.T(lang, "app.button"), link)))})
}

// sendAppButton sends a web_app button. The bot library is older than web apps, so this is a raw request.
func (a *App) sendAppButton(chatID int64, lang string) {
	if a.cfg.WebURL == "" {
		return
	}
	markup, _ := json.Marshal(map[string]any{"inline_keyboard": [][]map[string]any{{
		{"text": a.texts.T(lang, "app.button"), "web_app": map[string]string{"url": a.cfg.WebURL + "/"}},
	}}})
	_, err := a.bot.MakeRequest("sendMessage", tg.Params{
		"chat_id": strconv.FormatInt(chatID, 10), "text": a.texts.T(lang, "app.private"),
		"parse_mode": "HTML", "reply_markup": string(markup),
	})
	if err != nil {
		log.Printf("send app button: %v", err)
	}
}

// setMenuButton puts the web app on the menu button of every private chat with the bot.
func (a *App) setMenuButton() {
	if a.cfg.WebURL == "" {
		return
	}
	button, _ := json.Marshal(map[string]any{"type": "web_app", "text": "🍒 App", "web_app": map[string]string{"url": a.cfg.WebURL + "/"}})
	if _, err := a.bot.MakeRequest("setChatMenuButton", tg.Params{"menu_button": string(button)}); err != nil {
		log.Printf("set menu button: %v", err)
	}
}

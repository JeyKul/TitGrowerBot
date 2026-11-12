package main

// Inline mode: typing "@TitGrowerBot" in any chat shows grow, top, dod, stats and loan.
// "@TitGrowerBot 5" offers a fight with a bet of 5.
//
// An inline query doesn't say which chat it comes from. The bot sends a message with a button;
// when someone presses it, the button tells the chat:
//   - in a supergroup, inline_message_id contains the group id, so inline play counts towards
//     the same score as the normal commands;
//   - otherwise only chat_instance is known. If the group was seen with that chat_instance
//     before (any button pressed there), it still counts for the group; if not, the chat gets
//     its own row keyed by chat_instance.

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"

	tg "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// inlineChatID gets the group id out of an inline_message_id, or 0 if it has none
// (basic groups and private chats only have a chat_instance).
func inlineChatID(id string) int64 {
	b, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return 0
	}
	var peer int64
	switch len(id) {
	case 27: // dc_id int32, message_id int32, chat_id int32, access_hash int64
		if len(b) < 12 {
			return 0
		}
		peer = int64(int32(binary.LittleEndian.Uint32(b[8:12])))
	case 32: // dc_id int32, owner_id int64, message_id int32, access_hash int64
		if len(b) < 12 {
			return 0
		}
		peer = int64(binary.LittleEndian.Uint64(b[4:12]))
	default:
		return 0
	}
	// Only supergroups come out negative; the Bot API id is -100<peer id, 10 digits>.
	if peer >= 0 || peer < -999_999_999_999 {
		return 0
	}
	return -1_000_000_000_000 + peer
}

func (a *App) chatOfInline(ctx context.Context, inlineID, instance string) (int64, error) {
	if tgChat := inlineChatID(inlineID); tgChat != 0 {
		linkInstance(ctx, a.db, tgChat, instance)
		return chatByTG(ctx, a.db, tgChat)
	}
	if instance == "" {
		return 0, errors.New("button has neither a chat nor a chat_instance")
	}
	return chatByInstance(ctx, a.db, instance)
}

var inlineCommands = []string{"grow", "top", "dod", "stats", "loan"}

func (a *App) onInline(ctx context.Context, q *tg.InlineQuery) {
	lang := a.texts.pickLang(q.From.LanguageCode)
	act := Act{User: q.From, Lang: lang, G: getGender(ctx, a.db, q.From.ID)}
	var results []any

	if bet, err := strconv.ParseInt(strings.TrimSpace(q.Query), 10, 32); err == nil && bet > 0 {
		r := tg.NewInlineQueryResultArticleHTML("pvp", a.me(act, "inline.titles.pvp", "bet", bet),
			a.me(act, "pvp.results.start", "name", esc(fullName(q.From)), "bet", bet))
		r.ReplyMarkup = a.pvpButton(act, bet)
		results = append(results, r)
	}
	for _, cmd := range inlineCommands {
		r := tg.NewInlineQueryResultArticleHTML(cmd, a.me(act, "inline.titles."+cmd), a.me(act, "inline.text"))
		r.Description = a.me(act, "inline.descriptions."+cmd)
		r.ReplyMarkup = kb(tg.NewInlineKeyboardRow(
			tg.NewInlineKeyboardButtonData(a.t(act, "inline.button"), fmt.Sprintf("inl:%s:%d", cmd, q.From.ID))))
		results = append(results, r)
	}

	answer := tg.InlineConfig{InlineQueryID: q.ID, Results: results, IsPersonal: true, CacheTime: 1}
	if _, err := a.bot.Request(answer); err != nil {
		log.Printf("answer inline: %v", err)
	}
}

// btnInline runs the command of an inline message's button and puts the result into the message.
func (a *App) btnInline(ctx context.Context, q *tg.CallbackQuery, act Act, parts []string) error {
	if len(parts) < 3 {
		a.answer(q, "")
		return nil
	}
	cmd := parts[1]
	owner, _ := strconv.ParseInt(parts[2], 10, 64)
	// Growing and loans are personal: only the one who sent the message can press.
	if (cmd == "grow" || cmd == "stats" || cmd == "loan") && owner != q.From.ID {
		a.answer(q, a.t(act, "errors.another_user"))
		return nil
	}
	if _, err := saveUser(ctx, a.db, q.From.ID, fullName(q.From)); err != nil {
		return err
	}

	var r Reply
	var err error
	switch cmd {
	case "grow":
		r, err = a.grow(ctx, act)
	case "top":
		r, err = a.top(ctx, act, 0)
	case "dod":
		r, err = a.dod(ctx, act)
	case "stats":
		r, err = a.stats(ctx, act)
	case "loan":
		r, err = a.loanOffer(ctx, act)
	default:
		a.answer(q, "")
		return nil
	}
	a.answer(q, "")
	if err != nil {
		return err
	}
	a.edit(q, r)
	return nil
}

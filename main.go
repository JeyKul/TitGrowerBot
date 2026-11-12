// TitGrowerBot: a Telegram game bot for group chats.
//
// Files:
//
//	main.go   - config, startup, update routing
//	db.go     - every SQL query
//	game.go   - command and button handlers
//	texts.go  - loads locales/*.txt
package main

import (
	"context"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"

	tg "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Config holds every setting. All of them come from environment variables.
type Config struct {
	Token            string
	DatabaseURL      string
	GrowthMin        int64
	GrowthMax        int64
	GrowShrinkRatio  float64 // chance of a positive growth
	DodBonusMax      int64
	NewcomersGrace   int // days a new player only grows
	TopLimit         int
	InactivityDays   int
	HideInactiveZero bool
	HelpPussiesCoef  float64 // extra growth for negative lengths
	LoanPayoutCoef   float64 // part of each growth that repays a loan
	DodRichExclusion float64 // richest part of the chat that can't win DoD
	PvpCheckAcceptor bool
	PvpShowStats     bool
	Debug            bool
	WebURL           string // public https address of the web app, e.g. https://example.com/tits
	WebAddr          string // where the web server listens, e.g. 127.0.0.1:8085
	Admins           map[int64]bool
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int64) int64 {
	v, err := strconv.ParseInt(env(key, ""), 10, 64)
	if err != nil {
		return def
	}
	return v
}

func envFloat(key string, def float64) float64 {
	v, err := strconv.ParseFloat(env(key, ""), 64)
	if err != nil {
		return def
	}
	return v
}

func envBool(key string, def bool) bool {
	v, err := strconv.ParseBool(env(key, ""))
	if err != nil {
		return def
	}
	return v
}

// loadDotEnv reads KEY=VALUE lines from a .env file in the current folder.
// Variables already set in the environment win.
func loadDotEnv() {
	data, err := os.ReadFile(".env")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(strings.TrimPrefix(k, "export ")), strings.Trim(strings.TrimSpace(v), `"'`)
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
}

func loadConfig() Config {
	loadDotEnv()
	return Config{
		Token:            env("BOT_TOKEN", env("TELOXIDE_TOKEN", "")),
		DatabaseURL:      env("DATABASE_URL", ""),
		GrowthMin:        envInt("GROWTH_MIN", -5),
		GrowthMax:        envInt("GROWTH_MAX", 10),
		GrowShrinkRatio:  envFloat("GROW_SHRINK_RATIO", 0.5),
		DodBonusMax:      envInt("GROWTH_DOD_BONUS_MAX", 5),
		NewcomersGrace:   int(envInt("NEWCOMERS_GRACE_DAYS", 7)),
		TopLimit:         int(envInt("TOP_LIMIT", 10)),
		InactivityDays:   int(envInt("INACTIVITY_DAYS", 7)),
		HideInactiveZero: envBool("HIDE_INACTIVE_ZERO_LENGTH_FROM_TOP", true),
		HelpPussiesCoef:  envFloat("HELP_PUSSIES_COEF", 0.01),
		LoanPayoutCoef:   envFloat("LOAN_PAYOUT_COEF", 0.1),
		DodRichExclusion: envFloat("DOD_RICH_EXCLUSION_RATIO", 0.1),
		PvpCheckAcceptor: envBool("PVP_CHECK_ACCEPTOR_LENGTH", false),
		PvpShowStats:     envBool("PVP_STATS_SHOW", true),
		Debug:            envBool("DEBUG", false),
		WebURL:           strings.TrimRight(env("WEB_URL", ""), "/"),
		WebAddr:          env("WEB_ADDR", "127.0.0.1:8085"),
		Admins:           parseIDs(env("ADMIN_IDS", "")),
	}
}

// parseIDs reads "123,456" into a set.
func parseIDs(s string) map[int64]bool {
	ids := map[int64]bool{}
	for _, part := range strings.Split(s, ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err == nil {
			ids[id] = true
		}
	}
	return ids
}

// App is shared by all handlers.
type App struct {
	cfg   Config
	bot   *tg.BotAPI
	db    *pgxpool.Pool
	texts *Texts
	locks sync.Map // PvP buttons being processed right now
}

func main() {
	cfg := loadConfig()
	if cfg.Token == "" || cfg.DatabaseURL == "" {
		log.Fatal("BOT_TOKEN and DATABASE_URL must be set")
	}

	texts, err := loadTexts(env("LOCALES_DIR", "locales"))
	if err != nil {
		log.Fatalf("load locales: %v", err)
	}

	ctx := context.Background()
	db, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("connect to database: %v", err)
	}
	defer db.Close()
	if err := migrate(ctx, db, env("MIGRATIONS_DIR", "migrations")); err != nil {
		log.Fatalf("migrations: %v", err)
	}
	if err := ensureTables(ctx, db); err != nil {
		log.Fatalf("extra tables: %v", err)
	}

	go backupLoop(cfg.DatabaseURL)

	bot, err := tg.NewBotAPI(cfg.Token)
	if err != nil {
		log.Fatalf("telegram: %v", err)
	}
	bot.Debug = cfg.Debug
	log.Printf("started as @%s", bot.Self.UserName)

	app := &App{cfg: cfg, bot: bot, db: db, texts: texts}
	app.setCommands()
	app.setMenuButton()
	go app.serveWeb()

	u := tg.NewUpdate(0)
	u.Timeout = 60
	u.AllowedUpdates = []string{"message", "callback_query", "inline_query"}
	for update := range bot.GetUpdatesChan(u) {
		go app.handle(update)
	}
}

// handle routes one update to its handler. A panic in a handler only kills that update.
func (a *App) handle(u tg.Update) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic: %v", r)
		}
	}()
	ctx := context.Background()

	switch {
	case u.InlineQuery != nil:
		if !isBlocked(ctx, a.db, u.InlineQuery.From.ID) {
			a.onInline(ctx, u.InlineQuery)
		}
	case u.CallbackQuery != nil:
		if isBlocked(ctx, a.db, u.CallbackQuery.From.ID) {
			a.answer(u.CallbackQuery, a.texts.T(a.texts.pickLang(u.CallbackQuery.From.LanguageCode), "errors.blocked"))
			return
		}
		a.onButton(ctx, u.CallbackQuery)
	case u.Message != nil:
		m := u.Message
		if m.MigrateToChatID != 0 {
			a.onChatMigrated(ctx, m.Chat.ID, m.MigrateToChatID)
			return
		}
		if !m.IsCommand() || (m.From != nil && isBlocked(ctx, a.db, m.From.ID)) {
			return
		}
		// "/grow@OtherBot" is not for us.
		if at := m.CommandWithAt(); strings.Contains(at, "@") &&
			!strings.EqualFold(at[strings.Index(at, "@")+1:], a.bot.Self.UserName) {
			return
		}
		a.onCommand(ctx, m)
	}
}

// setCommands fills the command menu in Telegram for every language.
func (a *App) setCommands() {
	names := []string{"grow", "top", "dod", "pvp", "stats", "loan", "gender", "app", "language", "help"}
	for _, lang := range a.texts.Langs() {
		var cmds []tg.BotCommand
		for _, n := range names {
			cmds = append(cmds, tg.BotCommand{Command: n, Description: a.texts.T(lang, n+".description")})
		}
		if lang == "zh-TW" {
			continue // Telegram menus know only "zh"
		}
		cfg := tg.NewSetMyCommands(cmds...)
		if lang != "en" {
			cfg.LanguageCode = lang
		}
		if _, err := a.bot.Request(cfg); err != nil {
			log.Printf("set commands (%s): %v", lang, err)
		}
	}
}

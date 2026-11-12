package main

// The web app: a page inside Telegram (web/index.html) plus a small JSON API.
//
// Telegram opens the page and gives it "initData", signed with the bot token. The page sends it
// in the X-Init-Data header with every request; the server checks the signature, so nobody can
// pretend to be someone else. Admins (ADMIN_IDS) also get the admin API.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed web/index.html
var indexHTML []byte

type webUser struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

// checkInitData verifies Telegram's signature and returns the user.
// See https://core.telegram.org/bots/webapps#validating-data-received-via-the-mini-app
func checkInitData(initData, token string, maxAge time.Duration) (*webUser, error) {
	vals, err := url.ParseQuery(initData)
	if err != nil {
		return nil, err
	}
	hash := vals.Get("hash")
	if hash == "" {
		return nil, errors.New("no hash")
	}
	var keys []string
	for k := range vals {
		if k != "hash" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var lines []string
	for _, k := range keys {
		lines = append(lines, k+"="+vals.Get(k))
	}
	secret := hmac.New(sha256.New, []byte("WebAppData"))
	secret.Write([]byte(token))
	mac := hmac.New(sha256.New, secret.Sum(nil))
	mac.Write([]byte(strings.Join(lines, "\n")))
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(hash)) {
		return nil, errors.New("bad signature")
	}
	authDate, _ := strconv.ParseInt(vals.Get("auth_date"), 10, 64)
	if time.Since(time.Unix(authDate, 0)) > maxAge {
		return nil, errors.New("login expired, reopen the app")
	}
	var u webUser
	if err := json.Unmarshal([]byte(vals.Get("user")), &u); err != nil || u.ID == 0 {
		return nil, errors.New("no user")
	}
	return &u, nil
}

func (a *App) serveWeb() {
	if a.cfg.WebURL == "" {
		log.Printf("web app off (set WEB_URL to turn it on)")
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/me", a.api(false, a.apiMe))
	mux.HandleFunc("POST /api/gender", a.api(false, a.apiGender))
	mux.HandleFunc("GET /api/top", a.api(false, a.apiTop))
	mux.HandleFunc("GET /api/admin/users", a.api(true, a.apiAdminUsers))
	mux.HandleFunc("GET /api/admin/user", a.api(true, a.apiAdminUser))
	mux.HandleFunc("POST /api/admin/block", a.api(true, a.apiAdminBlock))
	mux.HandleFunc("POST /api/admin/length", a.api(true, a.apiAdminLength))
	mux.HandleFunc("POST /api/admin/delete", a.api(true, a.apiAdminDelete))

	// Works with or without the proxy stripping the path, e.g. "/tits".
	var handler http.Handler = mux
	if u, err := url.Parse(a.cfg.WebURL); err == nil && strings.Trim(u.Path, "/") != "" {
		prefix := "/" + strings.Trim(u.Path, "/")
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == prefix {
				http.Redirect(w, r, prefix+"/", http.StatusMovedPermanently)
				return
			}
			if p, ok := strings.CutPrefix(r.URL.Path, prefix); ok {
				r.URL.Path = p
			}
			mux.ServeHTTP(w, r)
		})
	}
	log.Printf("web app on %s (public: %s)", a.cfg.WebAddr, a.cfg.WebURL)
	srv := &http.Server{Addr: a.cfg.WebAddr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		log.Printf("web app stopped: %v", err)
	}
}

type apiFunc func(ctx context.Context, u *webUser, r *http.Request) (any, error)

// api checks the login, the admin rights and the block list, and writes JSON.
func (a *App) api(adminOnly bool, f apiFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fail := func(code int, msg string) {
			w.WriteHeader(code)
			json.NewEncoder(w).Encode(map[string]string{"error": msg})
		}
		u, err := checkInitData(r.Header.Get("X-Init-Data"), a.cfg.Token, 24*time.Hour)
		if err != nil {
			fail(http.StatusUnauthorized, err.Error())
			return
		}
		ctx := r.Context()
		if adminOnly && !a.cfg.Admins[u.ID] {
			fail(http.StatusForbidden, "admins only")
			return
		}
		if !a.cfg.Admins[u.ID] && isBlocked(ctx, a.db, u.ID) {
			fail(http.StatusForbidden, "you are blocked")
			return
		}
		out, err := f(ctx, u, r)
		if err != nil {
			fail(http.StatusBadRequest, err.Error())
			return
		}
		json.NewEncoder(w).Encode(out)
	}
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<16)).Decode(v)
}

func queryInt(r *http.Request, key string) int64 {
	v, _ := strconv.ParseInt(r.URL.Query().Get(key), 10, 64)
	return v
}

func (a *App) apiMe(ctx context.Context, u *webUser, r *http.Request) (any, error) {
	chats, err := userChats(ctx, a.db, u.ID)
	if err != nil {
		return nil, err
	}
	if chats == nil {
		chats = []UserChat{}
	}
	return map[string]any{
		"id": u.ID, "name": strings.TrimSpace(u.FirstName + " " + u.LastName),
		"admin": a.cfg.Admins[u.ID], "gender": getGender(ctx, a.db, u.ID), "chats": chats,
	}, nil
}

func (a *App) apiGender(ctx context.Context, u *webUser, r *http.Request) (any, error) {
	var g Gender
	if err := readJSON(r, &g); err != nil || !validGender(g) {
		return nil, errors.New("pick she/he/they and tits/dick")
	}
	return g, setGender(ctx, a.db, u.ID, g)
}

func (a *App) apiTop(ctx context.Context, u *webUser, r *http.Request) (any, error) {
	chat := queryInt(r, "chat")
	if !a.cfg.Admins[u.ID] && !playsIn(ctx, a.db, u.ID, chat) {
		return nil, errors.New("you don't play in that chat")
	}
	top, err := getTop(ctx, a.db, a.cfg, chat, 0, 100)
	if top == nil {
		top = []Player{}
	}
	return top, err
}

func (a *App) apiAdminUsers(ctx context.Context, u *webUser, r *http.Request) (any, error) {
	users, err := searchUsers(ctx, a.db, strings.TrimSpace(r.URL.Query().Get("q")))
	if users == nil {
		users = []AdminUser{}
	}
	return users, err
}

func (a *App) apiAdminUser(ctx context.Context, u *webUser, r *http.Request) (any, error) {
	uid := queryInt(r, "uid")
	name, err := userName(ctx, a.db, uid)
	if err != nil {
		return nil, errors.New("no such user")
	}
	chats, err := userChats(ctx, a.db, uid)
	if chats == nil {
		chats = []UserChat{}
	}
	return map[string]any{"uid": uid, "name": name, "blocked": isBlocked(ctx, a.db, uid),
		"gender": getGender(ctx, a.db, uid), "chats": chats}, err
}

func (a *App) apiAdminBlock(ctx context.Context, u *webUser, r *http.Request) (any, error) {
	var req struct {
		UID     int64 `json:"uid"`
		Blocked bool  `json:"blocked"`
	}
	if err := readJSON(r, &req); err != nil || req.UID == 0 {
		return nil, errors.New("bad request")
	}
	if a.cfg.Admins[req.UID] {
		return nil, errors.New("admins can't be blocked")
	}
	log.Printf("admin %d: block %d = %v", u.ID, req.UID, req.Blocked)
	return req, setBlocked(ctx, a.db, req.UID, req.Blocked)
}

func (a *App) apiAdminLength(ctx context.Context, u *webUser, r *http.Request) (any, error) {
	var req struct {
		UID    int64 `json:"uid"`
		Chat   int64 `json:"chat"`
		Length int64 `json:"length"`
	}
	if err := readJSON(r, &req); err != nil || req.UID == 0 {
		return nil, errors.New("bad request")
	}
	log.Printf("admin %d: length of %d in chat %d = %d", u.ID, req.UID, req.Chat, req.Length)
	return req, setLength(ctx, a.db, req.Chat, req.UID, req.Length)
}

func (a *App) apiAdminDelete(ctx context.Context, u *webUser, r *http.Request) (any, error) {
	var req struct {
		UID int64 `json:"uid"`
	}
	if err := readJSON(r, &req); err != nil || req.UID == 0 {
		return nil, errors.New("bad request")
	}
	log.Printf("admin %d: delete user %d", u.ID, req.UID)
	return req, deleteUser(ctx, a.db, req.UID)
}

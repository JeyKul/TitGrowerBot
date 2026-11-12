package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Texts holds all translations: lang -> key -> text.
//
// A locale file is locales/<lang>.txt with one "key = text" per line.
// Lines starting with # are comments. Write \n for a line break and
// {name} for a value the bot fills in. Missing keys fall back to English.
type Texts struct {
	m map[string]map[string]string
}

func loadTexts(dir string) (*Texts, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.txt"))
	if err != nil {
		return nil, err
	}
	t := &Texts{m: map[string]map[string]string{}}
	for _, f := range files {
		lang := strings.TrimSuffix(filepath.Base(f), ".txt")
		kv, err := parseTextFile(f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		t.m[lang] = kv
	}
	if _, ok := t.m["en"]; !ok {
		return nil, fmt.Errorf("no en.txt in %s", dir)
	}
	return t, nil
}

func parseTextFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	kv := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: no '='", n)
		}
		val = strings.TrimPrefix(val, " ")
		kv[strings.TrimSpace(key)] = strings.ReplaceAll(val, `\n`, "\n")
	}
	return kv, sc.Err()
}

// Langs lists the loaded languages, sorted.
func (t *Texts) Langs() []string {
	var out []string
	for l := range t.m {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

func (t *Texts) Has(lang string) bool { _, ok := t.m[lang]; return ok }

// T returns the text for key and fills in values: T("en", "top.line", "n", 1, "name", "Bob").
func (t *Texts) T(lang, key string, args ...any) string {
	s, ok := t.m[lang][key]
	if !ok {
		s, ok = t.m["en"][key]
		if !ok {
			return key
		}
	}
	for i := 0; i+1 < len(args); i += 2 {
		s = strings.ReplaceAll(s, "{"+fmt.Sprint(args[i])+"}", fmt.Sprint(args[i+1]))
	}
	return s
}

// G is T for a text about one player. A locale may have variants of a key:
//
//	key            tits + she (the default)
//	key.he         tits + he      key.they       tits + they
//	key.dick       dick (+ he)    key.dick.she   key.dick.they
//
// The most specific variant that exists wins: key.<body>.<pron>, key.<body>, key.<pron>, key.
func (t *Texts) G(lang string, g Gender, key string, args ...any) string {
	var tries []string
	if g.Body == "dick" {
		tries = append(tries, key+".dick."+g.Pron, key+".dick")
	}
	if g.Pron != "she" {
		tries = append(tries, key+"."+g.Pron)
	}
	for _, k := range tries {
		if _, ok := t.m[lang][k]; ok {
			return t.T(lang, k, args...)
		}
		if _, ok := t.m["en"][k]; ok && !t.hasKey(lang, key) {
			return t.T(lang, k, args...)
		}
	}
	return t.T(lang, key, args...)
}

func (t *Texts) hasKey(lang, key string) bool { _, ok := t.m[lang][key]; return ok }

// pickLang maps a Telegram language code ("ru", "zh-hans", "zh-hant", "pt-br") to a loaded locale.
func (t *Texts) pickLang(code string) string {
	code = strings.ToLower(code)
	if strings.HasPrefix(code, "zh") && (strings.Contains(code, "hant") || strings.Contains(code, "tw") || strings.Contains(code, "hk")) && t.Has("zh-TW") {
		return "zh-TW"
	}
	base := strings.SplitN(code, "-", 2)[0]
	if t.Has(base) {
		return base
	}
	return "en"
}

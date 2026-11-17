package main

import (
	"bufio"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Texts holds all translations: lang -> key -> text.
//
// A locale file is locales/<lang>.txt with one "key = text" per line.
// Lines starting with # are comments. Write \n for a line break and
// {name} for a value the bot fills in. Missing keys fall back to English.
//
// {pet} works in every text about a player: girl, boy or pet, by their pronoun.
//
// Any key can have random variants: key.1, key.2, ... (any numbers, gaps are fine).
// Each time, one of key, key.1, key.2, ... is picked. Only key.1 without key works too.
type Texts struct {
	m    map[string]map[string]string
	alts map[string]map[string][]string // lang -> key -> all its texts, when it has variants
	// lang -> body -> size tiers, from keys like size.dick.under5 and size.tits.over20
	sizes map[string]map[string][]sizeTier
}

type sizeTier struct {
	key   string
	under bool // true: length < n, false: length > n
	n     int64
}

func loadTexts(dir string) (*Texts, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.txt"))
	if err != nil {
		return nil, err
	}
	t := &Texts{m: map[string]map[string]string{}, alts: map[string]map[string][]string{}, sizes: map[string]map[string][]sizeTier{}}
	for _, f := range files {
		lang := strings.TrimSuffix(filepath.Base(f), ".txt")
		kv, err := parseTextFile(f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		t.m[lang], t.alts[lang] = groupVariants(kv)
		t.sizes[lang] = findSizeTiers(t.m[lang])
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

// groupVariants finds the numbered variants (key.1, key.2, ...) and removes them from kv.
// A key that only has numbered variants gets the first one as its plain text.
func groupVariants(kv map[string]string) (map[string]string, map[string][]string) {
	nums := map[string][]int{}
	for k := range kv {
		i := strings.LastIndexByte(k, '.')
		if n, err := strconv.Atoi(k[i+1:]); i > 0 && err == nil && n >= 0 {
			nums[k[:i]] = append(nums[k[:i]], n)
		}
	}
	alts := map[string][]string{}
	for base, ns := range nums {
		sort.Ints(ns)
		var list []string
		if s, ok := kv[base]; ok {
			list = append(list, s)
		}
		for _, n := range ns {
			k := base + "." + strconv.Itoa(n)
			list = append(list, kv[k])
			delete(kv, k)
		}
		kv[base] = list[0]
		if len(list) > 1 {
			alts[base] = list
		}
	}
	return kv, alts
}

var sizeKey = regexp.MustCompile(`^size\.(tits|dick)\.(under|over)(-?\d+)$`)

// findSizeTiers lists the size.<body>.under<N> / over<N> keys, "under" ones from the smallest N,
// then "over" ones from the biggest N. The first one that matches a length is used.
func findSizeTiers(kv map[string]string) map[string][]sizeTier {
	out := map[string][]sizeTier{}
	for k := range kv {
		if m := sizeKey.FindStringSubmatch(k); m != nil {
			n, _ := strconv.ParseInt(m[3], 10, 64)
			out[m[1]] = append(out[m[1]], sizeTier{key: k, under: m[2] == "under", n: n})
		}
	}
	for _, tiers := range out {
		sort.Slice(tiers, func(i, j int) bool {
			a, b := tiers[i], tiers[j]
			if a.under != b.under {
				return a.under
			}
			if a.under {
				return a.n < b.n
			}
			return a.n > b.n
		})
	}
	return out
}

// Size is the line of a language for a length, or "" if no tier matches.
// Pronoun variants: size.dick.under5.they wins over size.dick.under5 for a they-player.
func (t *Texts) Size(lang string, g Gender, length int64, args ...any) string {
	args = append(args, "pet", petWord(g))
	for _, tier := range t.sizes[lang][g.Body] {
		if (tier.under && length < tier.n) || (!tier.under && length > tier.n) {
			if t.hasKey(lang, tier.key+"."+g.Pron) {
				return t.T(lang, tier.key+"."+g.Pron, args...)
			}
			return t.T(lang, tier.key, args...)
		}
	}
	return ""
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
		lang = "en"
		s, ok = t.m[lang][key]
		if !ok {
			return key
		}
	}
	if list := t.alts[lang][key]; len(list) > 1 {
		s = list[rand.IntN(len(list))]
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
	args = append(args, "pet", petWord(g))
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

// petWord fills {pet}: "good {pet}" is good girl / good boy / good pet.
func petWord(g Gender) string {
	return map[string]string{"she": "girl", "he": "boy"}[g.Pron] + map[string]string{"they": "pet"}[g.Pron]
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

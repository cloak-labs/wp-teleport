package sqlstream

import (
	"bytes"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Pair is one literal find/replace rule.
type Pair struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// RegexPair is one regular-expression find/replace rule.
type RegexPair struct {
	Pattern *regexp.Regexp
	To      string
}

// Replacer applies find/replace rules to string values. It understands PHP
// serialize() output, so lengths stay correct when a value changes size, and
// it recurses into strings that are themselves serialized.
type Replacer struct {
	pairs      []Pair
	literal    *strings.Replacer
	needles    []string
	escNeedles [][]byte
	regexes    []RegexPair
}

// NewReplacer builds a Replacer. Pairs are applied longest-From first so a
// specific rule (an uploads URL) wins over a general one (the site URL).
func NewReplacer(pairs []Pair, regexes []RegexPair) *Replacer {
	seen := map[string]bool{}
	var clean []Pair
	for _, p := range pairs {
		if p.From == "" || p.From == p.To || seen[p.From] {
			continue
		}
		seen[p.From] = true
		clean = append(clean, p)
	}
	sort.SliceStable(clean, func(i, j int) bool { return len(clean[i].From) > len(clean[j].From) })

	r := &Replacer{pairs: clean, regexes: regexes}
	args := make([]string, 0, len(clean)*2)
	for _, p := range clean {
		args = append(args, p.From, p.To)
		r.needles = append(r.needles, p.From)
		r.escNeedles = append(r.escNeedles, Escape(nil, []byte(p.From)))
	}
	if len(args) > 0 {
		r.literal = strings.NewReplacer(args...)
	}
	return r
}

// Pairs returns the effective, ordered literal rules.
func (r *Replacer) Pairs() []Pair { return r.pairs }

// Empty reports whether the replacer has no rules at all.
func (r *Replacer) Empty() bool { return r == nil || (len(r.pairs) == 0 && len(r.regexes) == 0) }

func (r *Replacer) mayMatch(s string) bool {
	if len(r.regexes) > 0 {
		return true
	}
	for _, n := range r.needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// mayMatchEscaped checks the SQL-escaped form of a value without unescaping.
// Escaping maps byte-for-byte, so a needle inside the value is always present
// in escaped form inside the raw bytes.
func (r *Replacer) mayMatchEscaped(raw []byte) bool {
	if len(r.regexes) > 0 {
		return true
	}
	for _, n := range r.escNeedles {
		if bytes.Contains(raw, n) {
			return true
		}
	}
	return false
}

func (r *Replacer) plain(s string) string {
	if r.literal != nil {
		s = r.literal.Replace(s)
	}
	for _, rx := range r.regexes {
		s = rx.Pattern.ReplaceAllString(s, rx.To)
	}
	return s
}

// Value rewrites one database value.
func (r *Replacer) Value(s string) string {
	return r.value(s, 0)
}

func (r *Replacer) value(s string, depth int) string {
	if r.Empty() || !r.mayMatch(s) {
		return s
	}
	if depth < 32 && looksSerialized(s) {
		if out, ok := r.rewriteSerialized(s, depth); ok {
			return out
		}
	}
	return r.plain(s)
}

func looksSerialized(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 2 {
		return false
	}
	if s == "N;" {
		return true
	}
	switch s[0] {
	case 'a', 'O', 's', 'i', 'b', 'd', 'C', 'E':
		return s[1] == ':' && (s[len(s)-1] == ';' || s[len(s)-1] == '}')
	}
	return false
}

type serParser struct {
	r     *Replacer
	s     string
	i     int
	out   strings.Builder
	depth int
}

func (r *Replacer) rewriteSerialized(s string, depth int) (string, bool) {
	lead := len(s) - len(strings.TrimLeft(s, " \t\r\n"))
	trail := len(s) - len(strings.TrimRight(s, " \t\r\n"))
	body := s[lead : len(s)-trail]
	p := &serParser{r: r, s: body, depth: depth}
	p.out.Grow(len(s) + 64)
	if !p.value() || p.i != len(body) {
		return "", false
	}
	return s[:lead] + p.out.String() + s[len(s)-trail:], true
}

func (p *serParser) expect(b byte) bool {
	if p.i < len(p.s) && p.s[p.i] == b {
		p.i++
		return true
	}
	return false
}

func (p *serParser) readInt() (int, bool) {
	start := p.i
	for p.i < len(p.s) && (p.s[p.i] >= '0' && p.s[p.i] <= '9' || p.s[p.i] == '-' && p.i == start) {
		p.i++
	}
	n, err := strconv.Atoi(p.s[start:p.i])
	return n, err == nil
}

// copyUntil copies through the next occurrence of stop (inclusive).
func (p *serParser) copyUntil(stop byte) bool {
	j := strings.IndexByte(p.s[p.i:], stop)
	if j < 0 {
		return false
	}
	p.out.WriteString(p.s[p.i : p.i+j+1])
	p.i += j + 1
	return true
}

// lengthPrefixed reads `<n>:"<n bytes>"` and returns the payload.
func (p *serParser) lengthPrefixed() (string, bool) {
	n, ok := p.readInt()
	if !ok || n < 0 || !p.expect(':') || !p.expect('"') || p.i+n > len(p.s) {
		return "", false
	}
	payload := p.s[p.i : p.i+n]
	p.i += n
	return payload, p.expect('"')
}

func (p *serParser) value() bool {
	if p.i >= len(p.s) {
		return false
	}
	t := p.s[p.i]
	switch t {
	case 'N':
		if p.expect('N') && p.expect(';') {
			p.out.WriteString("N;")
			return true
		}
		return false
	case 'b', 'i', 'd', 'r', 'R':
		if p.i+1 >= len(p.s) || p.s[p.i+1] != ':' {
			return false
		}
		return p.copyUntil(';')
	case 's', 'E':
		p.i++
		if !p.expect(':') {
			return false
		}
		payload, ok := p.lengthPrefixed()
		if !ok || !p.expect(';') {
			return false
		}
		if t == 's' {
			payload = p.r.value(payload, p.depth+1)
		}
		p.out.WriteByte(t)
		p.out.WriteByte(':')
		p.out.WriteString(strconv.Itoa(len(payload)))
		p.out.WriteString(":\"")
		p.out.WriteString(payload)
		p.out.WriteString("\";")
		return true
	case 'a':
		p.i++
		if !p.expect(':') {
			return false
		}
		n, ok := p.readInt()
		if !ok || n < 0 || !p.expect(':') || !p.expect('{') {
			return false
		}
		p.out.WriteString("a:" + strconv.Itoa(n) + ":{")
		return p.members(2*n) && p.closeBrace()
	case 'O':
		p.i++
		if !p.expect(':') {
			return false
		}
		class, ok := p.lengthPrefixed()
		if !ok || !p.expect(':') {
			return false
		}
		n, ok := p.readInt()
		if !ok || n < 0 || !p.expect(':') || !p.expect('{') {
			return false
		}
		p.out.WriteString("O:" + strconv.Itoa(len(class)) + ":\"" + class + "\":" + strconv.Itoa(n) + ":{")
		return p.members(2*n) && p.closeBrace()
	case 'C':
		p.i++
		if !p.expect(':') {
			return false
		}
		class, ok := p.lengthPrefixed()
		if !ok || !p.expect(':') {
			return false
		}
		n, ok := p.readInt()
		if !ok || n < 0 || !p.expect(':') || !p.expect('{') || p.i+n > len(p.s) {
			return false
		}
		data := p.r.plain(p.s[p.i : p.i+n])
		p.i += n
		if !p.expect('}') {
			return false
		}
		p.out.WriteString("C:" + strconv.Itoa(len(class)) + ":\"" + class + "\":" + strconv.Itoa(len(data)) + ":{" + data + "}")
		return true
	}
	return false
}

func (p *serParser) members(n int) bool {
	for k := 0; k < n; k++ {
		if !p.value() {
			return false
		}
	}
	return true
}

func (p *serParser) closeBrace() bool {
	if !p.expect('}') {
		return false
	}
	p.out.WriteByte('}')
	return true
}

// JSONVariants returns the JSON-escaped form of pairs whose values contain
// "/" (block attributes store URLs as https:\/\/example.com).
func JSONVariants(pairs []Pair) []Pair {
	var out []Pair
	for _, p := range pairs {
		if strings.Contains(p.From, "/") {
			out = append(out, Pair{
				From: strings.ReplaceAll(p.From, "/", `\/`),
				To:   strings.ReplaceAll(p.To, "/", `\/`),
			})
		}
	}
	return out
}

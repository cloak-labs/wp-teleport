// Package sqlstream rewrites mysqldump output on the fly: it renames tables,
// runs serialization-safe find/replace on string values, and maps collations
// between MySQL and MariaDB. It never buffers more than one line.
package sqlstream

import (
	"bufio"
	"bytes"
	"io"
	"strings"
)

// Options configures a rewrite. Table keys are source table names.
type Options struct {
	Rename       map[string]string
	Replacer     *Replacer
	Collations   map[string]string
	SkipColumns  map[string]map[string]bool
	ExactColumns map[string]map[string]map[string]string
	// Only, when set, drops every table section not in it (restoring part of
	// a snapshot).
	Only map[string]string
}

// Stats summarizes a rewrite.
type Stats struct {
	InBytes       int64 `json:"in_bytes"`
	OutBytes      int64 `json:"out_bytes"`
	Tables        int   `json:"tables"`
	InsertLines   int64 `json:"insert_lines"`
	ChangedValues int64 `json:"changed_values"`
	// Completed is true once mysqldump's trailer comment has been seen; a
	// stream without it was truncated.
	Completed bool `json:"completed"`
}

type rewriter struct {
	opt      Options
	coll     *strings.Replacer
	columns  map[string][]string
	cur      string
	inCreate bool
	inInsert bool
	insTable string
	insCols  []string
	section  string
	stats    Stats
	scratch  []byte
}

var (
	sectionPrefixes = [][]byte{
		[]byte("-- Table structure for table `"),
		[]byte("-- Dumping data for table `"),
		[]byte("DROP TABLE IF EXISTS `"),
	}
	dumpStartPrefixes = [][]byte{[]byte("-- MySQL dump"), []byte("-- MariaDB dump")}
)

// skipSection tracks which table the dump is in and reports whether line
// belongs to a table outside opt.Only.
func (rw *rewriter) skipSection(line []byte) bool {
	for _, p := range sectionPrefixes {
		if bytes.HasPrefix(line, p) {
			rest := line[len(p):]
			if end := bytes.IndexByte(rest, '`'); end > 0 {
				rw.section = string(rest[:end])
			}
			break
		}
	}
	for _, p := range dumpStartPrefixes {
		if bytes.HasPrefix(line, p) {
			rw.section = ""
		}
	}
	if rw.section == "" {
		return false
	}
	_, keep := rw.opt.Only[rw.section]
	return !keep
}

var (
	insertPrefix    = []byte("INSERT INTO `")
	createPrefix    = []byte("CREATE TABLE `")
	createPrefix2   = []byte("CREATE TABLE IF NOT EXISTS `")
	sandboxPrefix   = []byte("/*M!999999")
	valuesToken     = []byte(" VALUES")
	completedPrefix = []byte("-- Dump completed")
)

// Rewrite streams r to w, applying opt.
func Rewrite(r io.Reader, w io.Writer, opt Options) (Stats, error) {
	rw := &rewriter{opt: opt, columns: map[string][]string{}}
	if len(opt.Collations) > 0 {
		var args []string
		for from, to := range opt.Collations {
			args = append(args, from, to)
		}
		rw.coll = strings.NewReplacer(args...)
	}
	br := bufio.NewReaderSize(r, 1<<20)
	cw := &countingWriter{w: w}
	bw := bufio.NewWriterSize(cw, 1<<20)
	var long []byte
	for {
		line, err := br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			long = append(long[:0], line...)
			for err == bufio.ErrBufferFull {
				line, err = br.ReadSlice('\n')
				long = append(long, line...)
			}
			line = long
		}
		if len(line) > 0 {
			rw.stats.InBytes += int64(len(line))
			rw.line(bw, line)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return rw.stats, err
		}
	}
	if err := bw.Flush(); err != nil {
		return rw.stats, err
	}
	rw.stats.OutBytes = cw.n
	return rw.stats, nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func (rw *rewriter) line(w *bufio.Writer, line []byte) {
	if bytes.HasPrefix(line, completedPrefix) {
		rw.stats.Completed = true
	}
	if rw.opt.Only != nil && !rw.inInsert && rw.skipSection(line) {
		return
	}
	if rw.inInsert {
		// mariadb-dump 10.11+ puts each row of an extended INSERT on its own line.
		rw.insertValues(w, rw.insTable, rw.insCols, line)
		rw.inInsert = !endsStatement(line)
		return
	}
	switch {
	case bytes.HasPrefix(line, insertPrefix):
		rw.insert(w, line)
		return
	case bytes.HasPrefix(line, sandboxPrefix):
		// MariaDB 10.11.8+ sandbox marker; MySQL clients reject it.
		return
	case bytes.HasPrefix(line, createPrefix) || bytes.HasPrefix(line, createPrefix2):
		start := bytes.IndexByte(line, '`') + 1
		end := bytes.IndexByte(line[start:], '`')
		if end > 0 {
			rw.cur = string(line[start : start+end])
			rw.columns[rw.cur] = nil
			rw.inCreate = true
			rw.stats.Tables++
		}
	case rw.inCreate:
		if bytes.HasPrefix(line, []byte("  `")) {
			end := bytes.IndexByte(line[3:], '`')
			if end > 0 {
				rw.columns[rw.cur] = append(rw.columns[rw.cur], string(line[3:3+end]))
			}
		} else if bytes.HasPrefix(line, []byte(")")) {
			rw.inCreate = false
		}
	}
	out := rw.renameIdents(line)
	if rw.coll != nil {
		out = []byte(rw.coll.Replace(string(out)))
	}
	w.Write(out)
}

// renameIdents rewrites every backtick-quoted identifier found in the rename map.
func (rw *rewriter) renameIdents(line []byte) []byte {
	if len(rw.opt.Rename) == 0 || bytes.IndexByte(line, '`') < 0 {
		return line
	}
	out := rw.scratch[:0]
	i := 0
	for {
		a := bytes.IndexByte(line[i:], '`')
		if a < 0 {
			break
		}
		a += i
		b := bytes.IndexByte(line[a+1:], '`')
		if b < 0 {
			break
		}
		b += a + 1
		name := string(line[a+1 : b])
		out = append(out, line[i:a+1]...)
		if to, ok := rw.opt.Rename[name]; ok {
			out = append(out, to...)
		} else {
			out = append(out, name...)
		}
		out = append(out, '`')
		i = b + 1
	}
	out = append(out, line[i:]...)
	rw.scratch = out
	return out
}

func (rw *rewriter) insert(w *bufio.Writer, line []byte) {
	rw.stats.InsertLines++
	start := len(insertPrefix)
	end := bytes.IndexByte(line[start:], '`')
	if end < 0 {
		w.Write(line)
		return
	}
	table := string(line[start : start+end])
	w.Write(insertPrefix)
	if to, ok := rw.opt.Rename[table]; ok {
		w.WriteString(to)
	} else {
		w.WriteString(table)
	}
	rest := line[start+end:]
	cols := rw.columns[table]
	if bytes.HasPrefix(rest, []byte("` (")) {
		// --complete-insert column list
		close := bytes.IndexByte(rest, ')')
		if close > 0 {
			cols = nil
			for _, c := range bytes.Split(rest[3:close], []byte(",")) {
				cols = append(cols, strings.Trim(strings.TrimSpace(string(c)), "`"))
			}
		}
	}
	v := bytes.Index(rest, valuesToken)
	if v < 0 {
		w.Write(rest)
		return
	}
	v += len(valuesToken)
	w.Write(rest[:v])
	if !endsStatement(line) {
		rw.inInsert, rw.insTable, rw.insCols = true, table, cols
	}
	rw.insertValues(w, table, cols, rest[v:])
}

// endsStatement reports whether a dump line closes its SQL statement.
func endsStatement(line []byte) bool {
	return bytes.HasSuffix(bytes.TrimRight(line, "\r\n "), []byte(";"))
}

func (rw *rewriter) insertValues(w *bufio.Writer, table string, cols []string, b []byte) {
	if rw.opt.Replacer.Empty() && len(rw.opt.ExactColumns[table]) == 0 {
		w.Write(b)
		return
	}
	rw.values(w, table, cols, b)
}

func (rw *rewriter) values(w *bufio.Writer, table string, cols []string, b []byte) {
	skip := rw.opt.SkipColumns[table]
	exact := rw.opt.ExactColumns[table]
	col, depth, flush := 0, 0, 0
	for i := 0; i < len(b); i++ {
		switch b[i] {
		case '(':
			if depth == 0 {
				col = 0
			}
			depth++
		case ')':
			depth--
		case ',':
			if depth == 1 {
				col++
			}
		case '\'':
			j := i + 1
			hasEsc := false
			for j < len(b) {
				if b[j] == '\\' {
					hasEsc = true
					j += 2
					continue
				}
				if b[j] == '\'' {
					if j+1 < len(b) && b[j+1] == '\'' {
						hasEsc = true
						j += 2
						continue
					}
					break
				}
				j++
			}
			if j > len(b) {
				j = len(b)
			}
			w.Write(b[flush : i+1])
			raw := b[i+1 : j]
			name := ""
			if col < len(cols) {
				name = cols[col]
			}
			w.Write(rw.value(raw, hasEsc, name, skip, exact))
			flush = j
			i = j
		}
	}
	w.Write(b[flush:])
}

func (rw *rewriter) value(raw []byte, hasEsc bool, col string, skip map[string]bool, exact map[string]map[string]string) []byte {
	if m, ok := exact[col]; ok {
		val := unescapeIf(raw, hasEsc)
		if to, ok := m[val]; ok {
			rw.stats.ChangedValues++
			return Escape(nil, []byte(to))
		}
	}
	if skip[col] || rw.opt.Replacer.Empty() || !rw.opt.Replacer.mayMatchEscaped(raw) {
		return raw
	}
	val := unescapeIf(raw, hasEsc)
	nv := rw.opt.Replacer.Value(val)
	if nv == val {
		return raw
	}
	rw.stats.ChangedValues++
	return Escape(nil, []byte(nv))
}

func unescapeIf(raw []byte, hasEsc bool) string {
	if !hasEsc {
		return string(raw)
	}
	return string(Unescape(raw))
}

// Unescape decodes a MySQL string literal body.
func Unescape(raw []byte) []byte {
	out := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c == '\'' && i+1 < len(raw) && raw[i+1] == '\'' {
			out = append(out, '\'')
			i++
			continue
		}
		if c != '\\' || i+1 >= len(raw) {
			out = append(out, c)
			continue
		}
		i++
		switch raw[i] {
		case '0':
			out = append(out, 0)
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'b':
			out = append(out, '\b')
		case 'Z':
			out = append(out, 26)
		case '%', '_':
			out = append(out, '\\', raw[i])
		default:
			out = append(out, raw[i])
		}
	}
	return out
}

// Escape encodes b the way mysqldump does and appends it to dst.
func Escape(dst, b []byte) []byte {
	for _, c := range b {
		switch c {
		case 0:
			dst = append(dst, '\\', '0')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\'':
			dst = append(dst, '\\', '\'')
		case '"':
			dst = append(dst, '\\', '"')
		case 26:
			dst = append(dst, '\\', 'Z')
		default:
			dst = append(dst, c)
		}
	}
	return dst
}

// CollationMap returns the collation rewrites needed to import a dump from
// srcEngine into dstEngine ("mysql" or "mariadb").
func CollationMap(srcEngine, dstEngine, dstVersion string) map[string]string {
	if srcEngine == dstEngine {
		return nil
	}
	if dstEngine == "mariadb" {
		if versionAtLeast(dstVersion, 11, 4) {
			return nil
		}
		return map[string]string{
			"utf8mb4_0900_ai_ci": "utf8mb4_unicode_520_ci",
			"utf8mb4_0900_as_ci": "utf8mb4_unicode_520_ci",
			"utf8mb4_0900_as_cs": "utf8mb4_bin",
			"utf8mb4_0900_bin":   "utf8mb4_bin",
		}
	}
	return map[string]string{
		"utf8mb4_uca1400_ai_ci": "utf8mb4_0900_ai_ci",
		"utf8mb4_uca1400_as_cs": "utf8mb4_0900_as_cs",
		"utf8mb3_uca1400_ai_ci": "utf8mb3_general_ci",
	}
}

func versionAtLeast(v string, major, minor int) bool {
	var a, b int
	for i, part := range strings.SplitN(v, ".", 3) {
		n := 0
		for _, c := range part {
			if c < '0' || c > '9' {
				break
			}
			n = n*10 + int(c-'0')
		}
		if i == 0 {
			a = n
		} else if i == 1 {
			b = n
		}
	}
	return a > major || a == major && b >= minor
}

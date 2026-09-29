package sqlstream

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func urlReplacer() *Replacer {
	pairs := []Pair{
		{From: "https://sites.cloaklabs.co/ccc", To: "https://wp.localhost/ccc"},
		{From: "https://sites.cloaklabs.co/app/uploads/sites/26", To: "https://wp.localhost/app/uploads/sites/27"},
		{From: "https://sites.cloaklabs.co", To: "https://wp.localhost"},
	}
	return NewReplacer(append(pairs, JSONVariants(pairs)...), nil)
}

func TestSerializedLengthsAreFixed(t *testing.T) {
	r := urlReplacer()
	in := `a:2:{s:3:"url";s:30:"https://sites.cloaklabs.co/ccc";s:4:"list";a:1:{i:0;s:26:"https://sites.cloaklabs.co";}}`
	want := `a:2:{s:3:"url";s:24:"https://wp.localhost/ccc";s:4:"list";a:1:{i:0;s:20:"https://wp.localhost";}}`
	if got := r.Value(in); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestNestedSerializedAndObjects(t *testing.T) {
	r := urlReplacer()
	inner := `a:1:{s:1:"u";s:26:"https://sites.cloaklabs.co";}`
	in := `O:8:"stdClass":1:{s:4:"data";s:` + itoa(len(inner)) + `:"` + inner + `";}`
	got := r.Value(in)
	wantInner := `a:1:{s:1:"u";s:20:"https://wp.localhost";}`
	want := `O:8:"stdClass":1:{s:4:"data";s:` + itoa(len(wantInner)) + `:"` + wantInner + `";}`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestLongestRuleWins(t *testing.T) {
	r := urlReplacer()
	got := r.Value("https://sites.cloaklabs.co/app/uploads/sites/26/2024/01/a.jpg")
	if got != "https://wp.localhost/app/uploads/sites/27/2024/01/a.jpg" {
		t.Fatal(got)
	}
}

func TestJSONEscapedURLs(t *testing.T) {
	r := urlReplacer()
	in := `<!-- wp:image {"url":"https:\/\/sites.cloaklabs.co\/ccc\/x"} -->`
	want := `<!-- wp:image {"url":"https:\/\/wp.localhost\/ccc\/x"} -->`
	if got := r.Value(in); got != want {
		t.Fatalf("got %s", got)
	}
}

func TestBrokenSerializedFallsBackToPlain(t *testing.T) {
	r := urlReplacer()
	in := `a:1:{s:3:"url";s:99:"https://sites.cloaklabs.co";}`
	got := r.Value(in)
	if !strings.Contains(got, "https://wp.localhost") {
		t.Fatal(got)
	}
}

func TestRegex(t *testing.T) {
	r := NewReplacer(nil, []RegexPair{{Pattern: regexp.MustCompile(`foo(\d)`), To: "bar$1"}})
	if got := r.Value(`s:4:"foo1";`); got != `s:4:"bar1";` {
		t.Fatal(got)
	}
}

func TestEscapeRoundTrip(t *testing.T) {
	in := []byte("a'b\"c\\d\ne\rf\x00g\x1ah")
	if got := Unescape(Escape(nil, in)); !bytes.Equal(got, in) {
		t.Fatalf("%q", got)
	}
}

func TestRewriteDump(t *testing.T) {
	dump := strings.Join([]string{
		"/*M!999999\\- enable the sandbox mode */ ",
		"DROP TABLE IF EXISTS `wp_26_options`;",
		"CREATE TABLE `wp_26_options` (",
		"  `option_id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,",
		"  `option_name` varchar(191) COLLATE utf8mb4_0900_ai_ci NOT NULL DEFAULT '',",
		"  `option_value` longtext COLLATE utf8mb4_0900_ai_ci NOT NULL,",
		"  PRIMARY KEY (`option_id`)",
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;",
		"INSERT INTO `wp_26_options` VALUES (1,'siteurl','https://sites.cloaklabs.co/ccc/wp'),(2,'wp_26_user_roles','a:1:{s:4:\\\"link\\\";s:30:\\\"https://sites.cloaklabs.co/ccc\\\";}'),(3,'it''s','x');",
		"CREATE TABLE `wp_26_posts` (",
		"  `ID` bigint(20) unsigned NOT NULL AUTO_INCREMENT,",
		"  `guid` varchar(255) NOT NULL DEFAULT '',",
		"  `post_content` longtext NOT NULL,",
		") ENGINE=InnoDB;",
		"INSERT INTO `wp_26_posts` VALUES (5,'https://sites.cloaklabs.co/ccc/?p=5','see https://sites.cloaklabs.co/ccc (it\\'s here)');",
		"",
	}, "\n")
	pairs := append(urlReplacer().Pairs(), Pair{From: "wp_26_user_roles", To: "wp_27_user_roles"})
	opt := Options{
		Rename: map[string]string{
			"wp_26_options": "_tabc123_wp_27_options",
			"wp_26_posts":   "_tabc123_wp_27_posts",
		},
		Replacer:    NewReplacer(pairs, nil),
		Collations:  CollationMap("mysql", "mariadb", "10.7.8-MariaDB"),
		SkipColumns: map[string]map[string]bool{"wp_26_posts": {"guid": true}},
	}
	var out bytes.Buffer
	stats, err := Rewrite(strings.NewReader(dump), &out, opt)
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"DROP TABLE IF EXISTS `_tabc123_wp_27_options`;",
		"CREATE TABLE `_tabc123_wp_27_options` (",
		"COLLATE=utf8mb4_unicode_520_ci;",
		"(1,'siteurl','https://wp.localhost/ccc/wp')",
		`(2,'wp_27_user_roles','a:1:{s:4:\"link\";s:24:\"https://wp.localhost/ccc\";}')`,
		"(3,'it''s','x')",
		"(5,'https://sites.cloaklabs.co/ccc/?p=5','see https://wp.localhost/ccc (it\\'s here)')",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s\n---\n%s", want, got)
		}
	}
	if strings.Contains(got, "sandbox") || strings.Contains(got, "0900") {
		t.Errorf("sandbox marker or MySQL collation left in output:\n%s", got)
	}
	if stats.Tables != 2 || stats.ChangedValues != 4 {
		t.Errorf("stats %+v", stats)
	}
}

func TestMariaDBMultiLineInsert(t *testing.T) {
	dump := "CREATE TABLE `wp_39_options` (\n  `option_id` bigint,\n  `option_name` varchar(191),\n  `option_value` longtext\n) ENGINE=InnoDB;\n" +
		"INSERT INTO `wp_39_options` VALUES\n" +
		"(1,'home','https://sites.cloaklabs.co/ccc'),\n" +
		"(2,'wp_26_user_roles','a:1:{s:1:\\\"u\\\";s:26:\\\"https://sites.cloaklabs.co\\\";}');\n" +
		"CREATE TABLE `wp_39_posts` (\n  `ID` bigint\n) ENGINE=InnoDB;\n"
	pairs := append(urlReplacer().Pairs(), Pair{From: "wp_26_user_roles", To: "wp_27_user_roles"})
	var out bytes.Buffer
	_, err := Rewrite(strings.NewReader(dump), &out, Options{
		Rename:   map[string]string{"wp_39_options": "_tx_wp_35_options", "wp_39_posts": "_tx_wp_35_posts"},
		Replacer: NewReplacer(pairs, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"INSERT INTO `_tx_wp_35_options` VALUES\n",
		"(1,'home','https://wp.localhost/ccc'),\n",
		`(2,'wp_27_user_roles','a:1:{s:1:\"u\";s:20:\"https://wp.localhost\";}');` + "\n",
		"CREATE TABLE `_tx_wp_35_posts` (",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in\n%s", want, out.String())
		}
	}
}

func TestOnlyKeepsSelectedSections(t *testing.T) {
	dump := "-- MySQL dump 10.13\n/*!40101 SET NAMES utf8mb4 */;\n" +
		"--\n-- Table structure for table `wp_2_posts`\n--\n\nDROP TABLE IF EXISTS `wp_2_posts`;\n" +
		"CREATE TABLE `wp_2_posts` (\n  `ID` bigint\n) ENGINE=InnoDB;\n" +
		"INSERT INTO `wp_2_posts` VALUES\n(1),\n(2);\n" +
		"--\n-- Table structure for table `wp_3_posts`\n--\n\nDROP TABLE IF EXISTS `wp_3_posts`;\n" +
		"CREATE TABLE `wp_3_posts` (\n  `ID` bigint\n) ENGINE=InnoDB;\n" +
		"INSERT INTO `wp_3_posts` VALUES\n(7),\n(8);\n" +
		"/*!40101 SET SQL_MODE=@OLD_SQL_MODE */;\n-- Dump completed on 2026-01-01\n" +
		"-- MySQL dump 10.13\n/*!40101 SET NAMES utf8mb4 */;\n"
	var out bytes.Buffer
	st, err := Rewrite(strings.NewReader(dump), &out, Options{
		Rename: map[string]string{"wp_3_posts": "_tx_wp_5_posts"},
		Only:   map[string]string{"wp_3_posts": "_tx_wp_5_posts"},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if strings.Contains(s, "wp_2_posts") || strings.Contains(s, "(1)") {
		t.Errorf("unselected table leaked:\n%s", s)
	}
	for _, want := range []string{"CREATE TABLE `_tx_wp_5_posts`", "(7),\n(8);", "/*!40101 SET NAMES utf8mb4 */;"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	if strings.Count(s, "SET NAMES") != 2 || !st.Completed {
		t.Errorf("dump headers must survive and completion be seen:\n%s", s)
	}
}

func TestExactColumns(t *testing.T) {
	dump := "CREATE TABLE `wp_blogs` (\n  `blog_id` bigint,\n  `domain` varchar(200),\n  `path` varchar(100)\n) ENGINE=InnoDB;\nINSERT INTO `wp_blogs` VALUES (1,'sites.cloaklabs.co','/'),(2,'sites.cloaklabs.co','/ccc/');\n"
	var out bytes.Buffer
	_, err := Rewrite(strings.NewReader(dump), &out, Options{
		ExactColumns: map[string]map[string]map[string]string{
			"wp_blogs": {"domain": {"sites.cloaklabs.co": "wp.localhost"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "(1,'wp.localhost','/'),(2,'wp.localhost','/ccc/')") {
		t.Fatal(out.String())
	}
}

func TestGoldenFixtures(t *testing.T) {
	files, _ := filepath.Glob("testdata/*.in.sql")
	for _, in := range files {
		want, err := os.ReadFile(strings.TrimSuffix(in, ".in.sql") + ".out.sql")
		if err != nil {
			t.Fatal(err)
		}
		src, _ := os.ReadFile(in)
		var out bytes.Buffer
		_, err = Rewrite(bytes.NewReader(src), &out, Options{
			Rename:   map[string]string{"wp_26_postmeta": "wp_27_postmeta"},
			Replacer: urlReplacer(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if out.String() != string(want) {
			t.Errorf("%s mismatch\n--- got\n%s", in, out.String())
		}
	}
}

func FuzzEscapeRoundTrip(f *testing.F) {
	f.Add([]byte("hello 'world' \\ \n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		if got := Unescape(Escape(nil, b)); !bytes.Equal(got, b) {
			t.Fatalf("%q -> %q", b, got)
		}
	})
}

// FuzzSerialized checks that rewriting valid serialized data always yields
// valid serialized data (every s:N: length matches its payload).
func FuzzSerialized(f *testing.F) {
	f.Add("https://sites.cloaklabs.co/ccc", "x")
	f.Add("", "https://sites.cloaklabs.co")
	r := urlReplacer()
	check := NewReplacer([]Pair{{From: "\x00never\x00", To: "y"}}, nil)
	f.Fuzz(func(t *testing.T, a, b string) {
		in := `a:2:{i:0;s:` + itoa(len(a)) + `:"` + a + `";i:1;s:` + itoa(len(b)) + `:"` + b + `";}`
		out := r.Value(in)
		if _, ok := check.rewriteSerialized(out, 0); !ok {
			t.Fatalf("invalid serialized output %q", out)
		}
	})
}

func itoa(n int) string { return strconv.Itoa(n) }

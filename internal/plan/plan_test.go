package plan

import (
	"strconv"
	"strings"
	"testing"

	"github.com/cloak-labs/wp-teleport/internal/agent"
	"github.com/cloak-labs/wp-teleport/internal/config"
)

func site(id int, domain, path, root string) agent.Site {
	prefix := "wp_"
	uploads := "/app/uploads"
	if id != 1 {
		prefix = "wp_" + strconv.Itoa(id) + "_"
		uploads += "/sites/" + strconv.Itoa(id)
	}
	home := strings.TrimRight("https://"+domain+path, "/")
	return agent.Site{
		ID: id, Domain: domain, Path: path, Home: home, SiteURL: home,
		UploadsDir: root + "/public" + uploads, UploadsURL: "https://" + domain + uploads,
		Prefix: prefix,
	}
}

func network(domain, root, host string, sites ...agent.Site) *agent.Manifest {
	m := &agent.Manifest{
		Multisite: true, BasePrefix: "wp_",
		Network:      &agent.Network{Domain: domain, Path: "/", MainSite: 1},
		GlobalTables: []string{"wp_users", "wp_usermeta", "wp_blogs", "wp_site", "wp_sitemeta"},
		ABSPATH:      root + "/public/wp/", ContentDir: root + "/public/app",
		PluginDir: root + "/public/app/plugins", MuPluginDir: root + "/public/app/mu-plugins",
		ThemeRoot: root + "/public/app/themes",
		DB:        agent.DB{Name: "wp", Server: host, Engine: "mysql", Version: "8.0.40"},
		Sites:     sites,
	}
	for _, g := range m.GlobalTables {
		m.Tables = append(m.Tables, agent.Table{Name: g, Bytes: 100})
	}
	for _, s := range sites {
		for _, t := range []string{"posts", "postmeta", "options", "comments"} {
			m.Tables = append(m.Tables, agent.Table{Name: s.Prefix + t, Bytes: int64(1000 + len(t))})
		}
	}
	return m
}

func prod() *agent.Manifest {
	return network("sites.example.com", "/srv/prod", "prod-db",
		site(1, "sites.example.com", "/", "/srv/prod"),
		site(11, "sites.example.com", "/shop/", "/srv/prod"),
		site(12, "sites.example.com", "/blog/", "/srv/prod"))
}

func local() *agent.Manifest {
	return network("wp.localhost", "/var/www/html", "local-db",
		site(1, "wp.localhost", "/", "/var/www/html"),
		site(26, "wp.localhost", "/shop/", "/var/www/html"))
}

func mustBuild(t *testing.T, src, dst *agent.Manifest, sel config.Selection) *Plan {
	t.Helper()
	p, err := Build(src, dst, sel, &config.Config{}, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func pairMap(p *Plan) map[string]string {
	m := map[string]string{}
	for _, pr := range p.Pairs {
		m[pr.From] = pr.To
	}
	return m
}

func TestSitesMatchedBySlugNotID(t *testing.T) {
	p := mustBuild(t, prod(), local(), config.Selection{Sites: []string{"shop"}, DB: true, Media: true})
	if len(p.Sites) != 1 || p.Sites[0].Dst.ID != 26 {
		t.Fatalf("shop should map 11 -> 26, got %+v", p.Sites)
	}
	lives := map[string]string{}
	for _, tj := range p.Tables {
		lives[tj.Src] = tj.Live
		if !strings.HasPrefix(tj.Tmp, "_tabc123_") {
			t.Errorf("tmp name %q", tj.Tmp)
		}
	}
	if lives["wp_11_posts"] != "wp_26_posts" || len(lives) != 4 {
		t.Errorf("tables %v", lives)
	}
	if _, ok := lives["wp_users"]; ok {
		t.Error("global tables must not be copied with --sites")
	}
	pm := pairMap(p)
	for from, to := range map[string]string{
		"https://sites.example.com/shop":                 "https://wp.localhost/shop",
		"https://sites.example.com/app/uploads/sites/11": "https://wp.localhost/app/uploads/sites/26",
		"/srv/prod/public/app/uploads/sites/11":          "/var/www/html/public/app/uploads/sites/26",
		"wp_11_user_roles":                               "wp_26_user_roles",
		"//sites.example.com":                            "//wp.localhost",
		`https:\/\/sites.example.com\/shop`:              `https:\/\/wp.localhost\/shop`,
	} {
		if pm[from] != to {
			t.Errorf("replace %q => %q, got %q", from, to, pm[from])
		}
	}
	if len(p.Files) != 1 || p.Files[0].Dst != "/var/www/html/public/app/uploads/sites/26" {
		t.Errorf("files %+v", p.Files)
	}
	if len(p.PreservePairs) != 1 || p.PreservePairs[0].Live != "wp_26_options" {
		t.Errorf("preserve pairs %+v", p.PreservePairs)
	}
}

func TestUsersCopiesGlobalTables(t *testing.T) {
	p := mustBuild(t, prod(), local(), config.Selection{Sites: []string{"shop"}, Users: true})
	lives := map[string]string{}
	for _, tj := range p.Tables {
		lives[tj.Src] = tj.Live
	}
	if lives["wp_users"] != "wp_users" || lives["wp_usermeta"] != "wp_usermeta" {
		t.Errorf("users tables %v", lives)
	}
	if _, ok := lives["wp_11_posts"]; ok {
		t.Error("--users without --db should not copy site tables")
	}
	p = mustBuild(t, prod(), local(), config.Selection{Sites: []string{"shop"}, DB: true, Users: true})
	var posts, users bool
	for _, tj := range p.Tables {
		if tj.Src == "wp_11_posts" {
			posts = true
		}
		if tj.Src == "wp_users" {
			users = true
		}
	}
	if !posts || !users {
		t.Error("--db --users should copy both site tables and users")
	}
}

func TestMissingSite(t *testing.T) {
	_, err := Build(prod(), local(), config.Selection{Sites: []string{"blog"}, DB: true}, &config.Config{}, "r")
	if err == nil || !strings.Contains(err.Error(), "--create-site") {
		t.Fatalf("want create-site hint, got %v", err)
	}
	p := mustBuild(t, prod(), local(), config.Selection{Sites: []string{"blog"}, DB: true, CreateSite: true})
	if len(p.NeedsCreate()) != 1 || len(p.Tables) != 0 {
		t.Errorf("want one site to create and no jobs yet, got %+v", p)
	}
}

func TestAsRenamesDestination(t *testing.T) {
	p := mustBuild(t, prod(), local(), config.Selection{Sites: []string{"blog"}, As: "shop", DB: true})
	if p.Sites[0].Dst.ID != 26 || pairMap(p)["https://sites.example.com/blog"] != "https://wp.localhost/shop" {
		t.Errorf("blog --as shop: %+v %v", p.Sites, pairMap(p))
	}
}

func TestMultisiteNeedsSelection(t *testing.T) {
	if _, err := Build(prod(), local(), config.Selection{DB: true}, &config.Config{}, "r"); err == nil {
		t.Fatal("expected an error without --sites or --network")
	}
}

func TestSameDatabaseRefused(t *testing.T) {
	src, dst := prod(), prod()
	if _, err := Build(src, dst, config.Selection{Sites: []string{"shop"}, DB: true}, &config.Config{}, "r"); err == nil {
		t.Fatal("expected same-database refusal")
	}
}

func TestNetworkMode(t *testing.T) {
	p := mustBuild(t, prod(), local(), config.Selection{Network: true, DB: true, Media: true})
	var hasUsers bool
	for _, tj := range p.Tables {
		if tj.Src == "wp_users" {
			hasUsers = true
		}
		if tj.Src == "wp_11_posts" && tj.Live != "wp_11_posts" {
			t.Errorf("network mode keeps IDs, got %s", tj.Live)
		}
	}
	if !hasUsers {
		t.Error("network mode copies users")
	}
	if p.ExactColumns["wp_blogs"]["domain"]["sites.example.com"] != "wp.localhost" {
		t.Error("blogs.domain should be mapped exactly")
	}
	if len(p.Warnings) != 0 {
		t.Errorf("unexpected warnings %v", p.Warnings)
	}
	if up := mustBuild(t, local(), prod(), config.Selection{Network: true, DB: true}); len(up.Warnings) != 1 || !strings.Contains(up.Warnings[0], `"blog"`) {
		t.Errorf("expected a warning that blog will be dropped, got %v", up.Warnings)
	}
	if len(p.Files) != 1 || p.Files[0].Label != "uploads (network)" {
		t.Errorf("files %+v", p.Files)
	}
}

func TestMainSiteMediaExcludesSubsites(t *testing.T) {
	p := mustBuild(t, prod(), local(), config.Selection{Sites: []string{"main"}, Media: true})
	if !contains(p.Files[0].Exclude, "/sites/") {
		t.Errorf("main site uploads must exclude /sites/: %v", p.Files[0].Exclude)
	}
}

func TestFilters(t *testing.T) {
	p := mustBuild(t, prod(), local(), config.Selection{
		Sites: []string{"shop"}, Tables: []string{"posts", "postmeta", "options"},
		ExcludePostTypes: []string{"revision"}, ExcludeTransients: true, SkipGUIDs: true,
		Delete: true, MediaSince: "2025", Media: true,
	})
	w := map[string]string{}
	for _, tj := range p.Tables {
		w[tj.Short] = tj.Where
	}
	if len(w) != 3 {
		t.Errorf("tables %v", w)
	}
	if w["posts"] != "post_type NOT IN ('revision')" ||
		!strings.Contains(w["postmeta"], "SELECT ID FROM `wp_11_posts`") ||
		!strings.Contains(w["options"], `\_transient\_%`) {
		t.Errorf("where %v", w)
	}
	if !p.SkipColumns["wp_11_posts"]["guid"] {
		t.Error("guid should be skipped")
	}
	if p.Files[0].Delete {
		t.Error("--delete must be dropped with --media-since")
	}
	if _, err := Build(prod(), local(), config.Selection{Sites: []string{"shop"}, DB: true, ExcludePostTypes: []string{"x') OR 1=1 -- "}}, &config.Config{}, "r"); err == nil {
		t.Error("unsafe post type accepted")
	}
}

func TestReplaceFlags(t *testing.T) {
	if _, err := Build(prod(), local(), config.Selection{Sites: []string{"shop"}, DB: true, Replace: []string{"nope"}}, &config.Config{}, "r"); err == nil {
		t.Error("malformed --replace accepted")
	}
	cfg := &config.Config{Replace: []string{"cdn.old=>cdn.new"}}
	p, err := Build(prod(), local(), config.Selection{Sites: []string{"shop"}, DB: true, Regex: []string{`v\d+=>v0`}}, cfg, "r")
	if err != nil {
		t.Fatal(err)
	}
	if pairMap(p)["cdn.old"] != "cdn.new" || len(p.Regexes) != 1 {
		t.Errorf("pairs %v regexes %d", pairMap(p), len(p.Regexes))
	}
}

func TestSingleSiteIntoNetwork(t *testing.T) {
	single := &agent.Manifest{
		BasePrefix: "wp_", GlobalTables: []string{"wp_users", "wp_usermeta"},
		ContentDir: "/srv/single/wp-content", ABSPATH: "/srv/single/",
		DB:     agent.DB{Name: "single", Server: "s"},
		Sites:  []agent.Site{{ID: 1, Domain: "old.test", Path: "/", Home: "https://old.test", SiteURL: "https://old.test", Prefix: "wp_"}},
		Tables: []agent.Table{{Name: "wp_posts"}, {Name: "wp_users"}, {Name: "wp_usermeta"}, {Name: "wp_options"}},
	}
	if _, err := Build(single, local(), config.Selection{DB: true}, &config.Config{}, "r"); err == nil {
		t.Fatal("expected --as to be required")
	}
	p := mustBuild(t, single, local(), config.Selection{DB: true, As: "shop"})
	for _, tj := range p.Tables {
		if tj.Src == "wp_users" {
			t.Error("users must be skipped when copying into a network")
		}
		if tj.Src == "wp_posts" && tj.Live != "wp_26_posts" {
			t.Errorf("wp_posts -> %s", tj.Live)
		}
	}
	if pairMap(p)["wp_user_roles"] != "wp_26_user_roles" {
		t.Errorf("user_roles not remapped: %v", pairMap(p))
	}
}

func TestTableGlobs(t *testing.T) {
	src := prod()
	src.Tables = append(src.Tables, agent.Table{Name: "wp_11_gf_entry"}, agent.Table{Name: "wp_11_gf_form"})
	p := mustBuild(t, src, local(), config.Selection{Sites: []string{"shop"}, Tables: []string{"gf_*", "options"}})
	if len(p.Tables) != 3 {
		t.Errorf("gf_* + options: %+v", p.Tables)
	}
	p = mustBuild(t, src, local(), config.Selection{Sites: []string{"shop"}, DB: true, ExcludeTables: []string{"wp_11_gf_*", "comments"}})
	for _, tj := range p.Tables {
		if strings.Contains(tj.Src, "gf_") || tj.Short == "comments" {
			t.Errorf("excluded %s", tj.Src)
		}
	}
	if _, err := Build(src, local(), config.Selection{Sites: []string{"shop"}, Tables: []string{"["}}, &config.Config{}, "r"); err == nil {
		t.Error("bad glob accepted")
	}
}

func TestExportPlan(t *testing.T) {
	p, err := BuildExport(prod(), config.Selection{Sites: []string{"shop"}, DB: true}, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tj := range p.Tables {
		if tj.Src != tj.Live {
			t.Errorf("export must keep names: %s -> %s", tj.Src, tj.Live)
		}
	}
	if len(p.Pairs) != 0 || len(p.Tables) != 4 {
		t.Errorf("pairs %v tables %d", p.Pairs, len(p.Tables))
	}
}

func TestSubdomainAndMappedSlugs(t *testing.T) {
	m := &agent.Manifest{
		Multisite: true,
		Network:   &agent.Network{Domain: "example.com", Path: "/", MainSite: 1},
		Sites: []agent.Site{
			{ID: 1, Domain: "example.com", Path: "/"},
			{ID: 2, Domain: "shop.example.com", Path: "/"},
			{ID: 3, Domain: "client.org", Path: "/"},
			{ID: 4, Domain: "example.com", Path: "/blog/"},
		},
	}
	m.AssignSlugs()
	var got []string
	for _, s := range m.Sites {
		got = append(got, s.Slug())
	}
	if strings.Join(got, ",") != "main,shop,client.org,blog" {
		t.Errorf("slugs %v", got)
	}
}

func TestTmpNameLength(t *testing.T) {
	long := strings.Repeat("x", 80)
	if n := TmpName("abc123", long); len(n) > 64 || !strings.HasPrefix(n, "_tabc123_") {
		t.Errorf("tmp name %q", n)
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

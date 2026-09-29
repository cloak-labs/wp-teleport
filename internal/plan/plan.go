// Package plan turns a Selection plus two environment manifests into a
// concrete list of table and file jobs, replacement rules, and warnings.
package plan

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/cloak-labs/wp-teleport/internal/agent"
	"github.com/cloak-labs/wp-teleport/internal/config"
	"github.com/cloak-labs/wp-teleport/internal/sqlstream"
)

type SiteMap struct {
	Src    agent.Site  `json:"src"`
	Dst    *agent.Site `json:"dst"`
	Slug   string      `json:"slug"`
	Create bool        `json:"create,omitempty"`
}

type TableJob struct {
	Src   string `json:"src"`
	Short string `json:"short"`
	Live  string `json:"live"`
	Tmp   string `json:"tmp"`
	Where string `json:"where,omitempty"`
	Bytes int64  `json:"bytes"`
	Rows  int64  `json:"rows"`
	// Unchanged is set when checksums show the table is already in sync.
	Unchanged bool `json:"unchanged,omitempty"`
}

type FileJob struct {
	Label   string   `json:"label"`
	Src     string   `json:"src"`
	Dst     string   `json:"dst"`
	Exclude []string `json:"exclude,omitempty"`
	Delete  bool     `json:"delete,omitempty"`
	Since   string   `json:"since,omitempty"`
}

type PreservePair struct {
	Tmp  string `json:"tmp"`
	Live string `json:"live"`
}

type Plan struct {
	Run           string                                  `json:"run"`
	Network       bool                                    `json:"network,omitempty"`
	Sites         []SiteMap                               `json:"sites"`
	Tables        []TableJob                              `json:"tables"`
	Pairs         []sqlstream.Pair                        `json:"replace"`
	Regexes       []sqlstream.RegexPair                   `json:"-"`
	SkipColumns   map[string]map[string]bool              `json:"-"`
	ExactColumns  map[string]map[string]map[string]string `json:"-"`
	Collations    map[string]string                       `json:"collations,omitempty"`
	Preserve      []string                                `json:"preserve,omitempty"`
	PreservePairs []PreservePair                          `json:"-"`
	Files         []FileJob                               `json:"files"`
	Warnings      []string                                `json:"warnings,omitempty"`
}

// NeedsCreate lists destination sites that must be created before running.
func (p *Plan) NeedsCreate() []SiteMap {
	var out []SiteMap
	for _, s := range p.Sites {
		if s.Create {
			out = append(out, s)
		}
	}
	return out
}

func (p *Plan) warn(format string, args ...any) {
	p.Warnings = append(p.Warnings, fmt.Sprintf(format, args...))
}

var safeIdent = regexp.MustCompile(`^[A-Za-z0-9_\-]+$`)

// TmpName is the import-time name for a live table.
func TmpName(run, live string) string {
	name := "_t" + run + "_" + live
	if len(name) <= 64 {
		return name
	}
	sum := md5.Sum([]byte(live))
	return "_t" + run + "_" + hex.EncodeToString(sum[:])[:20]
}

// Build computes the plan. Sites the destination lacks are marked Create and
// get no jobs; callers create them and call Build again.
func Build(src, dst *agent.Manifest, sel config.Selection, cfg *config.Config, run string) (*Plan, error) {
	return build(src, dst, sel, cfg, run, false)
}

// BuildExport plans a dump of src's selected tables with no destination:
// every site maps onto itself, so no names or values are rewritten.
func BuildExport(src *agent.Manifest, sel config.Selection, cfg *config.Config) (*Plan, error) {
	sel.CreateSite, sel.As = false, ""
	return build(src, src, sel, cfg, "export", true)
}

// BuildRestore plans importing a snapshot taken from src into dst. Unlike
// Build it allows src and dst to be the same database (restoring a backup).
func BuildRestore(src, dst *agent.Manifest, sel config.Selection, cfg *config.Config, run string) (*Plan, error) {
	return build(src, dst, sel, cfg, run, true)
}

func build(src, dst *agent.Manifest, sel config.Selection, cfg *config.Config, run string, export bool) (*Plan, error) {
	p := &Plan{
		Run:          run,
		Network:      sel.Network,
		SkipColumns:  map[string]map[string]bool{},
		ExactColumns: map[string]map[string]map[string]string{},
	}
	if !export && src.DB.Server == dst.DB.Server && src.DB.Name == dst.DB.Name && src.BasePrefix == dst.BasePrefix {
		return nil, fmt.Errorf("source and destination are the same database (%s on %s). Is local WordPress attached to a remote DB (wp:db)?", src.DB.Name, src.DB.Server)
	}
	for _, pt := range sel.ExcludePostTypes {
		if !safeIdent.MatchString(pt) {
			return nil, fmt.Errorf("invalid post type %q", pt)
		}
	}
	if err := p.resolveSites(src, dst, sel); err != nil {
		return nil, err
	}
	if sel.HasDB() || sel.Users {
		if err := p.tables(src, dst, sel); err != nil {
			return nil, err
		}
	}
	if err := p.replacements(src, dst, sel, cfg); err != nil {
		return nil, err
	}
	p.Collations = sqlstream.CollationMap(src.DB.Engine, dst.DB.Engine, dst.DB.Version)
	p.Preserve = uniq(append(append(append([]string{}, config.DefaultPreserve...), cfg.Preserve...), sel.Preserve...))
	for _, t := range p.Tables {
		if t.Short == "options" {
			p.PreservePairs = append(p.PreservePairs, PreservePair{Tmp: t.Tmp, Live: t.Live})
		}
	}
	p.files(src, dst, sel, cfg)
	return p, nil
}

func (p *Plan) resolveSites(src, dst *agent.Manifest, sel config.Selection) error {
	if sel.Network {
		if src.Multisite != dst.Multisite {
			return fmt.Errorf("--network needs both environments to be multisite (or both single sites)")
		}
		if len(sel.Sites) > 0 {
			return fmt.Errorf("use either --network or --sites, not both")
		}
		for _, s := range src.Sites {
			m := SiteMap{Src: s, Slug: s.Slug()}
			if d, ok := siteBySlug(dst, s.Slug()); ok {
				m.Dst = &d
			}
			p.Sites = append(p.Sites, m)
		}
		for _, d := range dst.Sites {
			if _, ok := siteBySlug(src, d.Slug()); !ok {
				p.warn("destination site %q does not exist on the source and will be dropped from the network", d.Slug())
			}
		}
		return nil
	}
	keys := sel.Sites
	if len(keys) == 0 {
		if src.Multisite {
			return fmt.Errorf("source is a multisite network: choose --sites=<slug>[,<slug>] or --network (sites: %s)", siteList(src))
		}
		keys = []string{"main"}
	}
	if sel.As != "" && len(keys) > 1 {
		return fmt.Errorf("--as works with a single --sites value")
	}
	for _, key := range keys {
		s, ok := src.Site(key)
		if !ok {
			if !src.Multisite && len(src.Sites) == 1 {
				s = src.Sites[0]
			} else {
				return fmt.Errorf("site %q not found on the source (sites: %s)", key, siteList(src))
			}
		}
		slug := s.Slug()
		if sel.As != "" {
			slug = sel.As
		}
		m := SiteMap{Src: s, Slug: slug}
		switch {
		case !dst.Multisite:
			d := dst.Sites[0]
			m.Dst = &d
		default:
			if !src.Multisite && sel.As == "" {
				return fmt.Errorf("destination is a multisite network: pass --as=<slug> to choose the target site")
			}
			if d, ok := siteBySlug(dst, slug); ok {
				m.Dst = &d
			} else if sel.CreateSite {
				m.Create = true
			} else {
				return fmt.Errorf("site %q does not exist on the destination; pass --create-site to create it", slug)
			}
		}
		p.Sites = append(p.Sites, m)
	}
	return nil
}

func siteBySlug(m *agent.Manifest, slug string) (agent.Site, bool) {
	for _, s := range m.Sites {
		if s.Slug() == slug {
			return s, true
		}
	}
	return agent.Site{}, false
}

func siteList(m *agent.Manifest) string {
	var names []string
	for _, s := range m.Sites {
		names = append(names, s.Slug())
	}
	return strings.Join(names, ", ")
}

// SiteTables returns the tables that belong to one site.
func SiteTables(m *agent.Manifest, site agent.Site) []agent.Table {
	global := map[string]bool{}
	for _, g := range m.GlobalTables {
		global[g] = true
	}
	var out []agent.Table
	for _, t := range m.Tables {
		if t.View || !strings.HasPrefix(t.Name, m.BasePrefix) {
			continue
		}
		if !m.Multisite {
			out = append(out, t)
			continue
		}
		owner := tableOwner(m, t.Name)
		if global[t.Name] {
			continue
		}
		if owner == site.ID {
			out = append(out, t)
		}
	}
	return out
}

// tableOwner returns the blog ID that owns a prefixed table (1 when unnumbered).
func tableOwner(m *agent.Manifest, name string) int {
	rest := strings.TrimPrefix(name, m.BasePrefix)
	i := strings.IndexByte(rest, '_')
	if i > 0 {
		if id, err := strconv.Atoi(rest[:i]); err == nil {
			for _, s := range m.Sites {
				if s.ID == id && id != 1 {
					return id
				}
			}
		}
	}
	return 1
}

func (p *Plan) tables(src, dst *agent.Manifest, sel config.Selection) error {
	for _, pat := range append(append([]string{}, sel.Tables...), sel.ExcludeTables...) {
		if _, err := path.Match(pat, ""); err != nil {
			return fmt.Errorf("bad table pattern %q: %w", pat, err)
		}
	}
	// add queues a table; srcPrefix is the owning site's prefix, live the
	// destination name.
	add := func(t agent.Table, srcPrefix, live string) {
		short := strings.TrimPrefix(t.Name, srcPrefix)
		if len(sel.Tables) > 0 && !matchAny(sel.Tables, short, t.Name) {
			return
		}
		if matchAny(sel.ExcludeTables, short, t.Name) {
			return
		}
		p.Tables = append(p.Tables, TableJob{
			Src: t.Name, Short: short, Live: live, Tmp: TmpName(p.Run, live),
			Where: where(short, srcPrefix, sel), Bytes: t.Bytes, Rows: t.Rows,
		})
	}
	if sel.Network {
		for _, t := range src.Tables {
			if t.View || !strings.HasPrefix(t.Name, src.BasePrefix) {
				continue
			}
			prefix := src.BasePrefix
			if owner := tableOwner(src, t.Name); owner != 1 {
				prefix = fmt.Sprintf("%s%d_", src.BasePrefix, owner)
			}
			add(t, prefix, dst.BasePrefix+strings.TrimPrefix(t.Name, src.BasePrefix))
		}
		b := src.BasePrefix
		host := func(m *agent.Manifest) string {
			if m.Network != nil {
				return m.Network.Domain
			}
			return ""
		}
		if host(src) != host(dst) && host(dst) != "" {
			exact := map[string]map[string]string{"domain": {host(src): host(dst)}}
			p.ExactColumns[b+"blogs"] = exact
			p.ExactColumns[b+"site"] = exact
		}
	} else {
		globalSkipped := false
		if sel.HasDB() {
			for _, m := range p.Sites {
				if m.Create || m.Dst == nil {
					continue
				}
				for _, t := range SiteTables(src, m.Src) {
					if !src.Multisite && dst.Multisite && isGlobal(src, t.Name) {
						globalSkipped = true
						continue
					}
					add(t, m.Src.Prefix, m.Dst.Prefix+strings.TrimPrefix(t.Name, m.Src.Prefix))
				}
			}
		}
		if sel.Users && src.Multisite {
			for _, name := range []string{src.BasePrefix + "users", src.BasePrefix + "usermeta"} {
				t, ok := src.Table(name)
				if !ok {
					continue
				}
				add(t, src.BasePrefix, dst.BasePrefix+strings.TrimPrefix(t.Name, src.BasePrefix))
			}
			p.warn("--users overwrites the network-wide users tables on the destination")
		} else if src.Multisite && len(sel.Tables) == 0 {
			p.warn("users are network-wide and are not copied with --sites; pass --users to copy them, or authors must already exist on the destination")
		}
		if globalSkipped {
			p.warn("users tables are skipped when copying a single site into a network")
		}
	}
	if sel.SkipGUIDs {
		for _, t := range p.Tables {
			if t.Short == "posts" {
				p.SkipColumns[t.Src] = map[string]bool{"guid": true}
			}
		}
	}
	seen := map[string]string{}
	for _, t := range p.Tables {
		if prev, dup := seen[t.Live]; dup {
			return fmt.Errorf("tables %s and %s would both become %s", prev, t.Src, t.Live)
		}
		seen[t.Live] = t.Src
	}
	sort.SliceStable(p.Tables, func(i, j int) bool { return p.Tables[i].Bytes > p.Tables[j].Bytes })
	return nil
}

func isGlobal(m *agent.Manifest, name string) bool {
	for _, g := range m.GlobalTables {
		if g == name {
			return true
		}
	}
	return false
}

// where returns the mysqldump --where filter for a table, if any.
func where(short, prefix string, sel config.Selection) string {
	var conds []string
	types := ""
	if len(sel.ExcludePostTypes) > 0 {
		types = "'" + strings.Join(sel.ExcludePostTypes, "','") + "'"
	}
	excludedPosts := "SELECT ID FROM `" + prefix + "posts` WHERE post_type IN (" + types + ")"
	switch short {
	case "posts":
		if types != "" {
			conds = append(conds, "post_type NOT IN ("+types+")")
		}
	case "postmeta":
		if types != "" {
			conds = append(conds, "post_id NOT IN ("+excludedPosts+")")
		}
	case "term_relationships":
		if types != "" {
			conds = append(conds, "object_id NOT IN ("+excludedPosts+")")
		}
	case "comments":
		if types != "" {
			conds = append(conds, "comment_post_ID NOT IN ("+excludedPosts+")")
		}
		if sel.ExcludeSpam {
			conds = append(conds, "comment_approved <> 'spam'")
		}
	case "commentmeta":
		if sel.ExcludeSpam {
			conds = append(conds, "comment_id NOT IN (SELECT comment_ID FROM `"+prefix+"comments` WHERE comment_approved = 'spam')")
		}
	case "options":
		if sel.ExcludeTransients {
			conds = append(conds, `option_name NOT LIKE '\_transient\_%'`, `option_name NOT LIKE '\_site\_transient\_%'`)
		}
	case "sitemeta":
		if sel.ExcludeTransients {
			conds = append(conds, `meta_key NOT LIKE '\_site\_transient\_%'`)
		}
	}
	return strings.Join(conds, " AND ")
}

func (p *Plan) replacements(src, dst *agent.Manifest, sel config.Selection, cfg *config.Config) error {
	var pairs []sqlstream.Pair
	add := func(from, to string) {
		if from != "" && to != "" && from != to {
			pairs = append(pairs, sqlstream.Pair{From: from, To: to})
		}
	}
	srcMain, dstMain := mainSite(src), mainSite(dst)
	if sel.Network {
		add(srcMain.UploadsURL, dstMain.UploadsURL)
		add(srcMain.UploadsDir, dstMain.UploadsDir)
		for _, m := range p.Sites {
			if m.Dst != nil {
				add(m.Src.Home, m.Dst.Home)
				add(m.Src.SiteURL, m.Dst.SiteURL)
			}
		}
	}
	for _, m := range p.Sites {
		if sel.Network || m.Dst == nil {
			continue
		}
		add(m.Src.UploadsURL, m.Dst.UploadsURL)
		add(m.Src.UploadsDir, m.Dst.UploadsDir)
		add(m.Src.Home, m.Dst.Home)
		add(m.Src.SiteURL, m.Dst.SiteURL)
		if m.Src.Prefix != m.Dst.Prefix {
			add(m.Src.Prefix+"user_roles", m.Dst.Prefix+"user_roles")
		}
	}
	add(src.NetworkURL(), dst.NetworkURL())
	if sd, dd := hostOf(src.NetworkURL()), hostOf(dst.NetworkURL()); sd != "" && dd != "" && sd != dd {
		add("//"+sd, "//"+dd)
	}
	add(src.ContentDir, dst.ContentDir)
	add(src.ABSPATH, dst.ABSPATH)

	for _, raw := range append(append([]string{}, cfg.Replace...), sel.Replace...) {
		from, to, ok := strings.Cut(raw, "=>")
		if !ok {
			return fmt.Errorf("--replace %q: use 'old=>new'", raw)
		}
		add(from, to)
	}
	for _, raw := range sel.Regex {
		from, to, ok := strings.Cut(raw, "=>")
		if !ok {
			return fmt.Errorf("--regex %q: use 'pattern=>replacement'", raw)
		}
		rx, err := regexp.Compile(from)
		if err != nil {
			return fmt.Errorf("--regex %q: %w", from, err)
		}
		p.Regexes = append(p.Regexes, sqlstream.RegexPair{Pattern: rx, To: to})
	}
	pairs = append(pairs, sqlstream.JSONVariants(pairs)...)
	p.Pairs = sqlstream.NewReplacer(pairs, nil).Pairs()
	return nil
}

func mainSite(m *agent.Manifest) agent.Site {
	id := 1
	if m.Network != nil {
		id = m.Network.MainSite
	}
	for _, s := range m.Sites {
		if s.ID == id {
			return s
		}
	}
	if len(m.Sites) > 0 {
		return m.Sites[0]
	}
	return agent.Site{}
}

func hostOf(url string) string {
	rest := url
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

var defaultExcludes = []string{".DS_Store", "._*"}

func (p *Plan) files(src, dst *agent.Manifest, sel config.Selection, cfg *config.Config) {
	exclude := uniq(append(append(append([]string{}, defaultExcludes...), cfg.Files.Exclude...), sel.Exclude...))
	job := func(label, from, to string, extra ...string) {
		p.Files = append(p.Files, FileJob{
			Label: label, Src: from, Dst: to,
			Exclude: append(append([]string{}, exclude...), extra...),
			Delete:  sel.Delete, Since: sel.MediaSince,
		})
	}
	if sel.Media {
		if sel.Network {
			job("uploads (network)", mainSite(src).UploadsDir, mainSite(dst).UploadsDir)
		} else {
			for _, m := range p.Sites {
				if m.Dst == nil {
					continue
				}
				var extra []string
				if src.Multisite && m.Src.ID == mainSite(src).ID || dst.Multisite && m.Dst.ID == mainSite(dst).ID {
					extra = append(extra, "/sites/")
				}
				job("uploads "+m.Slug, m.Src.UploadsDir, m.Dst.UploadsDir, extra...)
			}
		}
	}
	dirJobs := func(kind string, names []string, srcRoot, dstRoot string) {
		for _, n := range names {
			if n == "*" || n == "" {
				job(kind, srcRoot, dstRoot)
				return
			}
		}
		for _, n := range names {
			job(kind+" "+n, srcRoot+"/"+n, dstRoot+"/"+n)
		}
	}
	dirJobs("themes", sel.Themes, src.ThemeRoot, dst.ThemeRoot)
	dirJobs("plugins", sel.Plugins, src.PluginDir, dst.PluginDir)
	if sel.MuPlugins {
		job("mu-plugins", src.MuPluginDir, dst.MuPluginDir)
	}
	for _, f := range sel.Files {
		f = strings.Trim(f, "/")
		job(f, src.ContentDir+"/"+f, dst.ContentDir+"/"+f)
	}
	for i := range p.Files {
		if p.Files[i].Since != "" && p.Files[i].Delete {
			p.warn("--delete is ignored with --media-since")
			p.Files[i].Delete = false
		}
	}
}

// matchAny reports whether any glob pattern (e.g. "gf_*") matches the
// unprefixed or full table name.
func matchAny(patterns []string, short, full string) bool {
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if ok, _ := path.Match(p, short); ok {
			return true
		}
		if ok, _ := path.Match(p, full); ok {
			return true
		}
	}
	return false
}

func uniq(list []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range list {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

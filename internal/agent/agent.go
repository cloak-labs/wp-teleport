// Package agent runs the embedded PHP agent inside an environment through
// `wp eval-file -`, so servers need nothing but WP-CLI.
package agent

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cloak-labs/wp-teleport/internal/transport"
)

//go:embed agent.php
var source []byte

var (
	startMarker = []byte("<<<TELEPORT\n")
	endMarker   = []byte("\nTELEPORT>>>")
)

// Call runs one agent action and decodes its JSON result into out.
func Call(ctx context.Context, env *transport.Env, action string, input any, out any) error {
	payload, err := json.Marshal(input)
	if err != nil {
		return err
	}
	script := env.WPCmd() + " eval-file - " + transport.Quote(action) + " " +
		base64.StdEncoding.EncodeToString(payload) + " --skip-plugins --skip-themes"
	stdout, runErr := env.Run(ctx, script, source)
	body, ok := extract(stdout)
	if !ok {
		if runErr != nil {
			return runErr
		}
		return fmt.Errorf("%s: agent returned no result:\n%s", env.Name, strings.TrimSpace(string(stdout)))
	}
	var probe struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &probe) == nil && probe.Error != "" {
		return fmt.Errorf("%s: %s", env.Name, probe.Error)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: decoding agent %s result: %w", env.Name, action, err)
	}
	return nil
}

func extract(stdout []byte) ([]byte, bool) {
	i := bytes.LastIndex(stdout, startMarker)
	if i < 0 {
		return nil, false
	}
	rest := stdout[i+len(startMarker):]
	j := bytes.Index(rest, endMarker)
	if j < 0 {
		return nil, false
	}
	return rest[:j], true
}

// ErrNotMultisite is returned when a site-level action needs a network.
var ErrNotMultisite = errors.New("not a multisite network")

type Site struct {
	ID         int    `json:"id"`
	Domain     string `json:"domain"`
	Path       string `json:"path"`
	Home       string `json:"home"`
	SiteURL    string `json:"siteurl"`
	UploadsDir string `json:"uploads_dir"`
	UploadsURL string `json:"uploads_url"`
	Prefix     string `json:"prefix"`
	Title      string `json:"title"`
	// Key is the slug computed with network context (see assignSlugs).
	Key string `json:"slug,omitempty"`
}

// Slug identifies a site across environments: the last path segment, the
// subdomain for subdomain installs, the full domain for a mapped domain, or
// "main" for the network's root site.
func (s Site) Slug() string {
	if s.Key != "" {
		return s.Key
	}
	p := strings.Trim(s.Path, "/")
	if p == "" {
		return "main"
	}
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[i+1:]
	}
	return p
}

type Table struct {
	Name      string `json:"name"`
	Rows      int64  `json:"rows"`
	Bytes     int64  `json:"bytes"`
	Engine    string `json:"engine"`
	Collation string `json:"collation"`
	View      bool   `json:"view"`
}

type Network struct {
	Domain           string `json:"domain"`
	Path             string `json:"path"`
	SubdomainInstall bool   `json:"subdomain_install"`
	MainSite         int    `json:"main_site"`
}

type DB struct {
	Name      string `json:"name"`
	Host      string `json:"host"`
	Server    string `json:"server"`
	Version   string `json:"version"`
	Engine    string `json:"engine"`
	Charset   string `json:"charset"`
	Collation string `json:"collation"`
}

type Swapped struct {
	Live   string  `json:"live"`
	Backup *string `json:"backup"`
}

type Run struct {
	ID            string    `json:"id"`
	Created       int64     `json:"created"`
	Description   string    `json:"description"`
	Swapped       []Swapped `json:"swapped"`
	CreatedTables []string  `json:"created_tables"`
	Backup        bool      `json:"backup"`
	RolledBack    bool      `json:"rolled_back"`
	// AlsoRolledBack lists newer migrations undone first by a rollback.
	AlsoRolledBack []string `json:"also_rolled_back,omitempty"`
}

type Lock struct {
	By      string `json:"by"`
	Created int64  `json:"created"`
}

type Manifest struct {
	WPVersion    string   `json:"wp_version"`
	WPCLIVersion string   `json:"wp_cli_version"`
	PHPVersion   string   `json:"php_version"`
	Multisite    bool     `json:"multisite"`
	Network      *Network `json:"network"`
	BasePrefix   string   `json:"base_prefix"`
	GlobalTables []string `json:"global_tables"`
	ABSPATH      string   `json:"abspath"`
	ContentDir   string   `json:"content_dir"`
	PluginDir    string   `json:"plugin_dir"`
	MuPluginDir  string   `json:"mu_plugin_dir"`
	ThemeRoot    string   `json:"theme_root"`
	Sites        []Site   `json:"sites"`
	Tables       []Table  `json:"tables"`
	DB           DB       `json:"db"`
	Lock         *Lock    `json:"lock"`
	Runs         []Run    `json:"runs"`
}

func GetManifest(ctx context.Context, env *transport.Env) (*Manifest, error) {
	m := &Manifest{}
	if err := Call(ctx, env, "manifest", map[string]any{}, m); err != nil {
		return nil, err
	}
	m.AssignSlugs()
	return m, nil
}

// AssignSlugs computes each site's Key. Without it, every site whose path is
// "/" (subdomain installs, mapped domains) would be "main".
func (m *Manifest) AssignSlugs() {
	for i := range m.Sites {
		s := &m.Sites[i]
		s.Key = ""
		slug := s.Slug()
		if slug == "main" && m.Network != nil && s.ID != m.Network.MainSite {
			slug = s.Domain
			if sub, ok := strings.CutSuffix(s.Domain, "."+strings.TrimPrefix(m.Network.Domain, "www.")); ok && !strings.Contains(sub, ".") {
				slug = sub
			}
		}
		s.Key = slug
	}
}

// Site finds a site by slug ("main" for the root), numeric ID, or path.
func (m *Manifest) Site(key string) (Site, bool) {
	for _, s := range m.Sites {
		if s.Slug() == key || fmt.Sprint(s.ID) == key || s.Path == key || s.Domain == key {
			return s, true
		}
	}
	return Site{}, false
}

func (m *Manifest) Table(name string) (Table, bool) {
	for _, t := range m.Tables {
		if t.Name == name {
			return t, true
		}
	}
	return Table{}, false
}

// NetworkURL is scheme://domain/path of the network (or the single site's home).
func (m *Manifest) NetworkURL() string {
	if m.Network == nil || len(m.Sites) == 0 {
		if len(m.Sites) > 0 {
			return m.Sites[0].Home
		}
		return ""
	}
	scheme := "https"
	for _, s := range m.Sites {
		if s.ID == m.Network.MainSite {
			if strings.HasPrefix(s.Home, "http://") {
				scheme = "http"
			}
		}
	}
	return strings.TrimRight(scheme+"://"+m.Network.Domain+m.Network.Path, "/")
}

type BeginResult struct {
	Cnf       string            `json:"cnf"`
	Checksums map[string]string `json:"checksums"`
}

type FinalizeResult struct {
	Run       *Run              `json:"run"`
	Pruned    int               `json:"pruned"`
	Checksums map[string]string `json:"checksums"`
}

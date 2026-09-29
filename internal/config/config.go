// Package config loads teleport.yml.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// FileNames are searched, in order, from the working directory upward.
var FileNames = []string{"teleport.yml", "teleport.yaml", ".teleport.yml"}

type Config struct {
	// Dir is the directory holding the config file. State lives in Dir/.teleport.
	Dir  string `yaml:"-"`
	File string `yaml:"-"`

	Environments map[string]*Env      `yaml:"environments"`
	Local        string               `yaml:"local"`
	Hooks        Hooks                `yaml:"hooks"`
	Profiles     map[string]Selection `yaml:"profiles"`
	Preserve     []string             `yaml:"preserve_options"`
	Replace      []string             `yaml:"replace"`
	Files        FilesConfig          `yaml:"files"`
	Backups      BackupsConfig        `yaml:"backups"`
	Parallel     int                  `yaml:"parallel"`
}

type Env struct {
	Name string `yaml:"-"`
	// Exactly one of SSH / Docker may be set; neither means this machine.
	SSH    string `yaml:"ssh"`
	Docker string `yaml:"docker"`
	// Path is the WordPress root (where wp-cli.yml or wp-config.php lives), as
	// seen by that environment.
	Path string `yaml:"path"`
	// HostPath maps Path to a directory on this machine for Docker envs whose
	// files are bind-mounted. Auto-detected from `docker inspect` when empty.
	HostPath  string   `yaml:"host_path"`
	WP        string   `yaml:"wp"`
	WPFlags   []string `yaml:"wp_flags"`
	Protected bool     `yaml:"protected"`
	Hooks     Hooks    `yaml:"hooks"`
}

type Hooks struct {
	Before []string `yaml:"before"`
	After  []string `yaml:"after"`
}

type FilesConfig struct {
	Exclude []string `yaml:"exclude"`
}

type BackupsConfig struct {
	// Keep is how many previous migrations keep their pre-swap tables for
	// rollback. 0 means the default (3); -1 disables backups.
	Keep int `yaml:"keep"`
}

// Selection describes what to migrate. It is both a saved profile and the
// parsed form of the pull/push/sync flags.
type Selection struct {
	From string `yaml:"from" json:"from,omitempty"`
	To   string `yaml:"to" json:"to,omitempty"`

	Sites      []string `yaml:"sites" json:"sites,omitempty"`
	As         string   `yaml:"as" json:"as,omitempty"`
	Network    bool     `yaml:"network" json:"network,omitempty"`
	CreateSite bool     `yaml:"create_site" json:"create_site,omitempty"`

	DB                bool     `yaml:"db" json:"db,omitempty"`
	Tables            []string `yaml:"tables" json:"tables,omitempty"`
	ExcludeTables     []string `yaml:"exclude_tables" json:"exclude_tables,omitempty"`
	ExcludePostTypes  []string `yaml:"exclude_post_types" json:"exclude_post_types,omitempty"`
	ExcludeRevisions  bool     `yaml:"exclude_revisions" json:"exclude_revisions,omitempty"`
	ExcludeSpam       bool     `yaml:"exclude_spam" json:"exclude_spam,omitempty"`
	ExcludeTransients bool     `yaml:"exclude_transients" json:"exclude_transients,omitempty"`
	SkipGUIDs         bool     `yaml:"skip_guids" json:"skip_guids,omitempty"`
	Users             bool     `yaml:"users" json:"users,omitempty"`
	Preserve          []string `yaml:"preserve_options" json:"preserve_options,omitempty"`
	PreservePlugins   bool     `yaml:"preserve_active_plugins" json:"preserve_active_plugins,omitempty"`
	Replace           []string `yaml:"replace" json:"replace,omitempty"`
	Regex             []string `yaml:"regex" json:"regex,omitempty"`

	Media      bool     `yaml:"media" json:"media,omitempty"`
	MediaSince string   `yaml:"media_since" json:"media_since,omitempty"`
	Delete     bool     `yaml:"delete" json:"delete,omitempty"`
	Themes     []string `yaml:"themes" json:"themes,omitempty"`
	Plugins    []string `yaml:"plugins" json:"plugins,omitempty"`
	MuPlugins  bool     `yaml:"mu_plugins" json:"mu_plugins,omitempty"`
	Files      []string `yaml:"files" json:"files,omitempty"`
	Exclude    []string `yaml:"exclude" json:"exclude,omitempty"`
}

// Normalize expands convenience flags into the lists the planner reads.
func (s *Selection) Normalize() {
	if s.ExcludeRevisions {
		s.ExcludePostTypes = appendUniq(s.ExcludePostTypes, "revision")
	}
	if s.PreservePlugins {
		s.Preserve = appendUniq(s.Preserve, "active_plugins", "stylesheet", "template")
	}
}

func appendUniq(list []string, extra ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range append(append([]string{}, list...), extra...) {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// HasFiles reports whether any file transfer is selected.
func (s Selection) HasFiles() bool {
	return s.Media || len(s.Themes) > 0 || len(s.Plugins) > 0 || s.MuPlugins || len(s.Files) > 0
}

// HasDB reports whether any database transfer is selected.
func (s Selection) HasDB() bool {
	return s.DB || len(s.Tables) > 0
}

// DefaultPreserve are destination options kept as-is by every migration.
var DefaultPreserve = []string{"blog_public"}

// Find locates a config file from explicit, $TELEPORT_CONFIG, or by walking up
// from the working directory.
func Find(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if env := os.Getenv("TELEPORT_CONFIG"); env != "" {
		return env, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		for _, name := range FileNames {
			p := filepath.Join(dir, name)
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no teleport.yml found (searched upward from the current directory). Run `teleport init` or pass --config")
		}
		dir = parent
	}
}

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	abs, _ := filepath.Abs(path)
	cfg.File = abs
	cfg.Dir = filepath.Dir(abs)
	if cfg.Environments == nil {
		cfg.Environments = map[string]*Env{}
	}
	if err := importWPCLIAliases(cfg); err != nil {
		return nil, err
	}
	for name, e := range cfg.Environments {
		if e == nil {
			return nil, fmt.Errorf("environment %q is empty", name)
		}
		e.Name = name
		if e.SSH != "" && e.Docker != "" {
			return nil, fmt.Errorf("environment %q sets both ssh and docker", name)
		}
		if e.Path == "" {
			if e.SSH != "" {
				return nil, fmt.Errorf("environment %q needs a path (the WordPress root on %s)", name, e.SSH)
			}
			if e.Docker != "" {
				e.Path = "/var/www/html"
			} else {
				e.Path = cfg.Dir
			}
		}
		if e.SSH == "" && e.Docker == "" && !filepath.IsAbs(e.Path) {
			e.Path = filepath.Join(cfg.Dir, e.Path)
		}
		if e.HostPath != "" && !filepath.IsAbs(e.HostPath) {
			e.HostPath = filepath.Join(cfg.Dir, e.HostPath)
		}
		if e.WP == "" {
			e.WP = "wp"
		}
		if e.Docker != "" && e.WPFlags == nil {
			e.WPFlags = []string{"--allow-root"}
		}
	}
	if cfg.Local == "" {
		cfg.Local = "local"
	}
	if cfg.Parallel <= 0 {
		cfg.Parallel = 4
	}
	if cfg.Backups.Keep == 0 {
		cfg.Backups.Keep = 3
	}
	for name, p := range cfg.Profiles {
		if p.From == "" || p.To == "" {
			return nil, fmt.Errorf("profile %q needs both from and to", name)
		}
	}
	return cfg, nil
}

// Env returns a named environment.
func (c *Config) Env(name string) (*Env, error) {
	name = strings.TrimPrefix(name, "@")
	if e, ok := c.Environments[name]; ok {
		return e, nil
	}
	return nil, fmt.Errorf("unknown environment %q (have: %s)", name, strings.Join(c.EnvNames(), ", "))
}

func (c *Config) EnvNames() []string {
	var names []string
	for n := range c.Environments {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// importWPCLIAliases adds @aliases from a sibling wp-cli.yml for environments
// not already defined. Supported forms: `ssh: [user@]host[:port]/path` and
// `ssh: docker:[user@]container[/path]`.
func importWPCLIAliases(cfg *Config) error {
	raw, err := os.ReadFile(filepath.Join(cfg.Dir, "wp-cli.yml"))
	if err != nil {
		return nil
	}
	var doc map[string]any
	if yaml.Unmarshal(raw, &doc) != nil {
		return nil
	}
	for key, val := range doc {
		if !strings.HasPrefix(key, "@") || key == "@all" {
			continue
		}
		name := strings.TrimPrefix(key, "@")
		if _, exists := cfg.Environments[name]; exists {
			continue
		}
		m, ok := val.(map[string]any)
		if !ok {
			continue
		}
		ssh, _ := m["ssh"].(string)
		path, _ := m["path"].(string)
		if ssh == "" {
			continue
		}
		e := &Env{}
		if rest, ok := strings.CutPrefix(ssh, "docker:"); ok {
			if at := strings.Index(rest, "@"); at >= 0 {
				rest = rest[at+1:]
			}
			if slash := strings.Index(rest, "/"); slash >= 0 {
				e.Docker, e.Path = rest[:slash], rest[slash:]
			} else {
				e.Docker = rest
			}
		} else {
			host := ssh
			if slash := strings.Index(host, "/"); slash >= 0 {
				host, e.Path = host[:slash], host[slash:]
			} else if tilde := strings.Index(host, "~"); tilde >= 0 {
				host, e.Path = host[:tilde], host[tilde:]
			}
			e.SSH = host
		}
		if path != "" {
			e.Path = path
		}
		cfg.Environments[name] = e
	}
	return nil
}

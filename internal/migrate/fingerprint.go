package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/cloak-labs/wp-teleport/internal/agent"
	"github.com/cloak-labs/wp-teleport/internal/config"
	"github.com/cloak-labs/wp-teleport/internal/plan"
)

// fingerprint binds approval to the configured operation and both databases.
// It does not snapshot files or prevent edits after the check completes.
func fingerprint(ctx context.Context, cfg *config.Config, src, dst *Side, p *plan.Plan, sel config.Selection) (string, error) {
	var source, destination []string
	tables := make([]map[string]string, 0, len(p.Tables))
	for _, t := range p.Tables {
		source = append(source, t.Src)
		destination = append(destination, t.Live)
		tables = append(tables, map[string]string{"source": t.Src, "live": t.Live, "where": t.Where})
	}
	sort.Slice(tables, func(i, j int) bool { return tables[i]["source"] < tables[j]["source"] })
	sums := func(side *Side, names []string) (map[string]string, error) {
		var out struct {
			Checksums map[string]string `json:"checksums"`
		}
		if len(names) == 0 {
			return map[string]string{}, nil
		}
		err := agent.Call(ctx, side.Env, "checksum", map[string]any{"tables": names}, &out)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			// A missing destination table is a legitimate planned creation.
			for _, t := range side.Manifest.Tables {
				if t.Name == name && out.Checksums[name] == "" {
					return nil, fmt.Errorf("cannot fingerprint table %s: checksum unavailable", name)
				}
			}
		}
		return out.Checksums, nil
	}
	a, err := sums(src, source)
	if err != nil {
		return "", err
	}
	b, err := sums(dst, destination)
	if err != nil {
		return "", err
	}
	return hashPlan(map[string]any{"version": 1, "config": cfg, "selection": sel, "tables": tables, "sites": p.Sites, "rules": p.Pairs, "files": p.Files, "source": a, "destination": b, "source_database": src.Manifest.DB, "destination_database": dst.Manifest.DB})
}

func hashPlan(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

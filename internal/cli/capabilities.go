package cli

import (
	"github.com/cloak-labs/wp-teleport/internal/transport"
	"github.com/spf13/cobra"
)

// Public configuration for integrations, without paths, commands or SSH details.
func (a *app) capabilitiesCmd() *cobra.Command {
	return &cobra.Command{Use: "capabilities", Short: "Describe the integration API and configured environments without connecting", RunE: func(cmd *cobra.Command, args []string) error {
		if err := a.load(); err != nil {
			return err
		}
		envs := []map[string]any{}
		for _, name := range a.cfg.EnvNames() {
			env := a.cfg.Environments[name]
			envs = append(envs, map[string]any{"name": name, "protected": env.Protected, "local": name == a.cfg.Local, "transport": transport.New(env).Kind})
		}
		return a.emit(map[string]any{"api_version": 1, "environments": envs, "backups_keep": a.cfg.Backups.Keep, "features": []string{"plan-fingerprint", "scoped-rollback-preview", "single-json-migration-result"}})
	}}
}

package cloudhypervisor

import (
	"context"
	"errors"
	"path/filepath"

	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse-provider-api/plugin"
)

// Factory supports the published SDK's process and outbound hosts. Configuration
// is strict JSON (valid YAML), and executor defaults are explicitly unsupported.
func Factory(_ context.Context, in plugin.Init) (api.Provider, plugin.Cleanup, error) {
	if len(in.ExecutorEnv) > 0 {
		return nil, nil, errors.New("cloudhypervisor: executor defaults unsupported")
	}
	c, e := DecodeConfig(in.ConfigYAML)
	if e != nil {
		return nil, nil, e
	}
	if !filepath.IsAbs(in.BaseDir) {
		return nil, nil, ErrConfig
	}
	c = resolvePaths(c, in.BaseDir)

	p, e := New(c)
	if e != nil {
		return nil, nil, e
	}
	// Maintenance is explicit: hosts may call Run themselves. Factory construction
	// and capability/list observations never prepare or reconcile resources.
	return p, func(context.Context) error { return p.Close() }, nil
}

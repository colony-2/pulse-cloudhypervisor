package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	ch "github.com/colony-2/pulse-cloudhypervisor"
	"github.com/colony-2/pulse-cloudhypervisor/internal/runner"
	"github.com/colony-2/pulse-cloudhypervisor/internal/state"
	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse-provider-api/plugin"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if run(ctx, os.Args[1:]) != nil {
		os.Stderr.WriteString("pulse-cloudhypervisor: command failed; inspect protected configuration and prerequisites\n")
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return ch.ErrConfig
	}
	mode := args[0]
	var paths ch.PathResolver
	f := flag.NewFlagSet(mode, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	if mode == "runner" || mode == "prepare-runner" {
		dir := f.String("dir", "", "absolute owner launch directory")
		digest := f.String("digest", "", "record digest")
		if f.Parse(args[1:]) != nil || f.NArg() != 0 {
			return ch.ErrConfig
		}
		if mode == "runner" {
			return runner.Execute(*dir, *digest)
		}
		return runner.Prepare(*dir, *digest)
	}
	if mode != "setup" && mode != "upgrade" && mode != "server" {
		return ch.ErrConfig
	}
	applyCapacity := capacityFlags(f)
	configPath := f.String("config", "", "versioned settings JSON path")
	owner := f.String("owner", "", "owner override")
	dir := f.String("state-dir", "", "absolute owner directory override")
	outbound := f.String("outbound", "", "collector host:port")
	name := f.String("name", "", "authorized provider identity")
	ca := f.String("ca", "", "collector CA file")
	cert := f.String("cert", "", "provider certificate file")
	key := f.String("key", "", "protected client key file")
	serverName := f.String("server-name", "", "collector DNS name")
	if f.Parse(args[1:]) != nil || f.NArg() != 0 {
		return ch.ErrConfig
	}
	if mode != "server" && (*outbound != "" || *name != "" || *ca != "" || *cert != "" || *key != "" || *serverName != "") {
		return ch.ErrConfig
	}
	if mode == "server" && *outbound == "" && (*name != "" || *ca != "" || *cert != "" || *key != "" || *serverName != "") {
		return ch.ErrConfig
	}
	if mode == "server" && *outbound == "" {
		// Configuration may be provided by SDK Init. A command settings path is
		// used only when Init omits configuration. Server never saves settings.
		factory := func(c context.Context, in plugin.Init) (api.Provider, plugin.Cleanup, error) {
			if len(in.ConfigYAML) == 0 {
				path := *configPath
				if path == "" {
					var e error
					path, e = paths.ConfigPath("")
					if e != nil {
						return nil, nil, e
					}
				}
				cfg, e := ch.LoadConfig(path)
				if e != nil {
					return nil, nil, e
				}
				in.ConfigYAML, _ = json.Marshal(cfg)
			}
			cfg, e := ch.DecodeConfig(in.ConfigYAML)
			if e != nil {
				return nil, nil, e
			}
			applyCapacity(&cfg)
			if *owner != "" {
				cfg.Owner = *owner
			}
			if *dir != "" {
				cfg.StateDir = *dir
			}
			in.ConfigYAML, _ = json.Marshal(cfg)
			return ch.Factory(c, in)
		}
		return plugin.Serve(factory, plugin.ServeOptions{Implementation: "pulse-cloudhypervisor", Version: "0.1.0-dev"})
	}
	path := *configPath
	if path == "" {
		var e error
		path, e = paths.ConfigPath("")
		if e != nil {
			return e
		}
	}
	if !filepath.IsAbs(path) {
		return ch.ErrConfig
	}
	cfg, e := loadPaths(path, *owner, *dir, &paths, applyCapacity)
	if e != nil {
		return e
	}
	if mode == "server" {
		if *name == "" || *serverName == "" {
			return ch.ErrConfig
		}
		f, e := state.OpenPrivate(*key, os.O_RDONLY)
		if e != nil {
			return ch.ErrConfig
		}
		f.Close()
		caBytes, e := os.ReadFile(*ca)
		if e != nil {
			return ch.ErrConfig
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(caBytes) {
			return ch.ErrConfig
		}
		pair, e := tls.LoadX509KeyPair(*cert, *key)
		if e != nil {
			return ch.ErrConfig
		}
		data, _ := json.Marshal(cfg)
		return plugin.Run(ctx, ch.Factory, plugin.RunOptions{Address: *outbound, Init: plugin.Initialization{Name: *name, BaseDir: filepath.Dir(path), ConfigYAML: data}, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: *serverName, Certificates: []tls.Certificate{pair}, NextProtos: []string{plugin.ReverseALPN}}, Serve: plugin.ServeOptions{Implementation: "pulse-cloudhypervisor", Version: "0.1.0-dev"}})
	}
	p, e := ch.New(cfg)
	if e != nil {
		return e
	}
	defer p.Close()
	if mode == "setup" {
		e = p.Setup(ctx)
	} else {
		e = p.Upgrade(ctx)
	}
	if e != nil {
		return e
	}
	cfg, e = ch.Normalize(cfg)
	if e != nil {
		return e
	}
	return ch.SaveConfig(path, cfg)
}
func load(path, owner, dir string) (ch.Config, error) {
	var paths ch.PathResolver
	return loadPaths(path, owner, dir, &paths)
}
func loadPaths(path, owner, dir string, paths *ch.PathResolver, overrides ...func(*ch.Config)) (ch.Config, error) {
	c, e := ch.LoadConfig(path)
	if e != nil {
		return c, e
	}
	if owner != "" {
		c.Owner = owner
	}
	if dir != "" {
		c.StateDir = dir
	}
	for _, apply := range overrides {
		apply(&c)
	}
	return paths.Normalize(c)
}

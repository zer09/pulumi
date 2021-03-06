// Copyright 2016-2018, Pulumi Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package plugin

import (
	"os"

	"github.com/blang/semver"
	"github.com/hashicorp/go-multierror"
	"github.com/pkg/errors"

	"github.com/pulumi/pulumi/pkg/diag"
	"github.com/pulumi/pulumi/pkg/resource"
	"github.com/pulumi/pulumi/pkg/resource/config"
	"github.com/pulumi/pulumi/pkg/tokens"
	"github.com/pulumi/pulumi/pkg/util/cmdutil"
	"github.com/pulumi/pulumi/pkg/util/contract"
	"github.com/pulumi/pulumi/pkg/util/logging"
	"github.com/pulumi/pulumi/pkg/workspace"
)

// A Host hosts provider plugins and makes them easily accessible by package name.
type Host interface {
	// ServerAddr returns the address at which the host's RPC interface may be found.
	ServerAddr() string

	// Log logs a message, including errors and warnings.  Messages can have a resource URN
	// associated with them.  If no urn is provided, the message is global.
	Log(sev diag.Severity, urn resource.URN, msg string)

	// Analyzer fetches the analyzer with a given name, possibly lazily allocating the plugins for it.  If an analyzer
	// could not be found, or an error occurred while creating it, a non-nil error is returned.
	Analyzer(nm tokens.QName) (Analyzer, error)
	// Provider fetches the provider for a given package, lazily allocating it if necessary.  If a provider for this
	// package could not be found, or an error occurs while creating it, a non-nil error is returned.
	Provider(pkg tokens.Package, version *semver.Version) (Provider, error)
	// LanguageRuntime fetches the language runtime plugin for a given language, lazily allocating if necessary.  If
	// an implementation of this language runtime wasn't found, on an error occurs, a non-nil error is returned.
	LanguageRuntime(runtime string) (LanguageRuntime, error)

	// ListPlugins lists all plugins that have been loaded, with version information.
	ListPlugins() []workspace.PluginInfo
	// EnsurePlugins ensures all plugins in the given array are loaded and ready to use.  If any plugins are missing,
	// and/or there are errors loading one or more plugins, a non-nil error is returned.
	EnsurePlugins(plugins []workspace.PluginInfo, kinds Flags) error
	// GetRequiredPlugins lists a full set of plugins that will be required by the given program.
	GetRequiredPlugins(info ProgInfo, kinds Flags) ([]workspace.PluginInfo, error)

	// Close reclaims any resources associated with the host.
	Close() error
}

// Events provides higher-level consumers of the plugin model to attach callbacks on
// plugin load events.
type Events interface {
	// OnPluginLoad is fired by the plugin host whenever a new plugin is successfully loaded.
	// newPlugin is the plugin that was loaded.
	OnPluginLoad(newPlugin workspace.PluginInfo) error
}

// NewDefaultHost implements the standard plugin logic, using the standard installation root to find them.
func NewDefaultHost(ctx *Context, config ConfigSource, events Events) (Host, error) {
	host := &defaultHost{
		ctx:             ctx,
		config:          config,
		events:          events,
		analyzerPlugins: make(map[tokens.QName]*analyzerPlugin),
		languagePlugins: make(map[string]*languagePlugin),
		resourcePlugins: make(map[tokens.Package]*resourcePlugin),
		loadRequests:    make(chan pluginLoadRequest),
	}

	// Fire up a gRPC server to listen for requests.  This acts as a RPC interface that plugins can use
	// to "phone home" in case there are things the host must do on behalf of the plugins (like log, etc).
	svr, err := newHostServer(host, ctx)
	if err != nil {
		return nil, err
	}
	host.server = svr

	// Start a goroutine we'll use to satisfy load requests serially and avoid race conditions.
	go func() {
		for req := range host.loadRequests {
			req.result <- req.load()
		}
	}()

	return host, nil
}

type pluginLoadRequest struct {
	load   func() error
	result chan<- error
}

type defaultHost struct {
	ctx             *Context                           // the shared context for this host.
	config          ConfigSource                       // the source for provider configuration parameters.
	events          Events                             // optional callbacks for plugin load events
	analyzerPlugins map[tokens.QName]*analyzerPlugin   // a cache of analyzer plugins and their processes.
	languagePlugins map[string]*languagePlugin         // a cache of language plugins and their processes.
	resourcePlugins map[tokens.Package]*resourcePlugin // a cache of resource plugins and their processes.
	plugins         []workspace.PluginInfo             // a list of plugins allocated by this host.
	loadRequests    chan pluginLoadRequest             // a channel used to satisfy plugin load requests.
	server          *hostServer                        // the server's RPC machinery.
}

type analyzerPlugin struct {
	Plugin Analyzer
	Info   workspace.PluginInfo
}

type languagePlugin struct {
	Plugin LanguageRuntime
	Info   workspace.PluginInfo
}

type resourcePlugin struct {
	Plugin Provider
	Info   workspace.PluginInfo
}

func (host *defaultHost) ServerAddr() string {
	return host.server.Address()
}

func (host *defaultHost) Log(sev diag.Severity, urn resource.URN, msg string) {
	host.ctx.Diag.Logf(sev, diag.RawMessage(urn, msg))
}

// loadPlugin sends an appropriate load request to the plugin loader and returns the loaded plugin (if any) and error.
func (host *defaultHost) loadPlugin(load func() (interface{}, error)) (interface{}, error) {
	var plugin interface{}

	result := make(chan error)
	host.loadRequests <- pluginLoadRequest{
		load: func() error {
			p, err := load()
			plugin = p
			return err
		},
		result: result,
	}
	return plugin, <-result
}

func (host *defaultHost) Analyzer(name tokens.QName) (Analyzer, error) {
	plugin, err := host.loadPlugin(func() (interface{}, error) {
		// First see if we already loaded this plugin.
		if plug, has := host.analyzerPlugins[name]; has {
			contract.Assert(plug != nil)
			return plug.Plugin, nil
		}

		// If not, try to load and bind to a plugin.
		plug, err := NewAnalyzer(host, host.ctx, name)
		if err == nil && plug != nil {
			info, infoerr := plug.GetPluginInfo()
			if infoerr != nil {
				return nil, infoerr
			}

			// Memoize the result.
			host.plugins = append(host.plugins, info)
			host.analyzerPlugins[name] = &analyzerPlugin{Plugin: plug, Info: info}
			if host.events != nil {
				if eventerr := host.events.OnPluginLoad(info); eventerr != nil {
					return nil, errors.Wrapf(eventerr, "failed to perform plugin load callback")
				}
			}
		}

		return plug, err
	})
	if plugin == nil || err != nil {
		return nil, err
	}
	return plugin.(Analyzer), nil
}

func (host *defaultHost) Provider(pkg tokens.Package, version *semver.Version) (Provider, error) {
	plugin, err := host.loadPlugin(func() (interface{}, error) {
		// First see if we already loaded this plugin.
		if plug, has := host.resourcePlugins[pkg]; has {
			contract.Assert(plug != nil)

			// Make sure the versions match.
			if version != nil {
				if plug.Info.Version == nil {
					return nil,
						errors.Errorf("resource plugin version %s requested, but an unknown version was found",
							version.String())
				} else if !plug.Info.Version.GTE(*version) {
					return nil,
						errors.Errorf("resource plugin version %s requested, but version %s was found",
							version.String(), plug.Info.Version.String())
				}
			}

			return plug.Plugin, nil
		}

		// If not, try to load and bind to a plugin.
		plug, err := NewProvider(host, host.ctx, pkg, version)
		if err == nil && plug != nil {
			info, infoerr := plug.GetPluginInfo()
			if infoerr != nil {
				return nil, infoerr
			}

			// Warn if the plugin version was not what we expected
			if version != nil && !cmdutil.IsTruthy(os.Getenv("PULUMI_DEV")) {
				if info.Version == nil || !info.Version.GTE(*version) {
					var v string
					if info.Version != nil {
						v = info.Version.String()
					}
					host.ctx.Diag.Warningf(
						diag.Message("", /*urn*/
							"resource plugin %s is expected to have version >=%s, but has %s; "+
								"the wrong version may be on your path, or this may be a bug in the plugin"),
						info.Name, version.String(), v)
				}
			}

			// Configure the provider. If no configuration source is present, assume no configuration. We do this here
			// because resource providers must be configured exactly once before any method besides Configure is called.
			providerConfig := make(map[config.Key]string)
			if host.config != nil {
				providerConfig, err = host.config.GetPackageConfig(pkg)
				if err != nil {
					return nil, errors.Wrapf(err, "failed to fetch configuration for pkg '%v' resource provider", pkg)
				}
			}
			if err = plug.Configure(providerConfig); err != nil {
				return nil, errors.Wrapf(err, "failed to configure pkg '%v' resource provider", pkg)
			}

			// Memoize the result.
			host.plugins = append(host.plugins, info)
			host.resourcePlugins[pkg] = &resourcePlugin{Plugin: plug, Info: info}
			if host.events != nil {
				if eventerr := host.events.OnPluginLoad(info); eventerr != nil {
					return nil, errors.Wrapf(eventerr, "failed to perform plugin load callback")
				}
			}
		}

		return plug, err
	})
	if plugin == nil || err != nil {
		return nil, err
	}
	return plugin.(Provider), nil
}

func (host *defaultHost) LanguageRuntime(runtime string) (LanguageRuntime, error) {
	plugin, err := host.loadPlugin(func() (interface{}, error) {
		// First see if we already loaded this plugin.
		if plug, has := host.languagePlugins[runtime]; has {
			contract.Assert(plug != nil)
			return plug.Plugin, nil
		}

		// If not, allocate a new one.
		plug, err := NewLanguageRuntime(host, host.ctx, runtime)
		if err == nil && plug != nil {
			info, infoerr := plug.GetPluginInfo()
			if infoerr != nil {
				return nil, infoerr
			}

			// Memoize the result.
			host.plugins = append(host.plugins, info)
			host.languagePlugins[runtime] = &languagePlugin{Plugin: plug, Info: info}
			if host.events != nil {
				if eventerr := host.events.OnPluginLoad(info); eventerr != nil {
					return nil, errors.Wrapf(eventerr, "failed to perform plugin load callback")
				}
			}
		}

		return plug, err
	})
	if plugin == nil || err != nil {
		return nil, err
	}
	return plugin.(LanguageRuntime), nil
}

func (host *defaultHost) ListPlugins() []workspace.PluginInfo {
	return host.plugins
}

// EnsurePlugins ensures all plugins in the given array are loaded and ready to use.  If any plugins are missing,
// and/or there are errors loading one or more plugins, a non-nil error is returned.
func (host *defaultHost) EnsurePlugins(plugins []workspace.PluginInfo, kinds Flags) error {
	// Use a multieerror to track failures so we can return one big list of all failures at the end.
	var result error
	for _, plugin := range plugins {
		switch plugin.Kind {
		case workspace.AnalyzerPlugin:
			if kinds&AnalyzerPlugins != 0 {
				if _, err := host.Analyzer(tokens.QName(plugin.Name)); err != nil {
					result = multierror.Append(result,
						errors.Wrapf(err, "failed to load analyzer plugin %s", plugin.Name))
				}
			}
		case workspace.LanguagePlugin:
			if kinds&LanguagePlugins != 0 {
				if _, err := host.LanguageRuntime(plugin.Name); err != nil {
					result = multierror.Append(result,
						errors.Wrapf(err, "failed to load language plugin %s", plugin.Name))
				}
			}
		case workspace.ResourcePlugin:
			if kinds&ResourcePlugins != 0 {
				if _, err := host.Provider(tokens.Package(plugin.Name), plugin.Version); err != nil {
					result = multierror.Append(result,
						errors.Wrapf(err, "failed to load resource plugin %s", plugin.Name))
				}
			}
		default:
			contract.Failf("unexpected plugin kind: %s", plugin.Kind)
		}
	}

	return result
}

// GetRequiredPlugins lists a full set of plugins that will be required by the given program.
func (host *defaultHost) GetRequiredPlugins(info ProgInfo, kinds Flags) ([]workspace.PluginInfo, error) {
	var plugins []workspace.PluginInfo

	if kinds&LanguagePlugins != 0 {
		// First make sure the language plugin is present.  We need this to load the required resource plugins.
		// TODO: we need to think about how best to version this.  For now, it always picks the latest.
		lang, err := host.LanguageRuntime(info.Proj.Runtime)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to load language plugin %s", info.Proj.Runtime)
		}
		plugins = append(plugins, workspace.PluginInfo{
			Name: info.Proj.Runtime,
			Kind: workspace.LanguagePlugin,
		})

		if kinds&ResourcePlugins != 0 {
			// Use the language plugin to compute this project's set of plugin dependencies.
			// TODO: we want to support loading precisely what the project needs, rather than doing a static scan of resolved
			//     packages.  Doing this requires that we change our RPC interface and figure out how to configure plugins
			//     later than we do (right now, we do it up front, but at that point we don't know the version).
			deps, err := lang.GetRequiredPlugins(info)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to discover plugin requirements")
			}
			plugins = append(plugins, deps...)
		}
	} else {
		// If we can't load the language plugin, we can't discover the resource plugins.
		contract.Assertf(kinds&ResourcePlugins != 0,
			"cannot load resource plugins without also loading the language plugin")
	}

	// Next, if there are analyzers listed in the project file, use them too.
	// TODO: these are currently not versioned.  We probably need to let folks specify versions in Pulumi.yaml.
	if info.Proj.Analyzers != nil && kinds&AnalyzerPlugins != 0 {
		for _, analyzer := range *info.Proj.Analyzers {
			plugins = append(plugins, workspace.PluginInfo{
				Name: string(analyzer),
				Kind: workspace.AnalyzerPlugin,
			})
		}
	}

	return plugins, nil
}

func (host *defaultHost) Close() error {
	// Close all plugins.
	for _, plug := range host.analyzerPlugins {
		if err := plug.Plugin.Close(); err != nil {
			logging.Infof("Error closing '%s' analyzer plugin during shutdown; ignoring: %v", plug.Info.Name, err)
		}
	}
	for _, plug := range host.resourcePlugins {
		if err := plug.Plugin.Close(); err != nil {
			logging.Infof("Error closing '%s' resource plugin during shutdown; ignoring: %v", plug.Info.Name, err)
		}
	}
	for _, plug := range host.languagePlugins {
		if err := plug.Plugin.Close(); err != nil {
			logging.Infof("Error closing '%s' language plugin during shutdown; ignoring: %v", plug.Info.Name, err)
		}
	}

	// Empty out all maps.
	host.analyzerPlugins = make(map[tokens.QName]*analyzerPlugin)
	host.languagePlugins = make(map[string]*languagePlugin)
	host.resourcePlugins = make(map[tokens.Package]*resourcePlugin)

	// Shut down the plugin loader.
	close(host.loadRequests)

	// Finally, shut down the host's gRPC server.
	return host.server.Cancel()
}

// Flags can be used to filter out plugins during loading that aren't necessary.
type Flags int

const (
	// AnalyzerPlugins is used to only load analyzers.
	AnalyzerPlugins Flags = 1 << iota
	// LanguagePlugins is used to only load language plugins.
	LanguagePlugins
	// ResourcePlugins is used to only load resource provider plugins.
	ResourcePlugins
)

// AllPlugins uses flags to ensure that all plugin kinds are loaded.
var AllPlugins = AnalyzerPlugins | LanguagePlugins | ResourcePlugins

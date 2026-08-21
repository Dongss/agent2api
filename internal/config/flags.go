package config

import "github.com/spf13/pflag"

// Flags cover what is worth changing per run — where to listen, how loud to log
// — and nothing else; the config file reaches every key. A flag per key made
// --help unreadable and bought little.
//
// Each flag maps to one config path, named here so the mapping reads in one
// place.
const (
	flagConfig         = "config"
	flagHost           = "host"
	flagPort           = "port"
	flagAPIKey         = "api-key"
	flagLogLevel       = "log-level"
	flagLogFile        = "log-file"
	flagMaxConcurrency = "max-concurrency"
)

// RegisterFlags adds agent2api's command-line flags to fs.
func RegisterFlags(fs *pflag.FlagSet) {
	// A flag's default is the built-in default, read from the struct so the two
	// cannot disagree and a renamed field is a compile error.
	def := Defaults()

	fs.StringP(flagConfig, "c", "", "path to a config file; without it, defaults and flags are the whole configuration")
	fs.String(flagHost, def.Server.Host, "address to bind (non-loopback requires --api-key)")
	fs.IntP(flagPort, "p", def.Server.Port, "port to listen on")
	fs.String(flagAPIKey, def.Server.APIKey, "static bearer key every request must present")
	fs.String(flagLogLevel, def.Log.Level, "log level: debug, info, warn, error")
	fs.String(flagLogFile, def.Log.File, "write logs to this file instead of stderr")
	fs.Int(flagMaxConcurrency, def.Server.MaxConcurrency, "concurrent CLI processes per adapter")
}

// ConfigFileHelp points at the config file for everything the flags omit.
func ConfigFileHelp() string {
	return "Only common settings have flags. Everything else — the CLI binary paths, sandbox\n" +
		"and system-prompt modes, timeouts — lives in a config file passed with -c/--config;\n" +
		"see agent2api.example.yaml for every key. Anything a file leaves out keeps its\n" +
		"built-in default, and a flag overrides the file.\n" +
		"Run `agent2api config print` to see the effective config and where each value came from."
}

// flagOverrides collects the keys the user actually set on the command line.
// Flags left at their default never mask a config file value.
func flagOverrides(fs *pflag.FlagSet) (map[string]any, error) {
	overrides := map[string]any{}
	if fs == nil {
		return overrides, nil
	}

	// cobra parses into a merged flag set, so consult Changed on each flag
	// rather than trusting Visit's bookkeeping on the set we were handed.
	set := func(name string) bool {
		f := fs.Lookup(name)
		return f != nil && f.Changed
	}

	var errs []error
	str := func(name, key string) {
		if !set(name) {
			return
		}
		v, err := fs.GetString(name)
		if err != nil {
			errs = append(errs, err)
			return
		}
		overrides[key] = v
	}
	str(flagHost, "server.host")
	str(flagAPIKey, "server.api_key")
	str(flagLogLevel, "log.level")
	str(flagLogFile, "log.file")

	for _, spec := range []struct{ name, key string }{
		{flagPort, "server.port"},
		{flagMaxConcurrency, "server.max_concurrency"},
	} {
		if !set(spec.name) {
			continue
		}
		v, err := fs.GetInt(spec.name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		overrides[spec.key] = v
	}

	if len(errs) > 0 {
		return nil, errs[0]
	}
	return overrides, nil
}

// Package remoteconfig loads the remote-half YAML configuration.
//
// The remote half reads /etc/myhostmcp/config.yaml by default (override with
// `myhostmcp remote --config <path>`). It is the ONLY place the command
// allowlist is specified: the local half no longer holds an allowlist, and the
// "configure" protocol message that used to push one has been removed. This
// keeps the final say over what may run on a host with the host itself — a
// local agent cannot override or relax it.
//
// # Allowlist resolution
//
// The config has two levels of allowlists:
//
//   - allowCommands — the base list, applied to every user regardless of group
//     membership. If absent or empty in the file, DefaultAllowCommands is used
//     so the remote is never unrestricted.
//   - groups.<name>.allowCommands — extra commands granted to users who are
//     members of that system group. Multiple group entries are all merged
//     together with the base (additive, never subtractive). A user in both
//     "operations" and "web" gets the union of both groups' entries plus the
//     base.
//
// The actual merging is done by Resolve, which the remote executor calls at
// startup after looking up the current user's system group memberships.
package remoteconfig

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"myhostmcp/internal/allowlist"
)

// DefaultPath is the conventional remote config file location.
const DefaultPath = "/etc/myhostmcp/config.yaml"

// DefaultAllowCommands is used when no allowlist is configured (file missing
// or allowCommands absent/empty) and no group commands apply either.
// The remote is never unrestricted.
var DefaultAllowCommands = []allowlist.Rule{
	{Prefix: []string{"df"}},
	{Prefix: []string{"ss"}},
	{Prefix: []string{"top"}},
	{Prefix: []string{"free"}},
	{Prefix: []string{"ls"}},
}

// Config is the remote-half configuration.
type Config struct {
	// AllowCommands is the base allowlist. It applies to all users,
	// regardless of group membership. It is never empty after Load: if the
	// file omits or empties it, Load substitutes DefaultAllowCommands.
	AllowCommands []allowlist.Rule

	// GroupAllowCommands maps system group name → extra allowed rules
	// granted to members of that group. Resolve merges these with
	// AllowCommands based on the calling user's actual group memberships.
	// It is nil when no groups are configured.
	GroupAllowCommands map[string][]allowlist.Rule

	// Path is the file actually loaded, set by Load for diagnostics. It is the
	// resolved path even when the file was missing (and the default was used).
	Path string
}

// Resolve returns the effective allowlist for a user who is a member of the
// given system groups. It starts with AllowCommands (the base, always
// non-empty after Load) and appends the command prefixes of every configured
// group the user belongs to. Groups that are not present in GroupAllowCommands
// are silently ignored, so passing the full list of the user's system groups
// is safe.
//
// The merged list may contain duplicate prefixes if a command appears in both
// the base and a group list; that is harmless — prefix matching checks each
// entry independently.
//
// If the merged result would somehow be empty (e.g. Config was constructed
// outside Load), DefaultAllowCommands is returned so the remote is never
// unrestricted.
func (c *Config) Resolve(userGroups []string) []allowlist.Rule {
	merged := clone(c.AllowCommands)
	for _, g := range userGroups {
		if cmds, ok := c.GroupAllowCommands[g]; ok {
			merged = append(merged, cmds...)
		}
	}
	if len(merged) == 0 {
		return clone(DefaultAllowCommands)
	}
	return merged
}

// yamlGroupConfig is the on-disk shape of a single group stanza.
type yamlGroupConfig struct {
	AllowCommands []yaml.Node `yaml:"allowCommands"`
}

// yamlConfig is the on-disk shape. Each allowCommands entry is either:
//
//   - a plain string, split on whitespace into a literal prefix at load time
//     (the original, still-supported shorthand: "- df", "- systemctl restart"), or
//
//   - a mapping with a "command" prefix string and an optional "requireFlags"
//     list, for rules that must pin down specific flag values while leaving
//     everything else about the command free-form:
//
//   - command: "panelssh-static -panel"
//     requireFlags:
//
//   - flag: "-panel"
//     pattern: "^[0-9]+$"
//
//   - flag: "-cmd"
//
//     "requireFlags" entries with no "pattern" accept any non-empty value.
//     "command" is still split on whitespace, so the fixed-prefix and
//     variable-flag parts can be combined freely.
type yamlConfig struct {
	AllowCommands []yaml.Node                `yaml:"allowCommands"`
	Groups        map[string]yamlGroupConfig `yaml:"groups"`
}

// yamlFlagRequirement is the on-disk shape of one requireFlags entry.
type yamlFlagRequirement struct {
	Flag    string `yaml:"flag"`
	Pattern string `yaml:"pattern"`
}

// yamlRule is the on-disk shape of a structured (mapping) allowCommands
// entry.
type yamlRule struct {
	Command      string                `yaml:"command"`
	RequireFlags []yamlFlagRequirement `yaml:"requireFlags"`
}

// decodeRules converts one allowCommands sequence (a mix of plain strings and
// structured mappings, per yamlConfig's doc comment) into allowlist.Rules. It
// is a hard error if a mapping entry is missing "command", a requireFlags
// entry is missing "flag", or a pattern fails to compile as a regexp — a
// malformed allowlist should fail loudly rather than silently be narrower or
// wider than the admin intended.
func decodeRules(nodes []yaml.Node) ([]allowlist.Rule, error) {
	var rules []allowlist.Rule
	for _, n := range nodes {
		switch n.Kind {
		case yaml.ScalarNode:
			var raw string
			if err := n.Decode(&raw); err != nil {
				return nil, fmt.Errorf("allowCommands entry: %w", err)
			}
			if toks := strings.Fields(raw); len(toks) > 0 {
				rules = append(rules, allowlist.Rule{Prefix: toks})
			}
		case yaml.MappingNode:
			var yr yamlRule
			if err := n.Decode(&yr); err != nil {
				return nil, fmt.Errorf("allowCommands entry: %w", err)
			}
			prefix := strings.Fields(yr.Command)
			if len(prefix) == 0 {
				return nil, fmt.Errorf("allowCommands entry: \"command\" is required and must be non-empty")
			}
			var reqs []allowlist.FlagRequirement
			for _, rf := range yr.RequireFlags {
				if rf.Flag == "" {
					return nil, fmt.Errorf("allowCommands entry %q: requireFlags entry missing \"flag\"", yr.Command)
				}
				if rf.Pattern != "" {
					if _, err := regexp.Compile(rf.Pattern); err != nil {
						return nil, fmt.Errorf("allowCommands entry %q: invalid pattern %q for flag %q: %w",
							yr.Command, rf.Pattern, rf.Flag, err)
					}
				}
				reqs = append(reqs, allowlist.FlagRequirement{Flag: rf.Flag, Pattern: rf.Pattern})
			}
			rules = append(rules, allowlist.Rule{Prefix: prefix, RequireFlags: reqs})
		default:
			return nil, fmt.Errorf("allowCommands entry must be a string or a mapping, got %v", n.Tag)
		}
	}
	return rules, nil
}

// Load reads a config file. If path is empty, DefaultPath is used. A missing
// file is not an error: the default allowlist is returned. A malformed file IS
// an error — fail loud rather than silently switching to a different policy
// than the admin intended. If the file parses but allowCommands is empty, the
// default allowlist is returned (the base is never empty).
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath
	}
	c := &Config{Path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			c.AllowCommands = clone(DefaultAllowCommands)
			return c, nil
		}
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	var yc yamlConfig
	if err := yaml.Unmarshal(data, &yc); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	rules, err := decodeRules(yc.AllowCommands)
	if err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	c.AllowCommands = rules
	if len(c.AllowCommands) == 0 {
		c.AllowCommands = clone(DefaultAllowCommands)
	}
	// Parse per-group allowlists. Groups with no commands are silently dropped
	// (they would have no effect on Resolve).
	if len(yc.Groups) > 0 {
		c.GroupAllowCommands = make(map[string][]allowlist.Rule, len(yc.Groups))
		for name, gc := range yc.Groups {
			cmds, err := decodeRules(gc.AllowCommands)
			if err != nil {
				return nil, fmt.Errorf("parse config %q group %q: %w", path, name, err)
			}
			if len(cmds) > 0 {
				c.GroupAllowCommands[name] = cmds
			}
		}
	}
	return c, nil
}

func clone(in []allowlist.Rule) []allowlist.Rule {
	out := make([]allowlist.Rule, len(in))
	for i, r := range in {
		cp := allowlist.Rule{Prefix: append([]string(nil), r.Prefix...)}
		if r.RequireFlags != nil {
			cp.RequireFlags = append([]allowlist.FlagRequirement(nil), r.RequireFlags...)
		}
		out[i] = cp
	}
	return out
}

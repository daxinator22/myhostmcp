// Package allowlist validates shell commands against an optional command
// allowlist.
//
// If the allowlist is empty, every command is permitted (unrestricted mode).
// When non-empty, every segment of the command (split at control operators
// such as ;, |, &&, ||) must begin with the tokens of one allowlist entry
// (Rule.Prefix). A Rule may additionally require specific flags — e.g.
// "-panel" must be present with a numeric value — via RequireFlags; anything
// not covered by a requirement is unconstrained. Constructs that could bypass
// the allowlist — command substitution ($(...) or backtick) and I/O
// redirection (<, >, >>, <<, >&, <&) — are rejected when an allowlist is
// active.
//
// This is a safety heuristic, not a formal shell parser. It is deliberately
// conservative: it may reject unusual but legitimate commands. With no
// allowlist configured it does nothing.
package allowlist

import (
	"fmt"
	"regexp"
	"strings"
)

// FlagRequirement demands that a flag appear somewhere in the matched
// command, followed by a value. If Pattern is empty, any non-empty value
// satisfies the requirement; otherwise the value must fully match the regexp
// (anchored as if wrapped in ^(?:...)$).
type FlagRequirement struct {
	Flag    string `json:"flag"`
	Pattern string `json:"pattern,omitempty"`
}

// Rule is one allowlist entry. A command segment matches a Rule if its
// leading tokens equal Prefix (the same literal-prefix matching as before),
// AND, for every entry in RequireFlags, that flag appears among the
// segment's tokens (anywhere, not just immediately after the prefix)
// followed by a value satisfying its Pattern. Flags and arguments not
// mentioned in RequireFlags are unconstrained: they may appear in any order,
// anywhere, or not at all.
//
// A Rule with no RequireFlags behaves exactly like the old plain
// literal-prefix entry.
type Rule struct {
	Prefix       []string          `json:"prefix"`
	RequireFlags []FlagRequirement `json:"requireFlags,omitempty"`
}

// String renders r for diagnostics/display, e.g.:
//
//	panelssh-static -panel <^[0-9]+$> -cmd <value>
func (r Rule) String() string {
	s := strings.Join(r.Prefix, " ")
	for _, req := range r.RequireFlags {
		if req.Pattern != "" {
			s += fmt.Sprintf(" %s <%s>", req.Flag, req.Pattern)
		} else {
			s += fmt.Sprintf(" %s <value>", req.Flag)
		}
	}
	return s
}

// Validate returns nil if cmd is permitted by the allowlist, or an error
// explaining why it is not.
func Validate(cmd string, allowed []Rule) error {
	if len(allowed) == 0 {
		return nil // unrestricted
	}
	segs, danger, err := tokenize(cmd)
	if err != nil {
		return fmt.Errorf("could not parse command: %w", err)
	}
	if danger != "" {
		return fmt.Errorf("disallowed construct: %s", danger)
	}
	anyNonEmpty := false
	for _, seg := range segs {
		if len(seg) == 0 {
			continue
		}
		anyNonEmpty = true
		// Skip leading VAR=value assignments (e.g. "FOO=bar df -h" => "df").
		toks := seg
		for len(toks) > 0 && isAssignment(toks[0]) {
			toks = toks[1:]
		}
		if len(toks) == 0 {
			return fmt.Errorf("segment has no command word: %q", strings.Join(seg, " "))
		}
		if !matchesAny(toks, allowed) {
			return fmt.Errorf("command not allowed: %q", strings.Join(seg, " "))
		}
	}
	if !anyNonEmpty {
		return fmt.Errorf("empty command")
	}
	return nil
}

func matchesAny(toks []string, allowed []Rule) bool {
	for _, r := range allowed {
		if matchesRule(toks, r) {
			return true
		}
	}
	return false
}

func matchesRule(toks []string, r Rule) bool {
	a := r.Prefix
	if len(a) == 0 || len(toks) < len(a) {
		return false
	}
	for i := range a {
		if toks[i] != a[i] {
			return false
		}
	}
	for _, req := range r.RequireFlags {
		if !hasRequiredFlag(toks, req) {
			return false
		}
	}
	return true
}

// hasRequiredFlag reports whether toks contains req.Flag followed by a
// value satisfying req.Pattern (or any non-empty value, if Pattern is
// empty). An invalid regexp never matches — such a rule can never be
// satisfied, which is the safe failure mode.
func hasRequiredFlag(toks []string, req FlagRequirement) bool {
	var re *regexp.Regexp
	if req.Pattern != "" {
		var err error
		re, err = regexp.Compile("^(?:" + req.Pattern + ")$")
		if err != nil {
			return false
		}
	}
	for i := 0; i+1 < len(toks); i++ {
		if toks[i] != req.Flag {
			continue
		}
		val := toks[i+1]
		if val == "" {
			continue
		}
		if re == nil || re.MatchString(val) {
			return true
		}
	}
	return false
}

func isAssignment(s string) bool {
	i := strings.IndexByte(s, '=')
	if i <= 0 {
		return false
	}
	for j := 0; j < i; j++ {
		c := s[j]
		switch {
		case c >= 'A' && c <= 'Z':
		case c >= 'a' && c <= 'z':
		case c == '_':
		case c >= '0' && c <= '9' && j > 0:
		default:
			return false
		}
	}
	return true
}

// tokenize splits cmd into segments at control operators (;, |, &&, ||, &),
// honouring single and double quotes and backslash escapes. It also detects
// "danger" constructs (command substitution, redirection) that appear outside
// quotes; if any are present, danger is a non-empty description.
func tokenize(cmd string) (segs [][]string, danger string, err error) {
	var cur []string
	i, n := 0, len(cmd)
	flush := func() { segs = append(segs, cur); cur = nil }

	for i < n {
		c := cmd[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++

		case c == '\'':
			j := i + 1
			for j < n && cmd[j] != '\'' {
				j++
			}
			if j >= n {
				return nil, "", fmt.Errorf("unterminated single quote")
			}
			cur = append(cur, cmd[i+1:j])
			i = j + 1

		case c == '"':
			var b strings.Builder
			j := i + 1
			for j < n && cmd[j] != '"' {
				if cmd[j] == '\\' && j+1 < n {
					switch cmd[j+1] {
					case '"', '\\', '$', '`':
						b.WriteByte(cmd[j+1])
						j += 2
						continue
					}
				}
				b.WriteByte(cmd[j])
				j++
			}
			if j >= n {
				return nil, "", fmt.Errorf("unterminated double quote")
			}
			cur = append(cur, b.String())
			i = j + 1

		case c == '\\':
			if i+1 < n {
				cur = append(cur, string(cmd[i+1]))
				i += 2
			} else {
				i++
			}

		case c == '`':
			if danger == "" {
				danger = "backtick command substitution"
			}
			i++

		case c == '$' && i+1 < n && cmd[i+1] == '(':
			if danger == "" {
				danger = "$(...) command substitution"
			}
			i += 2
			depth := 1
			for i < n && depth > 0 {
				switch cmd[i] {
				case '(':
					depth++
				case ')':
					depth--
				}
				i++
			}

		case c == '|':
			if i+1 < n && cmd[i+1] == '|' {
				i += 2
			} else {
				i++
			}
			flush()

		case c == '&':
			if i+1 < n && cmd[i+1] == '&' {
				i += 2
			} else {
				i++ // background operator
			}
			flush()

		case c == ';':
			i++
			flush()

		case c == '>' || c == '<':
			if danger == "" {
				danger = "I/O redirection"
			}
			i++
			if i < n && (cmd[i] == '>' || cmd[i] == '<' || cmd[i] == '&') {
				i++
			}
			// Skip the redirection target word (and any quotes around it) so it
			// is not mistaken for a command token.
			for i < n && (cmd[i] == ' ' || cmd[i] == '\t') {
				i++
			}
			if i < n && (cmd[i] == '\'' || cmd[i] == '"') {
				q := cmd[i]
				i++
				for i < n && cmd[i] != q {
					i++
				}
				if i < n {
					i++
				}
			} else {
				for i < n && cmd[i] != ' ' && cmd[i] != '\t' && cmd[i] != '\n' &&
					cmd[i] != ';' && cmd[i] != '|' && cmd[i] != '&' {
					i++
				}
			}

		default:
			j := i
			var b strings.Builder
			for j < n {
				d := cmd[j]
				if d == ' ' || d == '\t' || d == '\n' || d == '\r' {
					break
				}
				if d == ';' || d == '|' || d == '&' || d == '>' || d == '<' ||
					d == '\'' || d == '"' || d == '\\' || d == '`' {
					break
				}
				b.WriteByte(d)
				j++
			}
			if j == i { // single special char not handled above
				b.WriteByte(c)
				j = i + 1
			}
			cur = append(cur, b.String())
			i = j
		}
	}
	flush()
	return segs, danger, nil
}

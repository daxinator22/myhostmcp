// Package agentinstr holds the MCP agent instructions string that is injected
// into every initialize response (via mcp.ServerOptions.Instructions) so
// clients receive usage guidance at load time without any system-prompt
// editing.  Both the stdio server (internal/local) and the HTTP server
// (internal/httpserver) import this package so the text stays in one place.
package agentinstr

// Instructions is delivered to the agent in the MCP initialize response.
const Instructions = `myhostmcp usage rules (follow throughout the session):

1. CONNECT ONCE — call remote_connect once per host; reuse the default session
   for the whole investigation.  Check remote_status to see what is already
   open.  Call remote_disconnect only when you are completely finished.

2. ONE COMMAND PER remote_exec — the allowlist forbids ';', '&&', '||', pipes,
   and command-substitution chains.  Prefer a single command with its native
   flags (e.g. "journalctl -u foo -n 100") over any compound line.

3. ALWAYS SET timeout — SSH credential fetch (especially Teleport/tsh) can
   take up to a minute.  Use at least "60s" for the first call after connect
   and "30s"–"90s" thereafter.  Never leave timeout empty.

4. READ CHEAP BEFORE EXPENSIVE — within each host visit prefer this order:
     a) identity / version: uname, hostname, cat /etc/os-release
     b) build / deploy state: systemctl status, cat version/build files
     c) capacity: df -h, free -h
     d) targeted log tails: journalctl -n, tail -n
     e) database / REST reads (most expensive — do only when necessary)
   Keep the total number of remote_exec calls per host small.
`

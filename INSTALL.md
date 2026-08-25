# Installing myhostmcp

`myhostmcp` lets an AI agent run commands on a remote host in a persistent
session. It is a single Go binary with **two ways for an agent to reach it**,
plus the remote executor they both drive:

| Mode | Binary invocation | How the agent connects | Credentials | Best for |
|------|-------------------|-------------------------|-------------|----------|
| **A — Local (stdio)** | `myhostmcp local` | Agent spawns it as a **child process** and speaks MCP over stdin/stdout. It SSHes (or `tsh`) out to the target host per `remote_connect`, using **your normal local SSH credentials**. | Your local `~/.ssh/config`, keys, ssh-agent, Teleport certs. No credentials embedded anywhere. | An agent running on a machine that already has SSH/Teleport access to the target(s) — desktop use, one-off scripts, multiple target hosts from one agent. |
| **B — HTTP endpoint** | `myhostmcp serve-http` | A persistent process runs **on the target host** and exposes an HTTPS MCP endpoint. The agent connects over the network as an ordinary HTTP MCP client with Basic or Bearer token auth. No SSH hop — commands run in a local shell on that host. | A `username:token` pair issued in that host's `/etc/myhostmcp/http-auth.yaml`. | Cloud/remote agents with no SSH path to the target, or any agent you'd rather hand a scoped token than SSH access. |

Both modes drive the same `myhostmcp remote` executor and expose the same five
tools (`remote_connect`, `remote_exec`, `remote_allowed_commands`,
`remote_status`, `remote_disconnect`), and both are subject to the same
host-side command allowlist. **Pick one mode per agent** based on whether that
agent has SSH access to your targets (Mode A) or not (Mode B) — decide this
before following the per-agent instructions below, since the two modes are
configured and wired up completely differently.

This guide covers building it, then gives fully separate setup paths for
**Mode A** and **Mode B**, each with wiring instructions for **Claude Code**,
**OpenCode**, and **pi** (from pi.dev).

---

## 1. Prerequisites

- **Go 1.23+** to build (`go version`).
- **GNU `timeout(1)`** on every target host, for both modes, for per-command
  timeouts (present on essentially all Linux servers; part of coreutils). If
  absent, a Go deadline backstop still bounds commands (by killing the session
  on timeout).
- **Mode A only:** SSH access to the target host(s), working from a shell:
  ```sh
  ssh yourhost 'uname -sm; id'
  ```
  `myhostmcp` shells out to the real `ssh` binary, so whatever already works
  for you — `~/.ssh/config` aliases, default keys (`~/.ssh/id_ed25519`, etc.),
  ssh-agent, `ProxyJump` bastions, `known_hosts` — works with zero extra
  setup. Teleport / `tsh` is also supported (see [Teleport](#teleport-tsh)).
- **Mode B only:** the ability to run a process and open a port on the target
  host itself (you're installing a server there), plus a TLS cert/key for it.

## 2. Build

From the repo root:

```sh
./build.sh            # cross-compile remote binaries, embed them, build local
```

This produces `build/myhostmcp` — a single static binary with the cross-compiled
remote halves (linux/amd64, linux/arm64, linux/arm, darwin/amd64, darwin/arm64,
freebsd/amd64, freebsd/arm64) embedded inside. At connect time (Mode A) it picks
the right one for the remote host's `uname -sm` and uploads it. For Mode B, the
same binary runs `serve-http` directly on the target host, so build (or cross-
compile) for *that* host's OS/arch.

If you only ever deploy to the same OS/arch as your build machine:

```sh
./build.sh current && ./build.sh local
```

Verify:

```sh
build/myhostmcp version
build/myhostmcp demo                       # executor self-test, no SSH needed
build/myhostmcp demo --allow "df,free,ps,ss,echo,cd,pwd,systemctl restart"
```

## 3. The command allowlist (applies to both modes)

Whichever mode an agent connects through, commands are enforced by the
**remote host itself** — the local half (Mode A) or the connecting client
(Mode B) can query the allowlist but never relax it. It lives at
`/etc/myhostmcp/config.yaml` on the target host (override with `myhostmcp
remote --config <path>` / `myhostmcp serve-http --config <path>`). If the file
is absent or `allowCommands` is empty, a safe built-in default is used (`df`,
`ss`, `top`, `free`, `ls`) — the remote is never unrestricted.

Install it on each target host (requires root):

```sh
sudo mkdir -p /etc/myhostmcp
sudo cp config.remote.example.yaml /etc/myhostmcp/config.yaml
sudoedit /etc/myhostmcp/config.yaml
```

Every segment of a command (split at `;`, `|`, `&&`, `||`, `&`) must begin
with one entry's tokens. Command substitution (`$(...)`, backtick) and I/O
redirection (`<`, `>`, `>>`, `<<`) are rejected while an allowlist is active.

#### Base allowlist

The `allowCommands` list applies to **every** user on the host:

```yaml
allowCommands:
  - df
  - free
  - ps
  - ss
  - "systemctl status"   # prefix: allows any `systemctl status …`
  - "systemctl restart"  # but NOT `systemctl stop …`
```

#### Group-based allowlists

Use the `groups` key to grant **additional** commands to users who are members
of a specific system group (Mode A, where the connecting identity is an SSH/OS
user) or mapped via `users[].groups` in `http-auth.yaml` (Mode B, where the
connecting identity is a token user — see [Mode B](#mode-b--http-endpoint)).
The effective allowlist is the **union** of the base `allowCommands` and every
group the user belongs to / is mapped to.

```yaml
allowCommands:
  - df
  - ls

groups:
  # Users/tokens mapped to "operations" get these on top of the base.
  operations:
    allowCommands:
      - systemctl       # all subcommands
      - journalctl
      - dmesg
      - lsof

  # Users/tokens mapped to "web" get these on top of the base.
  web:
    allowCommands:
      - "nginx -t"
      - "systemctl reload nginx"
      - curl
```

For Mode A, group membership is looked up from the OS at startup (`/etc/group`
or nsswitch), so it picks up LDAP/NIS groups too. A user in both `operations`
and `web` gets all three lists merged. `remote_allowed_commands` always
returns the **effective** merged list for the connecting session.

See `config.remote.example.yaml` for a fuller example with `operations`,
`web`, and `dba` groups.

Enforcement happens **on the remote half**, so the policy cannot be bypassed
by anything the agent or local half does; disallowed commands come back as
remote errors. In Mode A, the remote must be version-matched to the local half
(the local half re-uploads on a version mismatch), so after changing
`/etc/myhostmcp/config.yaml` just reconnect — no restart needed. In Mode B,
`serve-http` reads it directly, so the same applies — no restart needed after
an edit.

---

## Mode A — Local (stdio)

Use this when the agent's own machine already has SSH (or Teleport) access to
the target host(s).

### A.1 Configure the local half (optional)

The local half reads `~/.myhostmcp/config.yaml` by default (override with
`myhostmcp local --config <path>`). All fields are optional. Copy
`config.example.yaml` to get started:

```sh
mkdir -p ~/.myhostmcp
cp config.example.yaml ~/.myhostmcp/config.yaml
```

Notable fields:

| Field | Purpose |
|-------|---------|
| `defaultHost` | used when `remote_connect` omits its `host` argument |
| `defaultUser`, `defaultPort` | defaults for connect |
| `identityFiles` | list of `-i` key paths; empty = whatever `ssh` normally uses |
| `remoteInstallDir` | where the remote binary is placed on the host (default `~/.myhostmcp`) |
| `strictHostKeyChecking` | passed to SSH `StrictHostKeyChecking` (default `accept-new`; set `yes` for stricter host-key policy) |
| `connectTimeout`, `execTimeout` | durations like `15s`, `60s` |
| `logFile` | local-half diagnostics; must NOT be stdout (stdout is MCP). Default: stderr. |
| `transport` | `"auto"` (default), `"ssh"`, or `"tsh"` |
| `teleportProxy` | Teleport proxy address for `tsh login`, e.g. `proxy.example.com:443` |
| `teleportCluster` | optional Teleport cluster / leaf-cluster name for `tsh login` |

> **Teleport:** `teleportProxy` and `teleportCluster` are used by the automatic
> `tsh login` step if the certificate is missing or expired. They can also be
> overridden per-call in `remote_connect { teleportProxy: "...", teleportCluster: "..." }`.
> See [Teleport (tsh)](#teleport-tsh) for the full flow.

> The command allowlist is **not** set here — see [section 3](#3-the-command-allowlist-applies-to-both-modes)
> above; it lives on the remote host and the local half can only query it.

### A.2 Wire it into your agent

The local half is a **stdio MCP server**: the agent launches it as a child
process and talks MCP over its stdin/stdout. It does nothing (no SSH) on
startup — it connects lazily on the first `remote_connect` (or `remote_exec`
against a configured `defaultHost`).

In the snippets below, replace `/path/to/build/myhostmcp` with the absolute
path to your built binary (e.g. `/home/you/myhostmcp/build/myhostmcp`).

#### Claude Code

Claude Code reads stdio MCP servers from `.mcp.json` (project scope),
`~/.claude.json` (user scope), or via the `claude mcp add` command.

**Option A — CLI** (writes to local scope by default; add `--scope user` for
all projects, or `--scope project` for a shared `.mcp.json`):

```sh
claude mcp add myhostmcp --scope user -- /path/to/build/myhostmcp local
```

With a config file:

```sh
claude mcp add myhostmcp --scope user -- /path/to/build/myhostmcp local --config /home/you/.myhostmcp/config.yaml
```

**Option B — JSON** (`.mcp.json` in the project root, or the `mcpServers`
object in `~/.claude.json`):

```json
{
  "mcpServers": {
    "myhostmcp": {
      "command": "/path/to/build/myhostmcp",
      "args": ["local", "--config", "/home/you/.myhostmcp/config.yaml"]
    }
  }
}
```

Verify inside Claude Code with `/mcp` (lists servers and connection status).
The first time you use a project-scoped server you may be asked to approve it.

#### OpenCode

OpenCode reads MCP servers from its JSON config (`opencode.json` or
`opencode.jsonc`) at the project root, or `~/.config/opencode/opencode.json`
for a global setup. Local stdio servers use `"type": "local"` with a
`command` array.

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "myhostmcp": {
      "type": "local",
      "command": ["/path/to/build/myhostmcp", "local", "--config", "/home/you/.myhostmcp/config.yaml"],
      "enabled": true
    }
  }
}
```

`timeout` (ms, default 5000) can be set alongside `enabled` for the startup
tool-list fetch. Once added, the `remote_*` tools are available to the LLM
alongside built-in tools.

#### pi (from pi.dev)

**pi has no built-in MCP support** (by design — see pi's
[extensions docs](https://pi.dev)). You bridge to `myhostmcp local` with a
small extension that spawns it as a subprocess and proxies pi tools through. A
ready-to-use, zero-dependency extension is included at
`examples/pi-extension/myhostmcp.ts`.

1. Build myhostmcp (step 2 above).

2. Copy the extension into pi's global extensions directory:
   ```sh
   mkdir -p ~/.pi/agent/extensions
   cp examples/pi-extension/myhostmcp.ts ~/.pi/agent/extensions/myhostmcp.ts
   ```
   (Use `.pi/extensions/` instead for a single project.)

3. Tell the extension where the binary is. Set in your shell environment
   (e.g. `~/.bashrc` / `~/.zshrc`), since pi reads the process environment:
   ```sh
   export MYHOSTMCP_BINARY=/path/to/build/myhostmcp
   # optional:
   export MYHOSTMCP_CONFIG=/home/you/.myhostmcp/config.yaml
   ```
   If `MYHOSTMCP_BINARY` is unset it falls back to `./build/myhostmcp`, then
   `myhostmcp` on `$PATH`, so alternatively copy/symlink the binary somewhere
   on `$PATH`.

4. Start pi (or run `/reload` in a running session). The five `remote_*` tools
   become available. The extension starts `myhostmcp local` when a session
   begins and stops it when the session ends.

How the extension works: on `session_start` it spawns `myhostmcp local`, does
the MCP `initialize` handshake, and registers five pi custom tools
(`remote_connect`, `remote_exec`, `remote_allowed_commands`, `remote_status`,
`remote_disconnect`) whose `execute` functions forward calls to the MCP server
and map the results back to pi's tool-result shape. See
`examples/pi-extension/myhostmcp.ts` for the source.

### A.3 Verify Mode A works

Ask your agent to connect and run something on a host you can SSH to:

> Use the remote_connect tool to connect to `myserver` (host alias from my
> ~/.ssh/config). Then use remote_exec to run `uname -a` and `df -h`.

You should see the agent call `remote_connect`, then `remote_exec`, and report
the remote output. `cd` and `export` persist across `remote_exec` calls within
the same session — try "cd /var/log then ls" as two separate commands.

You can also drive the full stdio path directly (no agent) to debug:

```sh
go run ./examples/stdio_client -binary ./build/myhostmcp -host myserver
```

---

## Mode B — HTTP endpoint

Use this when the agent has no SSH path to the target host — e.g. a cloud or
remote agent — and you'd rather hand it a scoped token than SSH access.
`serve-http` runs **on the target host** and turns it into a self-contained
HTTPS MCP endpoint; sessions run locally on that host (no SSH hop).

### B.1 Set up the server (on the target host)

```sh
sudo mkdir -p /etc/myhostmcp
sudo cp config.http-server.example.yaml /etc/myhostmcp/http-server.yaml
sudo cp config.http-auth.example.yaml /etc/myhostmcp/http-auth.yaml
sudo chmod 600 /etc/myhostmcp/http-auth.yaml
sudoedit /etc/myhostmcp/http-server.yaml
sudoedit /etc/myhostmcp/http-auth.yaml

# then run (or via systemd, see below)
myhostmcp serve-http --config /etc/myhostmcp/http-server.yaml
```

Notes:
- TLS is required (`tlsCertFile`, `tlsKeyFile`).
- Auth supports both:
  - HTTP Basic (`Authorization: Basic base64(username:token)`)
  - Bearer (`Authorization: Bearer <token>`)
- `/etc/myhostmcp/http-auth.yaml` supports plaintext `tokens` and bcrypt
  `tokenHashes` (recommended). **Issue one token per agent/user** so access
  can be revoked individually.
- Optional `users[].groups` in `http-auth.yaml` map token users to allowlist
  groups defined under `groups:` in `/etc/myhostmcp/config.yaml` (see
  [section 3](#3-the-command-allowlist-applies-to-both-modes)).
- `http-auth.yaml` must not be world-accessible, or startup fails.
- Tool names are unchanged (`remote_connect`, `remote_exec`, ...), but
  sessions are local to that host (no SSH hop, no `host` argument needed on
  `remote_connect`).

#### Run `serve-http` as a systemd service

An example unit is provided at `examples/systemd/myhostmcp-serve-http.service`.

```sh
sudo cp examples/systemd/myhostmcp-serve-http.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now myhostmcp-serve-http.service
sudo systemctl status myhostmcp-serve-http.service
```

If you run as non-root, ensure that user can read:
- `/etc/myhostmcp/http-server.yaml`
- `/etc/myhostmcp/http-auth.yaml`
- the TLS cert/key files configured in `http-server.yaml`

### B.2 Get a credential for the agent

Before wiring up any client, obtain a `username:token` (or plain bearer
token) pair issued in that host's `http-auth.yaml`. Treat it as a secret:
prefer environment variables over hardcoding it in a config file, so it's
never committed or logged. Each snippet below assumes you've exported:

```sh
export MYHOSTMCP_URL="https://your-host:8443"
export MYHOSTMCP_BASIC_AUTH="$(printf '%s' 'username:token' | base64)"   # Basic auth value
# — or, for bearer auth instead —
export MYHOSTMCP_BEARER_TOKEN="your-token"
```

### B.3 Wire it into your agent

#### Claude Code

Claude Code's `.mcp.json` / `~/.claude.json` support `${VAR}` (and
`${VAR:-default}`) expansion inside `url` and `headers` for HTTP-type MCP
servers. The `claude mcp add` CLI does **not** expand `${VAR}` in
`--header` values (it stores whatever literal string you pass), so for
env-var-based config, hand-edit the JSON directly:

```json
{
  "mcpServers": {
    "myhostmcp": {
      "type": "http",
      "url": "${MYHOSTMCP_URL}",
      "headers": {
        "Authorization": "Basic ${MYHOSTMCP_BASIC_AUTH}"
      }
    }
  }
}
```

Put this under `mcpServers` in `~/.claude.json` for user scope (all
projects), or in a `.mcp.json` at the repo root for project scope. Restart
Claude Code (or start a new session) so it picks up the exported env vars,
then check with `/mcp` — a missing-variable warning there means the env var
wasn't set in the shell Claude Code was launched from.

If you'd rather not export secrets into the shell environment at all, use
`headersHelper` instead of `headers` — a shell command Claude Code runs at
each connection to produce the headers JSON on stdout:

```json
{
  "mcpServers": {
    "myhostmcp": {
      "type": "http",
      "url": "${MYHOSTMCP_URL}",
      "headersHelper": "echo '{\"Authorization\": \"Basic '\"$(your-secret-lookup-command)\"'\"}'"
    }
  }
}
```

#### OpenCode

OpenCode uses `"type": "remote"` for HTTP MCP servers, with a `url` field and
an optional `headers` object. Env var substitution uses `{env:VAR_NAME}`
(not `${VAR}`). Since this server uses static Basic/Bearer auth rather than
OAuth, set `"oauth": false` so OpenCode treats `headers` as the sole auth
mechanism instead of attempting OAuth discovery:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "myhostmcp": {
      "type": "remote",
      "url": "{env:MYHOSTMCP_URL}",
      "oauth": false,
      "headers": {
        "Authorization": "Basic {env:MYHOSTMCP_BASIC_AUTH}"
      }
    }
  }
}
```

(Use `"Authorization": "Bearer {env:MYHOSTMCP_BEARER_TOKEN}"` instead if the
token was issued for bearer auth.) Add this to `opencode.json`/
`opencode.jsonc` at the project root, or `~/.config/opencode/opencode.json`
for a global setup.

#### pi (from pi.dev)

**Not currently supported by the bundled extension.** `examples/pi-extension/myhostmcp.ts`
only speaks stdio to a locally-spawned `myhostmcp local` process — it has no
HTTP transport. Since pi has no built-in MCP support at all, using Mode B from
pi would require writing a second extension variant that speaks MCP over
HTTP (streamable HTTP, with the same Basic/Bearer header) instead of spawning
a subprocess, following the same tool-registration pattern as the existing
extension. That variant isn't included in this repo yet.

If your agent is pi and the target host has no SSH path to it, the practical
options today are: add SSH/Teleport access so you can use Mode A instead, or
write the HTTP-transport pi extension yourself using the stdio one as a
template (swap `McpStdioClient` for an `fetch`-based client against the
streamable HTTP endpoint, carrying the same `Authorization` header on every
request).

### B.4 Verify Mode B works

> Use remote_connect (no host needed — it's local to this endpoint), then
> remote_exec `uname -a`.

Tool calls should return without an SSH hop. Use `remote_allowed_commands` to
see the effective allowlist for that token's mapped group(s).

---

## Known limitations

- PTY mode (`remote_exec` with `pty=true`) is not implemented yet.
- Allowlist parsing is intentionally conservative: redirection and command
  substitution are blocked while an allowlist is active.
- If GNU `timeout(1)` is missing on the target host, timeouts still work via a
  Go backstop, but timeout handling may terminate the session.
- Mode B has no pi client yet (see [above](#pi-from-pidev-1)).

## Teleport (tsh)

Mode A only. `myhostmcp` automatically uses `tsh` when it is found in `$PATH`
(`transport: auto`). The full workflow on each `remote_connect`:

1. **Check login status** — `tsh status` is run silently. If the certificate
   is valid, the connection proceeds immediately (no overhead).
2. **Automatic login** — if the cert is missing or expired, `tsh login` is
   called using the configured proxy and cluster:
   ```yaml
   # ~/.myhostmcp/config.yaml
   transport: auto           # or "tsh" to disable ssh fallback
   teleportProxy: proxy.example.com:443
   teleportCluster: prod     # optional leaf cluster
   ```
   - **Browser-based SSO** (Okta, GitHub, Google, etc.) — a browser window
     opens on your desktop automatically. The agent waits for the flow to
     complete before proceeding.
   - **Terminal-based auth** (local password, TOTP/OTP) — `tsh login` needs
     to write prompts to a terminal, which is not available while the MCP
     server is running (its stdin/stdout carry the MCP protocol). Run
     `tsh login` manually in your terminal first, or switch to browser SSO or
     device trust / machine certificates.
3. **PTY + session recording** — the persistent session is started with `-tt`
   (force PTY allocation even without a local terminal) so Teleport can record
   it. The PTY is immediately put into raw mode (`stty raw -echo`) so that
   output CRLF translation and input echo are both disabled — the JSON
   protocol framing is unaffected.
4. **Fallback to ssh** — if tsh is not in PATH, the login fails, or the host
   is not a Teleport node, the connection falls back transparently to plain
   `ssh`. The `fallbackNote` field in the `remote_connect` result explains
   why. Use `transport: tsh` to disable the fallback (return an error
   instead).

To confirm Teleport is being used, check the `transport` field in the
`remote_connect` result or call `remote_status`.

## Troubleshooting

**"failed to connect to HOST: ..." / SSH auth errors (Mode A).** Make sure
plain `ssh HOST` works from the same shell you launch the agent from.
`myhostmcp` passes `-o BatchMode=yes`, so it will never prompt — if your key
needs a passphrase, load it into `ssh-agent` (`ssh-add`). To use a specific
key, pass `identityFile` to `remote_connect` or set `identityFiles` in
config.

**Teleport: `tsh login` fails / browser does not open (Mode A).** Make sure
`tsh` is configured and the proxy is reachable. For browser SSO, the agent
process must inherit your display environment (`$DISPLAY` on Linux, Quartz on
macOS). If the browser does not open automatically, run
`tsh login --proxy=<proxy>` in a terminal first. For clusters that enforce
terminal MFA, always pre-authenticate manually before starting the agent.

**Teleport: sessions not appearing in the audit log (Mode A).** The `-tt` PTY
flag is always set by myhostmcp; check that the Teleport node's session
recording mode is `node` or `proxy` in the cluster config.

**"unsupported remote platform ... no prebuilt binary" (Mode A).** The
embedded set doesn't include your remote's OS/arch. Run `./build.sh` (not
`current`) to embed all targets, or tell the build script to add your target.
Check the remote with `ssh HOST 'uname -sm'` and confirm that `GOOS-GOARCH`
appears in `build.sh`'s `TARGETS`.

**Stale remote binary / version mismatch (Mode A).** On each connect, the
local half checks the installed remote's version; if it differs from the
local build it re-uploads. So after rebuilding, just reconnect (or restart
the agent session). To force a clean re-install, remove
`~/.myhostmcp/myhostmcp` on the remote.

**401/403 from the HTTP endpoint (Mode B).** Check that the `Authorization`
header value matches an entry in `/etc/myhostmcp/http-auth.yaml` on the
target host exactly (`Basic base64(username:token)` or `Bearer <token>`), and
that the file is readable by the `serve-http` process but not
world-accessible (startup fails otherwise). Check the client's env var is
actually set in the process Claude Code/OpenCode was launched from — a
missing var shows as an unexpanded `${VAR}` / `{env:VAR}` literal, which will
not authenticate.

**Allowlist rejected my command (either mode).** The allowlist is enforced
**on the target host** (`/etc/myhostmcp/config.yaml`; default `df, ss, top,
free, ls`). Every piped/sequenced segment must match an entry, and
redirection/command substitution are blocked entirely. To see what's
allowed, call `remote_allowed_commands`. To allow more, edit
`/etc/myhostmcp/config.yaml` on the target host — no restart needed for
either mode, just reconnect (Mode A) or make the next call (Mode B). See
`config.remote.example.yaml`.

**Where are the logs.** Mode A: the local half writes diagnostics to
`logFile` (default: stderr, which the agent may capture); the remote half
logs to its own stderr, forwarded into the local log prefixed with
`[session <name> remote]`. Mode B: `serve-http` logs to its own stderr
(captured by systemd/journald if run as a service). On the target host,
`~/.myhostmcp/` holds the uploaded binary in Mode A (no logs are kept there).

**`remote_exec` says "pty mode is not implemented".** PTY support is planned
for Phase 2. For now, commands run over pipes (clean output, no ANSI
escapes).

## Updating

```sh
git pull
./build.sh
```

**Mode A:** restart your agent (or, for pi, `/reload`; for Claude Code,
re-run the session or `/mcp` to reconnect). The remote half auto-updates on
the next `remote_connect` because of the version check.

**Mode B:** rebuild and redeploy the binary on the target host, then restart
the `serve-http` process (`sudo systemctl restart myhostmcp-serve-http`), or
`git pull && ./build.sh` on the host itself if you build there directly.

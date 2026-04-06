<p align="center">
  <img src="logo.png" alt="DrillBit logo" width="200">
</p>

# DrillBit

Terminal UI for managing SSH tunnels to PostgreSQL databases across Docker environments.

DrillBit connects to your SSH hosts, discovers running PostgreSQL containers (including PostGIS and TimescaleDB), and lets you open tunnels to them with a single keypress. It assigns deterministic local ports so your connection strings stay stable across restarts.

## Features

- **Auto-discovery** of PostgreSQL, PostGIS, and TimescaleDB containers via Docker
- **SSH tunnel management** with connection pooling and automatic reconnection
- **Deterministic ports** — same host/container always maps to the same local port
- **Credential overrides** — set user/password/database per container and persist to config
- **Autoconnect** — mark databases to connect on startup
- **SQL client integration** — launch `pgcli` or `psql` directly from the UI
- **Clipboard support** — copy passwords or connection strings (auto-clears after 30s)
- **Self-update** with Sigstore signature verification
- **Fuzzy filtering** to quickly find databases across many hosts

## Requirements

- SSH access to remote hosts (key-based auth via ssh-agent or key files)
- Docker running on the remote hosts
- Docker access on the remote hosts (tries user permissions first, falls back to `sudo`)
- Containers must have `POSTGRES_PASSWORD` set as an environment variable

## Installation

### Homebrew (macOS/Linux)

```bash
brew install jclement/tap/drillbit
```

### Pre-built Binaries

Download the latest release for your platform from the [releases page](https://github.com/jclement/drillbit/releases).

### Docker

```bash
docker run --rm -it \
  -v ./config.yaml:/config.yaml \
  -v ~/.ssh:/root/.ssh:ro \
  ghcr.io/jclement/drillbit -c /config.yaml
```

## Configuration

Create a `config.yaml` (or run `drillbit` once to scaffold one at `~/.config/drillbit/config.yaml`):

```yaml
hosts:
  - name: prod-server-1
    user: deploy
    env: prod                              # environment label (optional)
    databases:
      - container: myapp_db_1
        auto: true                         # connect on startup
      - container: otherapp_db_1
        auto: false
        password: custom-override-password # optional override
        user: myuser                       # optional override
        database: mydb                     # optional override

  - name: prod-server-2
    env: prod
    # No databases listed — will discover all Postgres containers on this host

  - name: test-server-1
    env: test
    databases:
      - container: testapp_db_1
        auto: true
```

See `config.yaml.example` for a complete example.

### How discovery works

For each configured host, DrillBit:

1. Opens an SSH connection (respects `~/.ssh/config` for HostName, User, Port, IdentityFile)
2. Runs `docker ps` to find containers with `postgres`, `postgis`, or `timescale` images (falls back to `sudo docker` if needed)
3. Runs `docker inspect` to extract `POSTGRES_USER`, `POSTGRES_PASSWORD`, and `POSTGRES_DB`
4. Only shows containers that have `POSTGRES_PASSWORD` set

## CLI Flags

```
drillbit [options]

Options:
  -c, --config <path>   Config file (default: ~/.config/drillbit/config.yaml)
  -e, --edit            Open config in $EDITOR
  -v, --version         Show version
  -h, --help            Show this help
```

Flags can be combined: `drillbit -c /path/to/config.yaml -e` opens a custom config in your editor.

## Keybindings

| Key | Action |
|-----|--------|
| `Space` | Toggle connect / disconnect |
| `Enter` | Connect / launch SQL client (pgcli/psql) |
| `c` | Configure overrides (user/password/db) |
| `a` | Toggle autoconnect |
| `y` | Copy menu (then `p` for password, `c` for connection string) |
| `/` | Filter entries (fuzzy search) |
| `Esc` | Clear filter / quit (confirms if connections active) |
| `r` / `Ctrl+R` | Refresh — re-discover all hosts |
| `j` / `k` / `Up` / `Down` | Navigate |
| `?` | Toggle help |
| `u` | Update to latest version (when available) |

---

*Vibe coded with Claude*

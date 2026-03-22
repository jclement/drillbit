# DrillBit

Terminal UI for managing SSH tunnels to PostgreSQL databases across Docker environments.

## Installation

### Homebrew (macOS/Linux)

```bash
brew tap jclement/tap
brew install drillbit
```

### Binary Releases

Download the latest release from the [releases page](https://github.com/jclement/drillbit/releases).

### Go Install

```bash
go install github.com/jclement/drillbit@latest
```

### Docker

```bash
docker run --rm -it \
  -v ./config.yaml:/config.yaml \
  -v ~/.ssh:/root/.ssh:ro \
  ghcr.io/jclement/drillbit -c /config.yaml
```

## Usage

```bash
drillbit [options]
```

### Options

- `-c, --config <path>` - Config file path (default: `~/.config/drillbit/config.yaml`)
- `-e, --edit` - Open config in `$EDITOR` (scaffolds if doesn't exist)
- `-v, --version` - Show version information
- `--help` - Show help message

## Configuration

Create a `config.yaml` (or run `drillbit` once to scaffold one at `~/.config/drillbit/config.yaml`):

```yaml
hosts:
  - name: prod-server-1
    user: deploy
    databases:
      - container: myapp_db_1
        auto: true
      - container: otherapp_db_1
        auto: false
        password: custom-override-password  # optional override

  - name: prod-server-2
    # No databases configured - will discover all on this host

  - name: test-server-1
    databases:
      - container: testapp_db_1
        auto: true
```

The `auto: true` flag marks databases to connect automatically on startup. You can override user, password, and database name per container if needed.

## Keybindings

| Key | Action |
|-----|--------|
| `Space` | Toggle connect / disconnect |
| `Enter` | Connect / open pgcli or psql (requires user, pw, db configured) |
| `c` | Configure selected database (user, password, database, auto) |
| `y` then `p` | Copy password to clipboard |
| `y` then `c` | Copy connection string to clipboard |
| `/` | Start filtering |
| `Esc` | Clear filter / quit (confirms if connections active) |
| `r` | Refresh — re-discover all hosts |
| `h` | Manage hosts configuration |
| `j` / `k` / `↓` / `↑` | Navigate up / down |
| `?` | Toggle help |

---

*Vibe coded with Claude*

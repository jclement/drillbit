# DrillBit

Terminal UI for managing SSH tunnels to PostgreSQL databases across Docker environments.

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
        auto: true
      - container: otherapp_db_1
        auto: false
        password: custom-override-password  # optional override

  - name: prod-server-2
    env: prod
    # No databases configured - will discover all on this host

  - name: test-server-1
    env: test
    databases:
      - container: testapp_db_1
        auto: true
```

See `config.yaml.example` for a complete example.

## CLI Flags

```
drillbit [options]

Options:
  -c, --config <path>   Config file (default: ~/.config/drillbit/config.yaml)
  -e, --edit            Open config in $EDITOR
  -v, --version         Show version
      --help            Show this help
```

## Keybindings

| Key | Action |
|-----|--------|
| `Space` / `Enter` | Toggle connect / launch SQL client |
| `c` | Configure overrides (user/password/db) |
| `a` | Toggle autoconnect |
| `y` | Copy menu (password/connection string) |
| `/` | Filter entries |
| `Esc` | Clear filter / quit |
| `r` / `Ctrl+R` | Refresh hosts |
| `j` / `k` / `↑` / `↓` | Navigate |
| `?` | Toggle help |
| `u` | Update (when available) |

---

*Vibe coded with Claude*

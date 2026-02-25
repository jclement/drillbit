# DrillBit

Terminal UI for managing SSH tunnels to PostgreSQL databases across Docker environments.

## Docker

```bash
docker run --rm -it \
  -v ./config.json:/config.json \
  -v ~/.ssh:/root/.ssh:ro \
  ghcr.io/jclement/drillbit -c /config.json
```

## Configuration

Create a `config.json` (or run `drillbit` once to scaffold one at `~/.config/drillbit/config.json`):

```json
{
  "environments": {
    "prod": [
      {"host": "prod-server-1", "user": "deploy", "root": "/docker"},
      "prod-server-2"
    ],
    "test": [
      "test-server-1"
    ]
  },
  "autoconnect": [
    ["test-server-1", "my-app"]
  ]
}
```

Hosts can be simple strings (using defaults) or objects with `host`, `user`, and `root` fields. The `root` field specifies where Docker Compose projects live on the remote host (default: `/docker`).

## Keybindings

| Key | Action |
|-----|--------|
| `c` / `Enter` | Toggle connect / disconnect |
| `d` | Disconnect selected tunnel |
| `x` | Open pgcli (if connected) |
| `p` | Copy password to clipboard |
| `C` | Copy connection string to clipboard |
| `/` | Start filtering |
| `Esc` | Clear filter / close help |
| `r` | Refresh hosts |
| `j` / `k` | Navigate up / down |
| `h` / `?` | Toggle help |
| `q` | Quit |

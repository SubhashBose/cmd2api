# cmd2api

A lightweight HTTP API server that exposes command-line programs as REST endpoints.
Query parameters are mapped to command-line arguments with full control over
flag prefixes, value joiners, positional arguments, and API key authentication.

---

## Build

```bash
# Requires Go 1.22+
GOFLAGS="-mod=vendor" go build -o cmd2api .
```

---

## Usage

```
cmd2api [flags]

  -config  string   Path to config.yml (default: config.yml next to binary)
  -listen  string   IP address or interface name to listen on
                    Examples: 192.168.1.10, lo, eth0, docker0
                    Empty = all interfaces (default)
  -port    int      Port to listen on (default 11555)
```

Command-line flags take precedence over values in `config.yml`.

---

## config.yml reference

```yaml
global:
  listen: ""                    # IP or interface; empty = all interfaces
  port: 11555                   # default port
  global_api_keys:              # keys valid for every route
    - supersecretkey

routes:
  myroute:                      # served at GET /myroute
    command: mycmd --preset a   # binary + hard-coded args
    allowed_args: "w x y z"      # space-separated list of accepted query params
    arg_prefix: "--"            # flag prefix (default "--", use "-" for single-dash)
    arg_value_joiner: " "       # joins flag+value (default space, use "=" for --k=v)
    positional_args: "w x"     # these args become positional values (no prefix)
    flag_args: "y"             # these are boolean flags with NO value
    append_args: "--extra 'v'"  # always appended after API-built args
    api_keys: "key1 key2"      # extra keys only for this route
  # can add more routes
```

### Argument construction

Given `allowed_args: "a b c on"`, `positional_args: "a"`, `flag_args: "on"`, `arg_prefix: "--"`,
`arg_value_joiner: "="`, and a request `GET /route?a=1&b=2&c=3&on`:

```
mycmd --preset X  1  --b=2  --c=3 --on  [append_args...]
                  ^positional  ^flags with joiner
```

---

## Authentication

API keys are checked via (in order):
1. `X-API-Key: <key>` request header
2. `Authorization: Bearer <key>` query parameter

A request is authorised if its key appears in `global.global_api_keys` **or**
the route's `api_keys` list. If neither list has any entries, the route is
**open** (no auth required).

---

## API response format

All routes return JSON:

```json
{
  "success": true,
  "stdout": "command output here\n",
  "stderr": "",
  "exit_code": 0
}
```

On error:
```json
{
  "success": false,
  "error": "description",
  "exit_code": 1
}
```

### Health check

```
GET /healthz  →  {"status":"ok"}
```

---

## Example

```bash
# Start
./cmd2api --config config.yml --port 8080

# Call /ls with a positional path arg, auth via header
curl -H "X-API-Key: supersecretkey" "http://localhost:8080/ls?path=/tmp"

# Call /date with a custom format
curl "http://localhost:8080/date?format=%25Y-%25m-%25d&api_key=supersecretkey"
```

# cmd2api

A lightweight HTTP API server that exposes command-line programs as REST endpoints.
Query parameters are mapped to command-line arguments with full control over allowed flags,
flag prefixes, value joiners, positional arguments, boolean flags, execution timeout, and API key authentication.
The server also supports gzip and brotil compression when supported by client.

The granular control over the allowed flags and its types make the rest api robust and secure.

---

## Download and Update

In the [release section](https://github.com/SubhashBose/cmd2api/releases) binaries are available in 15 OS and architecture combinations.

To update the binary to the latest release, simply run

```bash
cmd2api -upgrade
```

## Build

To compile the binary instead of downloading.
```bash
# Requires Go 1.22+
GOFLAGS="-mod=vendor" CGO_ENABLED=0 go build --ldflags '-w -s -buildid=' -o cmd2api .
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
  -upgrade          Self update to latest version 
```

Command-line flags take precedence over values in `config.yml`.

---

## config.yml reference

```yaml
global:
  listen: ""                    # (optional) IP or interface; empty = all interfaces
  port: 11555                   # (optional) default port
  global_api_keys:              # keys valid for every route
    - supersecretkey

routes:
  myroute:                      # served at GET /myroute
    command: mycmd --preset a   # binary + hard-coded args
    allowed_args: "w x y z"     # space-separated list of accepted query params
    # All below are optional
    arg_prefix: "--"            # flag prefix (default "--", use "-" for single-dash)
    arg_value_joiner: " "       # joins flag+value (default space, use "=" for --k=v)
    positional_args: "w x"      # these args become positional values (no prefix)
    flag_args: "y"              # these are boolean flags with NO value
    append_args: "--extra 'v'"  # always appended after API-built args
    cmd_workdir: /home/user     # working directory for the command
    exec_timeout: 2s            # timeout duration for command execution
    api_keys: "key1 key2"       # extra keys only for this route
  
  # can add more routes
```
Read the included config-example.yml for examples and more detail configuration options for routes. 

### Argument construction

Given `allowed_args: "a b c y"`, `positional_args: "a"`, `flag_args: "y"`, `arg_prefix: "--"`,
`arg_value_joiner: "="`, and a request `GET /route?a=1&b=2&c=3&y=4`:

```
mycmd --preset X  1  --b=2  --c=3 --y  [append_args...]
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
curl -H "Authorization: Bearer supersecretkey" "http://localhost:8080/date?format=%25Y-%25m-%25d"
```

---

## Security recommendations
The API server is secure enough against most common malformed argument injection at command execution level. However, it ultimately depends on the command-line program how it process the arguments and exposes risk to host system.

The best practice would be to run the API server and command-line program within a docker container, having an isolated filesystem from host, and also to make the filesystem read-only if that permits the use case. Here is a minimal example:

```bash
docker run -it --rm -v ./cmd2api_dir:/cmd2api:ro -p 11555:11555 alpine /cmd2api/cmd2api

#OR as a always on background process
docker run -itd --restart unless-stopped -v ./cmd2api_dir:/cmd2api:ro -p 11555:11555 alpine /cmd2api/cmd2api

```
package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/andybalholm/brotli"
	"gopkg.in/yaml.v3"

	selfupdate "github.com/SubhashBose/GoPkg-selfupdater"
)

var version = "1.0"

// ─────────────────────────────────────────────
// Config structures
// ─────────────────────────────────────────────

type GlobalConfig struct {
	Listen        string   `yaml:"listen"`
	Port          int      `yaml:"port"`
	GlobalAPIKeys []string `yaml:"global_api_keys"`
}

type RouteConfig struct {
	Command        string        `yaml:"command"`
	AllowedArgs    []string      `yaml:"allowed_args"`
	ArgPrefix      string        `yaml:"arg_prefix"`
	ArgValueJoiner string        `yaml:"arg_value_joiner"`
	PositionalArgs []string      `yaml:"positional_args"`
	FlagArgs       []string      `yaml:"flag_args"`
	AppendArgs     string        `yaml:"append_args"`
	CmdWorkdir     string        `yaml:"cmd_workdir"`
	ExecTimeout    time.Duration `yaml:"exec_timeout"`
	CORSOrigins    []string      `yaml:"cors_origins"`
	APIKeys        []string      `yaml:"api_keys"`
}

// UnmarshalYAML handles space-separated string lists for AllowedArgs,
// PositionalArgs, FlagArgs, CORSOrigins, and APIKeys fields.
func (r *RouteConfig) UnmarshalYAML(value *yaml.Node) error {
	type rawRoute struct {
		Command        string `yaml:"command"`
		AllowedArgs    string `yaml:"allowed_args"`
		ArgPrefix      string `yaml:"arg_prefix"`
		ArgValueJoiner string `yaml:"arg_value_joiner"`
		PositionalArgs string `yaml:"positional_args"`
		FlagArgs       string `yaml:"flag_args"`
		AppendArgs     string `yaml:"append_args"`
		CmdWorkdir     string `yaml:"cmd_workdir"`
		ExecTimeout    string `yaml:"exec_timeout"`
		CORSOrigins    string `yaml:"cors_origins"`
		APIKeys        string `yaml:"api_keys"`
	}
	var raw rawRoute
	if err := value.Decode(&raw); err != nil {
		return err
	}
	r.Command = raw.Command
	r.AllowedArgs = splitFields(raw.AllowedArgs)
	r.ArgPrefix = raw.ArgPrefix
	r.ArgValueJoiner = raw.ArgValueJoiner
	r.PositionalArgs = splitFields(raw.PositionalArgs)
	r.FlagArgs = splitFields(raw.FlagArgs)
	r.AppendArgs = raw.AppendArgs
	r.CmdWorkdir = raw.CmdWorkdir
	if raw.ExecTimeout != "" {
		d, err := time.ParseDuration(raw.ExecTimeout)
		if err != nil {
			return fmt.Errorf("invalid exec_timeout %q: %w", raw.ExecTimeout, err)
		}
		r.ExecTimeout = d
	}
	r.CORSOrigins = splitFields(raw.CORSOrigins)
	r.APIKeys = splitFields(raw.APIKeys)
	return nil
}

type Config struct {
	Global GlobalConfig           `yaml:"global"`
	Routes map[string]RouteConfig `yaml:"routes"`
}

// ─────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────

func splitFields(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Fields(s)
}

// resolveListenAddr resolves an interface name (e.g. "eth0", "lo") to its
// first IPv4 address. If addr is already an IP it is returned unchanged.
// Empty string means listen on all interfaces ("").
func resolveListenAddr(addr string) (string, error) {
	if addr == "" {
		return "", nil
	}
	// Check if it's already an IP address
	if ip := net.ParseIP(addr); ip != nil {
		return addr, nil
	}
	// Treat as interface name
	iface, err := net.InterfaceByName(addr)
	if err != nil {
		return "", fmt.Errorf("interface %q not found: %w", addr, err)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return "", fmt.Errorf("cannot get addresses for interface %q: %w", addr, err)
	}
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil || ip.IsLoopback() && addr != "lo" {
			continue
		}
		if ip4 := ip.To4(); ip4 != nil {
			return ip4.String(), nil
		}
	}
	return "", fmt.Errorf("no IPv4 address found on interface %q", addr)
}

// buildArgs constructs the final argument slice for the command.
func buildArgs(route RouteConfig, queryParams map[string]string) ([]string, error) {
// Determine prefix and joiner
	prefix := "--"
	if route.ArgPrefix != "" {
		prefix = route.ArgPrefix
	}
	joiner := " " // sentinel; we handle space specially
	if route.ArgValueJoiner != "" {
		joiner = route.ArgValueJoiner
	}

	// Index positional args for quick lookup
	positionalSet := make(map[string]bool, len(route.PositionalArgs))
	for _, p := range route.PositionalArgs {
		positionalSet[p] = true
	}

	// Index flag-only args (no value, just the flag itself)
	flagArgSet := make(map[string]bool, len(route.FlagArgs))
	for _, f := range route.FlagArgs {
		flagArgSet[f] = true
	}

	// Allowed args set
	allowedSet := make(map[string]bool, len(route.AllowedArgs))
	for _, a := range route.AllowedArgs {
		allowedSet[a] = true
	}

	var args []string

	for _, argName := range route.AllowedArgs {
		_, present := queryParams[argName]
		if !present {
			continue
		}
		switch {
		case flagArgSet[argName]:
			// Flag-only: emit just the prefix+name, ignore any query value
			args = append(args, prefix+argName)
		case positionalSet[argName]:
			args = append(args, queryParams[argName])
		default:
			val := queryParams[argName]
			if joiner == " " {
				args = append(args, prefix+argName, val)
			} else {
				args = append(args, prefix+argName+joiner+val)
			}
		}
	}

	// Append extra args (shell-split the append_args string)
	if route.AppendArgs != "" {
		extra := shellSplit(route.AppendArgs)
		args = append(args, extra...)
	}

	return args, nil
}

// shellSplit performs a very simple whitespace split that respects single and
// double quoted strings (no escape sequences).
func shellSplit(s string) []string {
	var tokens []string
	var cur strings.Builder
	inSingle := false
	inDouble := false

	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case (c == ' ' || c == '\t') && !inSingle && !inDouble:
			if cur.Len() > 0 {
				tokens = append(tokens, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		tokens = append(tokens, cur.String())
	}
	return tokens
}

// ─────────────────────────────────────────────
// Compression middleware
// ─────────────────────────────────────────────

// compressedResponseWriter wraps http.ResponseWriter and writes through an
// underlying compressor. WriteHeader is intercepted to inject the encoding
// headers before the status is sent.
type compressedResponseWriter struct {
	http.ResponseWriter
	writer      interface{ Write([]byte) (int, error) }
	wroteHeader bool
}

func (c *compressedResponseWriter) Write(b []byte) (int, error) {
	return c.writer.Write(b)
}

func (c *compressedResponseWriter) WriteHeader(status int) {
	c.wroteHeader = true
	c.ResponseWriter.WriteHeader(status)
}

// acceptedEncodings parses the Accept-Encoding header and returns the best
// supported encoding to use ("br", "gzip", or "" for none).
// Brotli and gzip are the only encodings we support. The highest q-value wins;
// ties are broken by preferring brotli over gzip.
func bestEncoding(r *http.Request) string {
	type entry struct {
		enc string
		q   float64
	}
	var supported []entry

	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		tokens := strings.SplitN(part, ";", 2)
		enc := strings.ToLower(strings.TrimSpace(tokens[0]))
		if enc != "br" && enc != "gzip" {
			continue
		}
		q := 1.0
		if len(tokens) == 2 {
			param := strings.TrimSpace(tokens[1])
			if strings.HasPrefix(param, "q=") {
				fmt.Sscanf(param[2:], "%f", &q)
			}
		}
		if q > 0 { // q=0 means "not acceptable"
			supported = append(supported, entry{enc, q})
		}
	}

	best := ""
	bestQ := -1.0
	for _, e := range supported {
// Prefer br over gzip on equal q
		if e.q > bestQ || (e.q == bestQ && e.enc == "br") {
			best = e.enc
			bestQ = e.q
		}
	}
	return best
}

// compressHandler wraps a handler and compresses responses when the client
// supports brotli or gzip. Brotli is preferred when both are accepted.
func compressHandler(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch bestEncoding(r) {
		case "br":
			w.Header().Set("Content-Encoding", "br")
			w.Header().Del("Content-Length")
			bw := brotli.NewWriter(w)
			defer bw.Close()
			next(&compressedResponseWriter{ResponseWriter: w, writer: bw}, r)

		case "gzip":
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Del("Content-Length")
			gw, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
			if err != nil {
				next(w, r)
				return
			}
			defer gw.Close()
			next(&compressedResponseWriter{ResponseWriter: w, writer: gw}, r)

		default:
			next(w, r)
		}
	}
}

// ─────────────────────────────────────────────
// CORS middleware
// ─────────────────────────────────────────────

// corsHandler adds CORS headers based on the route's cors_origins list.
// If the list is empty, no CORS headers are added.
// If it contains "*", all origins are allowed unconditionally.
// Otherwise the request Origin is matched against the list; if matched,
// that origin is echoed back (required for credentialed requests).
// Preflight OPTIONS requests are answered immediately with 204 No Content,
// bypassing auth and command execution entirely.
func corsHandler(origins []string, next http.HandlerFunc) http.HandlerFunc {
	if len(origins) == 0 {
		return next
	}

	wildcard := false
	originSet := make(map[string]bool, len(origins))
	for _, o := range origins {
		if o == "*" {
			wildcard = true
		} else {
			originSet[strings.ToLower(o)] = true
		}
	}

	return func(w http.ResponseWriter, r *http.Request) {
		requestOrigin := r.Header.Get("Origin")

		var allowOrigin string
		if requestOrigin != "" {
			if wildcard {
				allowOrigin = "*"
			} else if originSet[strings.ToLower(requestOrigin)] {
				allowOrigin = requestOrigin // echo back matched origin
			}
		}

		if allowOrigin != "" {
			w.Header().Set("Access-Control-Allow-Origin", allowOrigin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key, Authorization")
			// Vary: Origin required when reflecting a specific origin so caches
			// don't serve the wrong origin's response
			if allowOrigin != "*" {
				w.Header().Add("Vary", "Origin")
			}
		}

		// Answer preflight immediately
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next(w, r)
	}
}

// ─────────────────────────────────────────────
// Cloudflare-safe status codes
// ─────────────────────────────────────────────

// cfSafeStatus returns the HTTP status code to use for the response.
// Cloudflare intercepts 5xx responses and replaces the body with its own
// error page, so the JSON error payload never reaches the client.
// When the request came through Cloudflare (detected by the CF-Ray header),
// any 5xx status is downgraded to 200 so the JSON body is passed through
// intact. The "success": false field in the body still signals the error.
func cfSafeStatus(r *http.Request, status int) int {
	if status >= 500 && r.Header.Get("CF-Ray") != "" {
		return http.StatusOK
	}
	return status
}

// ─────────────────────────────────────────────
// API response helpers
// ─────────────────────────────────────────────

type APIResponse struct {
	Success  bool   `json:"success"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ─────────────────────────────────────────────
// HTTP handler factory
// ─────────────────────────────────────────────

func makeHandler(routeName string, route RouteConfig, globalKeys []string) http.HandlerFunc {
// Build combined key set
	keySet := make(map[string]bool)
	for _, k := range globalKeys {
		if k != "" {
			keySet[k] = true
		}
	}
	for _, k := range route.APIKeys {
		if k != "" {
			keySet[k] = true
		}
	}
	requireAuth := len(keySet) > 0

	return func(w http.ResponseWriter, r *http.Request) {
		// ── Auth ──────────────────────────────────────────
		if requireAuth {
			apiKey := r.Header.Get("X-API-Key")
			if apiKey == "" {
				// Authorization: Bearer <token>
				if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
					apiKey = strings.TrimPrefix(auth, "Bearer ")
				}
			}
			if !keySet[apiKey] {
				writeJSON(w, http.StatusUnauthorized, APIResponse{
					Success: false,
					Error:   "unauthorized: invalid or missing API key",
				})
				return
			}
		}

		// ── Method guard ──────────────────────────────────
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, APIResponse{
				Success: false,
				Error:   "method not allowed: use GET or POST",
			})
			return
		}

		// ── Collect params: URL query + POST body ─────────
		// URL query params are always read first.
		queryParams := make(map[string]string)
		for k, vals := range r.URL.Query() {
			if len(vals) > 0 {
				queryParams[k] = vals[0]
			}
		}

		// For POST, merge body params (JSON object or form-encoded).
		// Body values take precedence over URL query values.
		if r.Method == http.MethodPost {
			ct := strings.ToLower(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0])
			ct = strings.TrimSpace(ct)
			switch ct {
			case "application/json":
				var body map[string]interface{}
				if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
					for k, v := range body {
						queryParams[k] = fmt.Sprintf("%v", v)
					}
				}
			default:
				// application/x-www-form-urlencoded or multipart/form-data
				if err := r.ParseForm(); err == nil {
					for k, vals := range r.PostForm {
						if len(vals) > 0 {
							queryParams[k] = vals[0]
						}
					}
				}
			}
		}

		// ── Build argument list ───────────────────────────
		extraArgs, err := buildArgs(route, queryParams)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, APIResponse{
				Success: false,
				Error:   fmt.Sprintf("argument error: %v", err),
			})
			return
		}

		// ── Parse base command + preset args ──────────────
		baseTokens := shellSplit(route.Command)
		if len(baseTokens) == 0 {
			writeJSON(w, cfSafeStatus(r, http.StatusInternalServerError), APIResponse{
				Success: false,
				Error:   "route has empty command",
			})
			return
		}
		cmdName := baseTokens[0]
		cmdArgs := append(baseTokens[1:], extraArgs...)

		log.Printf("[%s] executing: %s %s", routeName, cmdName, strings.Join(cmdArgs, " "))

		// ── Execute ───────────────────────────────────────
		timeout := 60 * time.Second
		if route.ExecTimeout > 0 {
			timeout = route.ExecTimeout
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()

		cmd := exec.CommandContext(ctx, cmdName, cmdArgs...)
		if route.CmdWorkdir != "" {
			cmd.Dir = route.CmdWorkdir
		}
		var stdoutBuf, stderrBuf strings.Builder
		cmd.Stdout = &stdoutBuf
		cmd.Stderr = &stderrBuf

		runErr := cmd.Run()

		exitCode := 0
		if runErr != nil {
			if ctx.Err() == context.DeadlineExceeded {
				writeJSON(w, cfSafeStatus(r, http.StatusGatewayTimeout), APIResponse{
					Success: false,
					Error:   fmt.Sprintf("command timed out after %s", timeout),
					Stderr:  stderrBuf.String(),
				})
				return
			}
			if exitErr, ok := runErr.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				writeJSON(w, cfSafeStatus(r, http.StatusInternalServerError), APIResponse{
					Success: false,
					Error:   fmt.Sprintf("execution error: %v", runErr),
					Stderr:  stderrBuf.String(),
				})
				return
			}
		}

		writeJSON(w, http.StatusOK, APIResponse{
			Success:  exitCode == 0,
			Stdout:   stdoutBuf.String(),
			Stderr:   stderrBuf.String(),
			ExitCode: exitCode,
		})
	}
}

// ─────────────────────────────────────────────
// Self-update
// ─────────────────────────────────────────────

func runUpgrade() {
	cfg := selfupdate.Config{
		RepoURL:        "https://github.com/SubhashBose/cmd2api",
		BinaryPrefix:   "cmd2api-",
		OSSep:          "-",
		CurrentVersion: version, // build-time var
	}

	fmt.Printf("Current version: %s\nChecking for updates...", version)

	res, err := selfupdate.Update(cfg)

	if res.LatestVersion != "" {
		fmt.Printf(" Latest version: %s\n", res.LatestVersion)
	} else {
		fmt.Printf("\n")
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "Update failed:", err)
		os.Exit(1)
	}

	if !res.Updated {
		fmt.Printf("Already up to date (latest: %s)\n", res.LatestVersion)
		return
	}
	fmt.Printf("Successfully updated to v%s (asset: %s)\n", res.LatestVersion, res.AssetName)
}

// ─────────────────────────────────────────────
// main
// ─────────────────────────────────────────────

func main() {
	// ── Flags ─────────────────────────────────────────────
	exePath, _ := os.Executable()
	defaultConfig := filepath.Join(filepath.Dir(exePath), "config.yml")

	configFile := flag.String("config", defaultConfig, "path to config.yml")
	listenFlag := flag.String("listen", "", "IP address or interface name to listen on (empty = all)")
	portFlag := flag.Int("port", 0, "port to listen on (default 11555)")
	upgrade := flag.Bool("upgrade", false, "update cmd2api to latest version")
	flag.Parse()

	if upgrade != nil && *upgrade {
		runUpgrade()
		os.Exit(0)
	}

	data, err := os.ReadFile(*configFile)
	if err != nil {
		log.Fatalf("cannot read config file %q: %v", *configFile, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("cannot parse config file: %v", err)
	}

	// ── Resolve listen address (CLI > config > default) ───
	listenAddr := cfg.Global.Listen
	if *listenFlag != "" {
		listenAddr = *listenFlag
	}
	resolvedIP, err := resolveListenAddr(listenAddr)
	if err != nil {
		log.Fatalf("cannot resolve listen address %q: %v", listenAddr, err)
	}

	// ── Resolve port (CLI > config > 11555) ──────────────
	port := 11555
	if cfg.Global.Port != 0 {
		port = cfg.Global.Port
	}
	if *portFlag != 0 {
		port = *portFlag
	}

	bindAddr := fmt.Sprintf("%s:%d", resolvedIP, port)

	// ── Register routes ───────────────────────────────────
	mux := http.NewServeMux()

	// Health check
	mux.HandleFunc("/healthz", compressHandler(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}))

	if len(cfg.Routes) == 0 {
		log.Println("warning: no routes defined in config")
	}

	for name, route := range cfg.Routes {
		path := "/" + name
		log.Printf("registering route  %-20s  cmd: %s", path, route.Command)
		mux.HandleFunc(path, compressHandler(corsHandler(route.CORSOrigins, makeHandler(name, route, cfg.Global.GlobalAPIKeys))))
	}

	// ── Start server ──────────────────────────────────────
	srv := &http.Server{
		Addr:         bindAddr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 90 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown on SIGINT/SIGTERM
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("cmd2api listening on %s", bindAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-quit
	log.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("forced shutdown: %v", err)
	}
	log.Println("stopped")
}
// auron-api is a thin OpenAPI-aware client for the Auron API.
//
//	auron-api sync                    — fetch openapi.json and rebuild the wiki
//	auron-api search <query>          — find operations matching the query
//	auron-api call <METHOD> <path>    — perform an authenticated API call
//	auron-api list                    — list available wiki sections
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	defaultOpenAPIURL = "https://dev.useauron.ai/api/v1/openapi.json"
	defaultConfigPath = ".auron/config.json"
	defaultStateDir   = ".auron"
	wikiDirName       = "api-wiki"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	var err error
	switch cmd {
	case "sync":
		err = cmdSync(args)
	case "search":
		err = cmdSearch(args)
	case "call":
		err = cmdCall(args)
	case "list":
		err = cmdList(args)
	case "set-token":
		err = cmdSetToken(args)
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "auron-api: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "auron-api: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `auron-api — Auron OpenAPI client

Commands:
  sync                            Fetch openapi.json and rebuild ~/.auron/api-wiki/
  search <query> [--limit N]      Search operations by tag/path/summary/operationId
  list                            List wiki sections (tags)
  set-token                       Read a bearer token from stdin and cache it in ~/.auron/config.json
  call <METHOD> <path> [flags]    Make an authenticated API call
        --query k=v[,k=v...]      Query string params
        --header k=v[,k=v...]     Extra request headers
        --body @file.json | -     Request body from file or stdin
        --raw                     Print raw response (don't pretty-print JSON)

Env:
  AURON_OPENAPI_URL   override discovery URL (default ` + defaultOpenAPIURL + `)
  AURON_CONFIG        path to token config (default ~/.auron/config.json)
  AURON_STATE_DIR     where openapi.json + wiki live (default ~/.auron)`)
}

// ---------------------------------------------------------------------------
// sync
// ---------------------------------------------------------------------------

func cmdSync(args []string) error {
	openAPIURL := envOr("AURON_OPENAPI_URL", defaultOpenAPIURL)
	for i := 0; i < len(args); i++ {
		if args[i] == "--url" && i+1 < len(args) {
			openAPIURL = args[i+1]
			i++
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fmt.Printf("Fetching %s\n", openAPIURL)
	raw, err := httpGet(ctx, openAPIURL)
	if err != nil {
		return fmt.Errorf("fetch openapi: %w", err)
	}
	var spec OpenAPI
	if err := json.Unmarshal(raw, &spec); err != nil {
		return fmt.Errorf("parse openapi: %w", err)
	}
	if len(spec.Paths) == 0 {
		return errors.New("openapi has no paths")
	}

	stateDir := stateDirPath()
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	openapiPath := filepath.Join(stateDir, "openapi.json")
	if err := atomicWrite(openapiPath, raw, 0o600); err != nil {
		return fmt.Errorf("write openapi: %w", err)
	}

	wikiDir := filepath.Join(stateDir, wikiDirName)
	if err := os.RemoveAll(wikiDir); err != nil {
		return err
	}
	if err := os.MkdirAll(wikiDir, 0o700); err != nil {
		return err
	}

	groups := groupByTag(&spec)
	for tag, ops := range groups {
		md := renderTagMarkdown(&spec, tag, ops)
		fname := slug(tag) + ".md"
		if err := atomicWrite(filepath.Join(wikiDir, fname), []byte(md), 0o600); err != nil {
			return err
		}
	}
	if err := atomicWrite(filepath.Join(wikiDir, "index.md"), []byte(renderIndex(&spec, groups)), 0o600); err != nil {
		return err
	}

	fmt.Printf("Synced %d paths across %d tags → %s\n", len(spec.Paths), len(groups), wikiDir)
	return nil
}

// ---------------------------------------------------------------------------
// search
// ---------------------------------------------------------------------------

func cmdSearch(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: auron-api search <query> [--limit N]")
	}
	limit := 15
	var queryParts []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--limit":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &limit)
				i++
			}
		default:
			queryParts = append(queryParts, args[i])
		}
	}
	query := strings.ToLower(strings.Join(queryParts, " "))

	spec, err := loadCachedSpec()
	if err != nil {
		return err
	}

	type scored struct {
		op    operation
		score int
	}
	var hits []scored
	terms := tokenize(query)
	for _, op := range flatten(spec) {
		s := scoreOp(op, terms)
		if s > 0 {
			hits = append(hits, scored{op, s})
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	if len(hits) > limit {
		hits = hits[:limit]
	}

	out := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		out = append(out, map[string]any{
			"method":      h.op.Method,
			"path":        h.op.Path,
			"operationId": h.op.OperationID,
			"summary":     h.op.Summary,
			"tags":        h.op.Tags,
			"wiki":        slug(firstTag(h.op.Tags)) + ".md",
			"score":       h.score,
		})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// ---------------------------------------------------------------------------
// list
// ---------------------------------------------------------------------------

func cmdList(args []string) error {
	_ = args
	wikiDir := filepath.Join(stateDirPath(), wikiDirName)
	entries, err := os.ReadDir(wikiDir)
	if err != nil {
		return fmt.Errorf("wiki not built — run `auron-api sync` first: %w", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			fmt.Println(e.Name())
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// call
// ---------------------------------------------------------------------------

func cmdCall(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: auron-api call <METHOD> <path> [--query k=v] [--header k=v] [--body @file|-] [--raw]")
	}
	method := strings.ToUpper(args[0])
	path := args[1]
	rest := args[2:]

	var (
		query   = url.Values{}
		headers = http.Header{}
		body    []byte
		raw     bool
	)
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--query":
			if i+1 >= len(rest) {
				return errors.New("--query needs a value")
			}
			if err := parseKV(rest[i+1], query.Add); err != nil {
				return err
			}
			i++
		case "--header":
			if i+1 >= len(rest) {
				return errors.New("--header needs a value")
			}
			if err := parseKV(rest[i+1], headers.Add); err != nil {
				return err
			}
			i++
		case "--body":
			if i+1 >= len(rest) {
				return errors.New("--body needs a value")
			}
			b, err := readBodyArg(rest[i+1])
			if err != nil {
				return err
			}
			body = b
			i++
		case "--raw":
			raw = true
		default:
			return fmt.Errorf("unknown flag %q", rest[i])
		}
	}

	cfg, err := loadTokenConfig()
	if err != nil {
		return fmt.Errorf("auth: %w (token missing — call mcp__claude_ai_auron__exchange_token and pipe into `auron-api set-token`)", err)
	}

	base, err := apiBaseURL()
	if err != nil {
		return err
	}
	fullURL := base + path
	if len(query) > 0 {
		sep := "?"
		if strings.Contains(fullURL, "?") {
			sep = "&"
		}
		fullURL += sep + query.Encode()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, fullURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if req.Header.Get("Content-Type") == "" && len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.AccessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	fmt.Fprintf(os.Stderr, "%s %s → %s\n", method, fullURL, resp.Status)

	if !raw && strings.Contains(resp.Header.Get("Content-Type"), "json") {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, respBody, "", "  "); err == nil {
			os.Stdout.Write(pretty.Bytes())
			fmt.Println()
			return exitFromStatus(resp.StatusCode)
		}
	}
	os.Stdout.Write(respBody)
	if len(respBody) > 0 && respBody[len(respBody)-1] != '\n' {
		fmt.Println()
	}
	return exitFromStatus(resp.StatusCode)
}

func exitFromStatus(code int) error {
	if code >= 400 {
		return fmt.Errorf("HTTP %d", code)
	}
	return nil
}

// ---------------------------------------------------------------------------
// OpenAPI parsing
// ---------------------------------------------------------------------------

type OpenAPI struct {
	OpenAPI string `json:"openapi"`
	Info    struct {
		Title       string `json:"title"`
		Version     string `json:"version"`
		Description string `json:"description"`
	} `json:"info"`
	Servers []struct {
		URL string `json:"url"`
	} `json:"servers"`
	Paths map[string]map[string]rawOp `json:"paths"`
	Tags  []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"tags"`
}

type rawOp struct {
	OperationID string                 `json:"operationId"`
	Summary     string                 `json:"summary"`
	Description string                 `json:"description"`
	Tags        []string               `json:"tags"`
	Parameters  []parameter            `json:"parameters"`
	RequestBody map[string]any         `json:"requestBody"`
	Responses   map[string]rawResponse `json:"responses"`
	Deprecated  bool                   `json:"deprecated"`
}

type parameter struct {
	Name        string         `json:"name"`
	In          string         `json:"in"`
	Required    bool           `json:"required"`
	Description string         `json:"description"`
	Schema      map[string]any `json:"schema"`
}

type rawResponse struct {
	Description string         `json:"description"`
	Content     map[string]any `json:"content"`
}

type operation struct {
	Method      string
	Path        string
	OperationID string
	Summary     string
	Description string
	Tags        []string
	Parameters  []parameter
	HasBody     bool
	Responses   map[string]rawResponse
	Deprecated  bool
}

var httpMethods = map[string]bool{
	"get": true, "post": true, "put": true, "patch": true, "delete": true, "head": true, "options": true,
}

func flatten(spec *OpenAPI) []operation {
	var out []operation
	for p, methods := range spec.Paths {
		for m, op := range methods {
			if !httpMethods[strings.ToLower(m)] {
				continue
			}
			out = append(out, operation{
				Method:      strings.ToUpper(m),
				Path:        p,
				OperationID: op.OperationID,
				Summary:     op.Summary,
				Description: op.Description,
				Tags:        op.Tags,
				Parameters:  op.Parameters,
				HasBody:     op.RequestBody != nil,
				Responses:   op.Responses,
				Deprecated:  op.Deprecated,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path == out[j].Path {
			return out[i].Method < out[j].Method
		}
		return out[i].Path < out[j].Path
	})
	return out
}

func groupByTag(spec *OpenAPI) map[string][]operation {
	groups := map[string][]operation{}
	for _, op := range flatten(spec) {
		tag := firstTag(op.Tags)
		groups[tag] = append(groups[tag], op)
	}
	return groups
}

func firstTag(tags []string) string {
	if len(tags) == 0 {
		return "default"
	}
	return tags[0]
}

// ---------------------------------------------------------------------------
// Wiki rendering
// ---------------------------------------------------------------------------

func renderTagMarkdown(spec *OpenAPI, tag string, ops []operation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", titleCase(tag))
	if desc := tagDescription(spec, tag); desc != "" {
		fmt.Fprintf(&b, "%s\n\n", desc)
	}
	if len(spec.Servers) > 0 {
		fmt.Fprintf(&b, "**Base URL:** `%s`\n\n", spec.Servers[0].URL)
	}
	fmt.Fprintf(&b, "**%d operations.** Use `auron-api call <METHOD> <path>` to invoke.\n\n---\n\n", len(ops))

	for _, op := range ops {
		fmt.Fprintf(&b, "## `%s %s`\n\n", op.Method, op.Path)
		if op.OperationID != "" {
			fmt.Fprintf(&b, "**operationId:** `%s`\n\n", op.OperationID)
		}
		if op.Deprecated {
			b.WriteString("> ⚠️ **Deprecated**\n\n")
		}
		if op.Summary != "" {
			fmt.Fprintf(&b, "%s\n\n", op.Summary)
		}
		if op.Description != "" && op.Description != op.Summary {
			fmt.Fprintf(&b, "%s\n\n", op.Description)
		}

		// Parameters grouped by location.
		paramGroups := map[string][]parameter{}
		for _, p := range op.Parameters {
			paramGroups[p.In] = append(paramGroups[p.In], p)
		}
		for _, in := range []string{"path", "query", "header"} {
			ps := paramGroups[in]
			if len(ps) == 0 {
				continue
			}
			fmt.Fprintf(&b, "**%s params:**\n\n", titleCase(in))
			for _, p := range ps {
				req := ""
				if p.Required {
					req = " *(required)*"
				}
				typ := schemaType(p.Schema)
				desc := strings.TrimSpace(p.Description)
				if desc != "" {
					desc = " — " + desc
				}
				fmt.Fprintf(&b, "- `%s` (%s)%s%s\n", p.Name, typ, req, desc)
			}
			b.WriteString("\n")
		}
		if op.HasBody {
			b.WriteString("**Request body:** yes (JSON). See `~/.auron/openapi.json` for schema.\n\n")
		}
		if len(op.Responses) > 0 {
			b.WriteString("**Responses:**\n\n")
			codes := make([]string, 0, len(op.Responses))
			for c := range op.Responses {
				codes = append(codes, c)
			}
			sort.Strings(codes)
			for _, c := range codes {
				r := op.Responses[c]
				fmt.Fprintf(&b, "- `%s` — %s\n", c, strings.TrimSpace(r.Description))
			}
			b.WriteString("\n")
		}
		b.WriteString("---\n\n")
	}
	return b.String()
}

func renderIndex(spec *OpenAPI, groups map[string][]operation) string {
	var b strings.Builder
	title := spec.Info.Title
	if title == "" {
		title = "Auron API"
	}
	fmt.Fprintf(&b, "# %s\n\n", title)
	if spec.Info.Version != "" {
		fmt.Fprintf(&b, "Version: `%s`\n\n", spec.Info.Version)
	}
	if len(spec.Servers) > 0 {
		fmt.Fprintf(&b, "Base URL: `%s`\n\n", spec.Servers[0].URL)
	}
	b.WriteString("## Sections\n\n")
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "- [%s](%s.md) — %d operations\n", titleCase(k), slug(k), len(groups[k]))
	}
	return b.String()
}

func tagDescription(spec *OpenAPI, tag string) string {
	for _, t := range spec.Tags {
		if t.Name == tag {
			return strings.TrimSpace(t.Description)
		}
	}
	return ""
}

func schemaType(s map[string]any) string {
	if s == nil {
		return "any"
	}
	if t, ok := s["type"].(string); ok {
		if f, ok := s["format"].(string); ok {
			return t + " (" + f + ")"
		}
		return t
	}
	if ref, ok := s["$ref"].(string); ok {
		return strings.TrimPrefix(ref, "#/components/schemas/")
	}
	return "any"
}

// ---------------------------------------------------------------------------
// Search scoring
// ---------------------------------------------------------------------------

func tokenize(q string) []string {
	q = strings.ToLower(q)
	q = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(q, " ")
	return strings.Fields(q)
}

func scoreOp(op operation, terms []string) int {
	if len(terms) == 0 {
		return 0
	}
	hay := strings.ToLower(strings.Join([]string{
		op.OperationID, op.Path, op.Summary, op.Description, strings.Join(op.Tags, " "),
	}, " "))
	score := 0
	for _, t := range terms {
		if t == "" {
			continue
		}
		switch {
		case strings.Contains(strings.ToLower(op.OperationID), t):
			score += 5
		case strings.Contains(strings.ToLower(strings.Join(op.Tags, " ")), t):
			score += 4
		case strings.Contains(strings.ToLower(op.Path), t):
			score += 3
		case strings.Contains(strings.ToLower(op.Summary), t):
			score += 3
		case strings.Contains(hay, t):
			score += 1
		}
	}
	return score
}

// ---------------------------------------------------------------------------
// Token + spec loading
// ---------------------------------------------------------------------------

type tokenConfig struct {
	AccessToken string `json:"access_token"`
}

func configPath() (string, error) {
	if p := os.Getenv("AURON_CONFIG"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, defaultConfigPath), nil
}

func loadTokenConfig() (*tokenConfig, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var c tokenConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.AccessToken == "" {
		return nil, errors.New("no access_token in config")
	}
	return &c, nil
}

func cmdSetToken(args []string) error {
	_ = args
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return errors.New("set-token: no token on stdin")
	}
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(tokenConfig{AccessToken: token}, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWrite(path, body, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote token → %s\n", path)
	return nil
}

func loadCachedSpec() (*OpenAPI, error) {
	path := filepath.Join(stateDirPath(), "openapi.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read cached openapi (run `auron-api sync` first): %w", err)
	}
	var spec OpenAPI
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, err
	}
	return &spec, nil
}

func apiBaseURL() (string, error) {
	spec, err := loadCachedSpec()
	if err == nil && len(spec.Servers) > 0 && spec.Servers[0].URL != "" {
		return strings.TrimRight(spec.Servers[0].URL, "/"), nil
	}
	if v := os.Getenv("AURON_API_BASE"); v != "" {
		return strings.TrimRight(v, "/"), nil
	}
	return "https://dev.useauron.ai/api/v1", nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func stateDirPath() string {
	if v := os.Getenv("AURON_STATE_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return defaultStateDir
	}
	return filepath.Join(home, defaultStateDir)
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func httpGet(ctx context.Context, u string) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("%s: %s", resp.Status, string(body))
	}
	return io.ReadAll(resp.Body)
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func parseKV(s string, set func(k, v string)) error {
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			return fmt.Errorf("bad key=value: %q", pair)
		}
		set(strings.TrimSpace(k), strings.TrimSpace(v))
	}
	return nil
}

func readBodyArg(arg string) ([]byte, error) {
	if arg == "-" {
		return io.ReadAll(os.Stdin)
	}
	if strings.HasPrefix(arg, "@") {
		return os.ReadFile(arg[1:])
	}
	return []byte(arg), nil
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	s = strings.ToLower(s)
	s = slugRe.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

func titleCase(s string) string {
	s = strings.ReplaceAll(s, "-", " ")
	s = strings.ReplaceAll(s, "_", " ")
	parts := strings.Fields(s)
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}

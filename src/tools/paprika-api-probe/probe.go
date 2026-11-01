package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://www.paprikaapp.com"
	loginPath      = "/api/v1/account/login/"
	syncPrefix     = "/api/v2/sync/"
	userAgent      = "paprika-mcp-go-apiprobe/0.1"
	maxBodyBytes   = 256 << 20
	maxIntValues   = 12
)

// candidateResources are the route names to try even if sync/status omits them.
var candidateResources = []string{
	"bookmarks", "categories", "groceries", "groceryaisles", "groceryingredients",
	"grocerylists", "meals", "mealtypes", "menuitems", "menus", "pantry", "photos",
	"recipes",
}

var (
	uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	hexPattern  = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
	datePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}([ T]\d{2}:\d{2}(:\d{2}(\.\d+)?)?(Z|[+-]\d{2}:?\d{2})?)?$`)
	digit       = regexp.MustCompile(`\d`)
)

var errWriteBlocked = errors.New("probe blocked a request that is not read-only")

// readOnlyTransport makes the read-only guarantee structural: the only non-GET
// request that can leave the process is the login.
type readOnlyTransport struct {
	next http.RoundTripper
}

func (t readOnlyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	isLogin := req.Method == http.MethodPost && req.URL.Path == loginPath
	if req.Method != http.MethodGet && !isLogin {
		return nil, fmt.Errorf("%w: %s %s", errWriteBlocked, req.Method, req.URL.Path)
	}
	return t.next.RoundTrip(req)
}

type prober struct {
	client  *http.Client
	baseURL string
	token   string
	delay   time.Duration
	out     io.Writer
}

func newProber(baseURL string, delay time.Duration, out io.Writer) *prober {
	return &prober{
		client: &http.Client{
			Timeout:   60 * time.Second,
			Transport: readOnlyTransport{next: http.DefaultTransport},
			// A redirect could carry the token to another host, so report it instead.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		baseURL: strings.TrimRight(baseURL, "/"),
		delay:   delay,
		out:     out,
	}
}

func (p *prober) login(ctx context.Context, email, password string) error {
	form := url.Values{"email": {email}, "password": {password}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+loginPath, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("build login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)

	resp, err := p.client.Do(req)
	if err != nil {
		// Transport errors include the URL but never the form body.
		return fmt.Errorf("login request: %w", err)
	}
	defer resp.Body.Close()

	var body struct {
		Result struct {
			Token string `json:"token"`
		} `json:"result"`
		Error *apiError `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&body); err != nil {
		return fmt.Errorf("login returned HTTP %d with an unreadable body: %w", resp.StatusCode, err)
	}
	if body.Error != nil {
		return fmt.Errorf("login returned HTTP %d: %s (code %d)", resp.StatusCode, body.Error.Message, body.Error.Code)
	}
	if resp.StatusCode != http.StatusOK || body.Result.Token == "" {
		return fmt.Errorf("login returned HTTP %d with no token", resp.StatusCode)
	}
	p.token = body.Result.Token
	return nil
}

type apiError struct {
	Message string `json:"message"`
	Code    int    `json:"code"`
}

type fetchResult struct {
	status          int
	contentEncoding string
	envelopeKeys    []string
	result          any
	apiErr          *apiError
}

func (p *prober) fetch(ctx context.Context, path string) (*fetchResult, error) {
	if p.delay > 0 {
		select {
		case <-time.After(p.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("User-Agent", userAgent)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	res := &fetchResult{status: resp.StatusCode, contentEncoding: resp.Header.Get("Content-Encoding")}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes))
	decoder.UseNumber()
	var envelope map[string]json.RawMessage
	if err := decoder.Decode(&envelope); err != nil {
		// Non-JSON bodies are expected on a missing route. Their content is never shown.
		return res, nil
	}
	res.envelopeKeys = slices.Sorted(maps.Keys(envelope))

	if raw, ok := envelope["error"]; ok {
		var apiErr apiError
		if err := json.Unmarshal(raw, &apiErr); err == nil {
			res.apiErr = &apiErr
		}
	}
	if raw, ok := envelope["result"]; ok {
		inner := json.NewDecoder(strings.NewReader(string(raw)))
		inner.UseNumber()
		if err := inner.Decode(&res.result); err != nil {
			return nil, fmt.Errorf("decode result: %w", err)
		}
	}
	return res, nil
}

// fieldStats summarizes one JSON path across every object seen. It stores string
// shapes, never string values, so a report is safe to read and share.
type fieldStats struct {
	seen       int
	types      map[string]int
	shapes     map[string]int
	minLen     int
	maxLen     int
	trueCount  int
	falseCount int
	ints       map[string]int
	intsCapped bool
}

type summary struct {
	objects int
	fields  map[string]*fieldStats
}

func newSummary() *summary {
	return &summary{fields: map[string]*fieldStats{}}
}

func (s *summary) field(path string) *fieldStats {
	f, ok := s.fields[path]
	if !ok {
		f = &fieldStats{types: map[string]int{}, shapes: map[string]int{}, ints: map[string]int{}, minLen: -1}
		s.fields[path] = f
	}
	return f
}

// addObject records the fields of one object, treating its keys as field names.
func (s *summary) addObject(object map[string]any, prefix string) {
	for key, value := range object {
		s.addValue(prefix+key, value)
	}
}

func (s *summary) addValue(path string, value any) {
	f := s.field(path)
	f.seen++
	switch v := value.(type) {
	case nil:
		f.types["null"]++
	case bool:
		f.types["boolean"]++
		if v {
			f.trueCount++
		} else {
			f.falseCount++
		}
	case json.Number:
		f.types[numberType(v)]++
		f.recordNumber(v.String())
	case string:
		f.types["string"]++
		f.recordString(v)
	case []any:
		f.types["array"]++
		for _, item := range v {
			s.addValue(path+"[]", item)
		}
	case map[string]any:
		f.types["object"]++
		s.addObject(v, path+".")
	}
}

func numberType(n json.Number) string {
	if _, err := strconv.ParseInt(n.String(), 10, 64); err == nil {
		return "integer"
	}
	return "number"
}

func (f *fieldStats) recordNumber(value string) {
	if _, ok := f.ints[value]; !ok && len(f.ints) >= maxIntValues {
		f.intsCapped = true
		return
	}
	f.ints[value]++
}

func (f *fieldStats) recordString(value string) {
	length := len([]rune(value))
	if f.minLen < 0 || length < f.minLen {
		f.minLen = length
	}
	if length > f.maxLen {
		f.maxLen = length
	}
	f.shapes[stringShape(value)]++
}

// stringShape classifies a string without revealing it. Only date-like strings
// keep their punctuation, with every digit masked.
func stringShape(value string) string {
	switch {
	case value == "":
		return "empty"
	case uuidPattern.MatchString(value):
		if value == strings.ToUpper(value) {
			return "uuid-upper"
		}
		return "uuid-lower"
	case hexPattern.MatchString(value):
		if value == strings.ToUpper(value) {
			return "sha256-hex-upper"
		}
		return "sha256-hex-lower"
	case datePattern.MatchString(value):
		return "date " + digit.ReplaceAllString(value, "9")
	case strings.HasPrefix(value, "http://"), strings.HasPrefix(value, "https://"):
		return "url"
	case strings.Contains(value, "\n"):
		return "multiline-text"
	default:
		return "text"
	}
}

func (s *summary) write(w io.Writer) {
	for _, path := range slices.Sorted(maps.Keys(s.fields)) {
		f := s.fields[path]
		fmt.Fprintf(w, "- `%s` seen=%d types=%s", path, f.seen, formatCounts(f.types))
		if len(f.shapes) > 0 {
			fmt.Fprintf(w, " shapes=%s length=%d..%d", formatCounts(f.shapes), f.minLen, f.maxLen)
		}
		if f.trueCount+f.falseCount > 0 {
			fmt.Fprintf(w, " true=%d false=%d", f.trueCount, f.falseCount)
		}
		if len(f.ints) > 0 {
			fmt.Fprintf(w, " values=%s", formatCounts(f.ints))
			if f.intsCapped {
				fmt.Fprint(w, " (more omitted)")
			}
		}
		fmt.Fprintln(w)
	}
}

func formatCounts(counts map[string]int) string {
	parts := make([]string, 0, len(counts))
	for _, key := range slices.Sorted(maps.Keys(counts)) {
		parts = append(parts, fmt.Sprintf("%s:%d", key, counts[key]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func (p *prober) writeHeader(path string, res *fetchResult) {
	fmt.Fprintf(p.out, "\n## GET %s\n\n", path)
	fmt.Fprintf(p.out, "HTTP %d, content-encoding=%q, envelope keys=%v\n", res.status, res.contentEncoding, res.envelopeKeys)
	if res.apiErr != nil {
		fmt.Fprintf(p.out, "API error: %s (code %d)\n", res.apiErr.Message, res.apiErr.Code)
	}
}

// probeList summarizes one route and returns its array items, if it returned any.
func (p *prober) probeList(ctx context.Context, path string) ([]any, error) {
	res, err := p.fetch(ctx, path)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		fmt.Fprintf(p.out, "\n## GET %s\n\nrequest failed: %v\n", path, err)
		return nil, nil
	}
	p.writeHeader(path, res)

	items, isArray := res.result.([]any)
	if !isArray {
		if res.result != nil {
			fmt.Fprintf(p.out, "result type: %T\n", res.result)
		}
		return nil, nil
	}
	fmt.Fprintf(p.out, "result: array of %d\n\n", len(items))
	stats := newSummary()
	for _, item := range items {
		if object, ok := item.(map[string]any); ok {
			stats.addObject(object, "")
		}
	}
	stats.write(p.out)
	return items, nil
}

// probeStatus reports the counters. Numbers and resource names carry no personal
// data, so they are printed as they are.
func (p *prober) probeStatus(ctx context.Context) ([]string, error) {
	path := syncPrefix + "status/"
	res, err := p.fetch(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("fetch status: %w", err)
	}
	p.writeHeader(path, res)

	counters, ok := res.result.(map[string]any)
	if !ok {
		fmt.Fprintf(p.out, "result type: %T\n", res.result)
		return nil, nil
	}
	names := slices.Sorted(maps.Keys(counters))
	fmt.Fprintln(p.out)
	for _, name := range names {
		fmt.Fprintf(p.out, "- `%s` = %v\n", name, counters[name])
	}
	return names, nil
}

// probeRecipes reads a few full recipes, spread evenly through the list.
func (p *prober) probeRecipes(ctx context.Context, items []any, samples int) error {
	uids := make([]string, 0, len(items))
	for _, item := range items {
		if object, ok := item.(map[string]any); ok {
			if uid, ok := object["uid"].(string); ok && uuidPattern.MatchString(uid) {
				uids = append(uids, uid)
			}
		}
	}
	if len(uids) == 0 || samples <= 0 {
		fmt.Fprintln(p.out, "\n## GET sync/recipe/{uid}/\n\nskipped: no recipe uid found")
		return nil
	}
	samples = min(samples, len(uids))

	stats := newSummary()
	statuses := map[string]int{}
	for i := range samples {
		res, err := p.fetch(ctx, syncPrefix+"recipe/"+uids[i*len(uids)/samples]+"/")
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			statuses["request failed"]++
			continue
		}
		statuses[strconv.Itoa(res.status)]++
		if object, ok := res.result.(map[string]any); ok {
			stats.addObject(object, "")
		}
	}
	fmt.Fprintf(p.out, "\n## GET sync/recipe/{uid}/\n\nsampled %d, statuses=%s\n\n", samples, formatCounts(statuses))
	stats.write(p.out)
	return nil
}

func (p *prober) run(ctx context.Context, email, password string, samples int) error {
	if err := p.login(ctx, email, password); err != nil {
		return err
	}
	fmt.Fprintln(p.out, "# Paprika API probe report")
	fmt.Fprintf(p.out, "\nGenerated %s. Login succeeded with User-Agent %q.\n", time.Now().UTC().Format(time.RFC3339), userAgent)

	counterNames, err := p.probeStatus(ctx)
	if err != nil {
		return err
	}

	resources := append(slices.Clone(candidateResources), counterNames...)
	slices.Sort(resources)
	resources = slices.Compact(resources)

	var recipes []any
	for _, name := range resources {
		// A counter name comes from the server, so it must be a plain route segment.
		if !isRouteSegment(name) {
			continue
		}
		items, err := p.probeList(ctx, syncPrefix+name+"/")
		if err != nil {
			return err
		}
		if name == "recipes" {
			recipes = items
		}
	}
	return p.probeRecipes(ctx, recipes, samples)
}

var routeSegment = regexp.MustCompile(`^[a-z0-9_-]+$`)

func isRouteSegment(name string) bool {
	return name != "status" && routeSegment.MatchString(name)
}

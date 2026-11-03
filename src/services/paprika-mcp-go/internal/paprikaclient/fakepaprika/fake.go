// Package fakepaprika is an in-process fake of the Paprika cloud sync API for
// tests. It serves only synthetic data, records every request, and behaves like
// the real server did in the write checks:
//
//   - stale writes are accepted, and unknown references are stored as sent;
//   - a write merges by uid, so fields omitted from an upload are kept;
//   - an object posted with "deleted": true leaves the lists;
//   - counters rise by one per written object, including inside a bulk POST;
//   - the recipe list includes trashed recipes.
//
// Faults injected with [Server.Inject] cover error envelopes on HTTP 200, 401,
// 503, slow responses, redirects, and arbitrary bodies.
package fakepaprika

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Default credentials accepted by a new server.
const (
	DefaultEmail    = "cook@example.test"
	DefaultPassword = "fake-password-for-tests"
)

const (
	loginPath  = "/api/v1/account/login/"
	syncPrefix = "/api/v2/sync/"
)

// listResources are the bulk resources the fake serves and accepts.
var listResources = map[string]bool{
	"categories":         true,
	"meals":              true,
	"mealtypes":          true,
	"groceries":          true,
	"grocerylists":       true,
	"groceryaisles":      true,
	"groceryingredients": true,
}

// Part describes one part of a recorded multipart upload.
type Part struct {
	Name        string
	Filename    string
	ContentType string
	// Gzipped is true when the part content began with the gzip magic bytes.
	Gzipped bool
}

// Request is one recorded request. The Authorization value is never kept. A
// request is listed as soon as it arrives, with only its method, path, headers,
// and time; the rest is filled in when the response is sent.
type Request struct {
	Time   time.Time
	Method string
	Path   string
	// Header is a copy of the request headers, with Authorization replaced by
	// "[redacted]" when present.
	Header http.Header
	// Authorized reports whether the bearer token was valid when received.
	Authorized bool
	// Email is the email from a login form. The password is not kept.
	Email string
	// Body is the decoded, gunzipped JSON of the "data" part, or nil.
	Body json.RawMessage
	// Parts lists every multipart part received.
	Parts []Part
	// BodyError is set when an upload could not be decoded.
	BodyError string
	// Status is the HTTP status sent back.
	Status int
}

// FaultKind selects what an injected fault does.
type FaultKind int

// Fault kinds.
const (
	// FaultEnvelopeError answers HTTP 200 with {"error": {...}}.
	FaultEnvelopeError FaultKind = iota + 1
	// FaultUnauthorized answers HTTP 401.
	FaultUnauthorized
	// FaultUnavailable answers HTTP 503.
	FaultUnavailable
	// FaultStatus answers Fault.Status with an error envelope.
	FaultStatus
	// FaultDelay waits Fault.Delay, then handles the request normally. If the
	// client gives up first, the request is dropped unapplied.
	FaultDelay
	// FaultRedirect answers 302 with Location set to Fault.Location.
	FaultRedirect
	// FaultBody answers HTTP 200 with Fault.Body as is.
	FaultBody
)

// Fault is an injected failure.
type Fault struct {
	// Method and Path select requests. An empty value matches anything. Path is
	// compared exactly with the request path.
	Method string
	Path   string
	// Times is how many matching requests are affected. Zero means once, and a
	// negative number means every matching request.
	Times int
	Kind  FaultKind

	Code     int           // FaultEnvelopeError and FaultStatus
	Message  string        // FaultEnvelopeError and FaultStatus
	Status   int           // FaultStatus
	Delay    time.Duration // FaultDelay
	Location string        // FaultRedirect
	Body     []byte        // FaultBody
	Gzip     bool          // FaultBody: compress Body and set Content-Encoding
}

type activeFault struct {
	Fault
	left int
}

// Server is a running fake. Create one with [New].
type Server struct {
	srv *httptest.Server

	mu          sync.Mutex
	email       string
	password    string
	tokens      map[string]bool
	tokenSeq    int
	lastToken   string
	loginCount  int
	recipes     map[string]map[string]any
	recipeOrder []string
	lists       map[string][]map[string]any
	counters    map[string]int64
	requests    []Request
	faults      []*activeFault
	inflight    int
	maxInflight int
}

// New starts a fake server that accepts [DefaultEmail] and [DefaultPassword],
// and closes it when the test ends.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{
		email:    DefaultEmail,
		password: DefaultPassword,
		tokens:   map[string]bool{},
		recipes:  map[string]map[string]any{},
		lists:    map[string][]map[string]any{},
		counters: map[string]int64{"recipes": 0},
	}
	for name := range listResources {
		s.counters[name] = 0
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.srv.Close)
	return s
}

// URL is the base URL to give the client.
func (s *Server) URL() string { return s.srv.URL }

// Email is the accepted account email.
func (s *Server) Email() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.email
}

// Password is the accepted account password.
func (s *Server) Password() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.password
}

// SetPassword changes the accepted password, as if it were changed on the account.
func (s *Server) SetPassword(password string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.password = password
}

// RevokeTokens invalidates every issued token, so the next authenticated
// request gets a 401.
func (s *Server) RevokeTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = map[string]bool{}
}

// LastToken is the most recently issued token, for checking that it is never logged.
func (s *Server) LastToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastToken
}

// LoginCount is the number of login requests received, successful or not.
func (s *Server) LoginCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loginCount
}

// MaxConcurrent is the highest number of requests handled at the same time.
func (s *Server) MaxConcurrent() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxInflight
}

// Inject adds a fault. Faults are tried in the order they were added.
func (s *Server) Inject(f Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	left := f.Times
	if left == 0 {
		left = 1
	}
	s.faults = append(s.faults, &activeFault{Fault: f, left: left})
}

// ClearFaults removes every injected fault.
func (s *Server) ClearFaults() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = nil
}

// Requests returns a copy of every recorded request, oldest first.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// RequestsTo returns the recorded requests with the given method and path.
func (s *Server) RequestsTo(method, path string) []Request {
	var out []Request
	for _, r := range s.Requests() {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

// ResetRequests forgets the recorded requests.
func (s *Server) ResetRequests() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = nil
}

// Counters returns a copy of the change counters.
func (s *Server) Counters() map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int64, len(s.counters))
	for k, v := range s.counters {
		out[k] = v
	}
	return out
}

// Objects returns a copy of the stored objects of a bulk resource, such as "meals".
func (s *Server) Objects(resource string) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]any, 0, len(s.lists[resource]))
	for _, o := range s.lists[resource] {
		out = append(out, deepCopy(o))
	}
	return out
}

// StoredRecipe returns a copy of a stored recipe.
func (s *Server) StoredRecipe(uid string) (map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.recipes[uid]
	if !ok {
		return nil, false
	}
	return deepCopy(r), true
}

// SeedRecipe stores a recipe as if it had been uploaded. It needs a "uid".
func (s *Server) SeedRecipe(recipe map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upsertRecipe(uidOf(recipe), deepCopy(recipe))
}

// Seed stores objects of a bulk resource as if they had been uploaded. Each
// needs a "uid".
func (s *Server) Seed(resource string, objects ...map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range objects {
		s.upsertListObject(resource, deepCopy(o))
	}
}

func uidOf(o map[string]any) string {
	uid, _ := o["uid"].(string)
	return uid
}

func deepCopy(o map[string]any) map[string]any {
	raw, err := json.Marshal(o)
	if err != nil {
		panic(err)
	}
	return decodeObject(raw)
}

func decodeObject(raw []byte) map[string]any {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		panic(err)
	}
	return out
}

// upsertRecipe merges by uid. The caller holds s.mu.
func (s *Server) upsertRecipe(uid string, obj map[string]any) {
	s.counters["recipes"]++
	if obj["deleted"] == true {
		delete(s.recipes, uid)
		for i, id := range s.recipeOrder {
			if id == uid {
				s.recipeOrder = append(s.recipeOrder[:i], s.recipeOrder[i+1:]...)
				break
			}
		}
		return
	}
	delete(obj, "deleted")
	existing, ok := s.recipes[uid]
	if !ok {
		s.recipes[uid] = obj
		s.recipeOrder = append(s.recipeOrder, uid)
		return
	}
	for k, v := range obj {
		existing[k] = v
	}
}

// upsertListObject merges by uid. The caller holds s.mu.
func (s *Server) upsertListObject(resource string, obj map[string]any) {
	s.counters[resource]++
	uid := uidOf(obj)
	list := s.lists[resource]
	idx := -1
	for i, o := range list {
		if uidOf(o) == uid {
			idx = i
			break
		}
	}
	if obj["deleted"] == true {
		if idx >= 0 {
			s.lists[resource] = append(list[:idx], list[idx+1:]...)
		}
		return
	}
	delete(obj, "deleted")
	if idx < 0 {
		s.lists[resource] = append(list, obj)
		return
	}
	for k, v := range obj {
		list[idx][k] = v
	}
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	rec := Request{
		Time:   time.Now(),
		Method: r.Method,
		Path:   r.URL.Path,
		Header: r.Header.Clone(),
	}
	if rec.Header.Get("Authorization") != "" {
		rec.Header.Set("Authorization", "[redacted]")
	}

	s.mu.Lock()
	s.inflight++
	s.maxInflight = max(s.maxInflight, s.inflight)
	idx := len(s.requests)
	s.requests = append(s.requests, rec)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inflight--
		s.mu.Unlock()
	}()

	status := s.serve(w, r, &rec)

	rec.Status = status
	s.mu.Lock()
	s.requests[idx] = rec
	s.mu.Unlock()
}

// serve answers one request and returns the status it sent.
func (s *Server) serve(w http.ResponseWriter, r *http.Request, rec *Request) int {
	s.mu.Lock()
	fault := s.takeFault(r)
	s.mu.Unlock()

	if fault != nil {
		switch fault.Kind {
		case FaultEnvelopeError:
			return writeJSON(w, r, http.StatusOK, errorEnvelope(fault.Code, fault.Message))
		case FaultUnauthorized:
			return writeJSON(
				w,
				r,
				http.StatusUnauthorized,
				errorEnvelope(0, "Invalid or expired credentials."),
			)
		case FaultUnavailable:
			return writeJSON(
				w,
				r,
				http.StatusServiceUnavailable,
				errorEnvelope(0, "Service unavailable."),
			)
		case FaultStatus:
			return writeJSON(w, r, fault.Status, errorEnvelope(fault.Code, fault.Message))
		case FaultRedirect:
			w.Header().Set("Location", fault.Location)
			w.WriteHeader(http.StatusFound)
			return http.StatusFound
		case FaultBody:
			return writeRaw(w, r, fault.Body, fault.Gzip)
		case FaultDelay:
			// Reading the body lets the server notice a client that hangs up.
			buffered, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(buffered))
			select {
			case <-time.After(fault.Delay):
			case <-r.Context().Done():
				return 0
			}
		}
	}

	if r.URL.Path == loginPath {
		return s.login(w, r, rec)
	}

	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	rec.Authorized = token != "" && s.tokens[token]
	s.mu.Unlock()
	if !rec.Authorized {
		return writeJSON(
			w,
			r,
			http.StatusUnauthorized,
			errorEnvelope(0, "Invalid or expired credentials."),
		)
	}
	if !strings.HasPrefix(r.URL.Path, syncPrefix) {
		return writeJSON(w, r, http.StatusNotFound, errorEnvelope(0, "Not found."))
	}
	return s.route(w, r, rec, strings.TrimPrefix(r.URL.Path, syncPrefix))
}

// takeFault returns the first active fault matching the request and uses up
// one of its remaining hits. The caller holds s.mu.
func (s *Server) takeFault(r *http.Request) *Fault {
	for i, f := range s.faults {
		if f.Method != "" && f.Method != r.Method {
			continue
		}
		if f.Path != "" && f.Path != r.URL.Path {
			continue
		}
		fault := f.Fault
		if f.left > 0 {
			f.left--
			if f.left == 0 {
				s.faults = append(s.faults[:i], s.faults[i+1:]...)
			}
		}
		return &fault
	}
	return nil
}

func (s *Server) login(w http.ResponseWriter, r *http.Request, rec *Request) int {
	if r.Method != http.MethodPost {
		return writeJSON(w, r, http.StatusMethodNotAllowed, errorEnvelope(0, "Method not allowed."))
	}
	if err := r.ParseForm(); err != nil {
		return writeJSON(w, r, http.StatusBadRequest, errorEnvelope(0, "Bad request."))
	}
	rec.Email = r.PostForm.Get("email")

	s.mu.Lock()
	defer s.mu.Unlock()
	s.loginCount++
	if rec.Email != s.email || r.PostForm.Get("password") != s.password {
		return writeJSON(w, r, http.StatusOK, errorEnvelope(0, "Incorrect password."))
	}
	s.tokenSeq++
	token := fmt.Sprintf("fake-token-%d", s.tokenSeq)
	s.tokens[token] = true
	s.lastToken = token
	return writeJSON(
		w,
		r,
		http.StatusOK,
		map[string]any{"result": map[string]string{"token": token}},
	)
}

func (s *Server) route(w http.ResponseWriter, r *http.Request, rec *Request, rest string) int {
	notFound := func() int {
		return writeJSON(w, r, http.StatusNotFound, errorEnvelope(0, "Not found."))
	}
	switch {
	case rest == "status/" && r.Method == http.MethodGet:
		s.mu.Lock()
		defer s.mu.Unlock()
		return writeJSON(w, r, http.StatusOK, map[string]any{"result": s.counters})

	case rest == "notify/" && r.Method == http.MethodPost:
		return writeJSON(w, r, http.StatusOK, map[string]any{"result": true})

	case rest == "recipes/" && r.Method == http.MethodGet:
		s.mu.Lock()
		defer s.mu.Unlock()
		refs := make([]map[string]any, 0, len(s.recipeOrder))
		for _, uid := range s.recipeOrder {
			refs = append(refs, map[string]any{"uid": uid, "hash": s.recipes[uid]["hash"]})
		}
		return writeJSON(w, r, http.StatusOK, map[string]any{"result": refs})

	case strings.HasPrefix(rest, "recipe/") && strings.HasSuffix(rest, "/"):
		uid := strings.TrimSuffix(strings.TrimPrefix(rest, "recipe/"), "/")
		if uid == "" || strings.Contains(uid, "/") {
			return notFound()
		}
		switch r.Method {
		case http.MethodGet:
			s.mu.Lock()
			defer s.mu.Unlock()
			recipe, ok := s.recipes[uid]
			if !ok {
				// The live server answers 200 with an error envelope, not a 404.
				return writeJSON(w, r, http.StatusOK, errorEnvelope(0, "Recipe not found."))
			}
			return writeJSON(w, r, http.StatusOK, map[string]any{"result": recipe})
		case http.MethodPost:
			body, ok := readUpload(r, rec)
			if !ok {
				return writeJSON(w, r, http.StatusBadRequest, errorEnvelope(0, "Bad upload."))
			}
			obj := decodeObjectOrNil(body)
			if obj == nil {
				return writeJSON(w, r, http.StatusBadRequest, errorEnvelope(0, "Bad upload."))
			}
			if _, has := obj["uid"]; !has {
				obj["uid"] = uid
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			s.upsertRecipe(uid, obj)
			return writeJSON(w, r, http.StatusOK, map[string]any{"result": true})
		}
		return notFound()

	case strings.HasSuffix(rest, "/") && listResources[strings.TrimSuffix(rest, "/")]:
		resource := strings.TrimSuffix(rest, "/")
		switch r.Method {
		case http.MethodGet:
			s.mu.Lock()
			defer s.mu.Unlock()
			list := s.lists[resource]
			if list == nil {
				list = []map[string]any{}
			}
			return writeJSON(w, r, http.StatusOK, map[string]any{"result": list})
		case http.MethodPost:
			body, ok := readUpload(r, rec)
			if !ok {
				return writeJSON(w, r, http.StatusBadRequest, errorEnvelope(0, "Bad upload."))
			}
			var objs []json.RawMessage
			if err := json.Unmarshal(body, &objs); err != nil {
				return writeJSON(w, r, http.StatusBadRequest, errorEnvelope(0, "Bad upload."))
			}
			parsed := make([]map[string]any, 0, len(objs))
			for _, raw := range objs {
				obj := decodeObjectOrNil(raw)
				if obj == nil || uidOf(obj) == "" {
					return writeJSON(w, r, http.StatusBadRequest, errorEnvelope(0, "Bad upload."))
				}
				parsed = append(parsed, obj)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			for _, obj := range parsed {
				s.upsertListObject(resource, obj)
			}
			return writeJSON(w, r, http.StatusOK, map[string]any{"result": true})
		}
	}
	return notFound()
}

func decodeObjectOrNil(raw []byte) (obj map[string]any) {
	defer func() {
		if recover() != nil {
			obj = nil
		}
	}()
	return decodeObject(raw)
}

// readUpload decodes the multipart "data" part, which holds gzipped JSON, and
// records its structure.
func readUpload(r *http.Request, rec *Request) ([]byte, bool) {
	mr, err := r.MultipartReader()
	if err != nil {
		rec.BodyError = "not multipart: " + err.Error()
		return nil, false
	}
	var data []byte
	found := false
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			rec.BodyError = "multipart: " + err.Error()
			return nil, false
		}
		content, err := io.ReadAll(part)
		if err != nil {
			rec.BodyError = "multipart: " + err.Error()
			return nil, false
		}
		info := Part{
			Name:        part.FormName(),
			Filename:    part.FileName(),
			ContentType: part.Header.Get("Content-Type"),
			Gzipped:     len(content) > 2 && content[0] == 0x1f && content[1] == 0x8b,
		}
		rec.Parts = append(rec.Parts, info)
		if info.Name != "data" {
			continue
		}
		found = true
		zr, err := gzip.NewReader(bytes.NewReader(content))
		if err != nil {
			rec.BodyError = "gzip: " + err.Error()
			return nil, false
		}
		data, err = io.ReadAll(zr)
		if err != nil {
			rec.BodyError = "gzip: " + err.Error()
			return nil, false
		}
		if !json.Valid(data) {
			rec.BodyError = "data part is not JSON"
			return nil, false
		}
		rec.Body = append(json.RawMessage(nil), data...)
	}
	if !found {
		rec.BodyError = "no data part"
		return nil, false
	}
	return data, true
}

func errorEnvelope(code int, message string) map[string]any {
	return map[string]any{"error": map[string]any{"message": message, "code": code}}
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, payload any) int {
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return writeRawStatus(
		w,
		r,
		status,
		raw,
		strings.Contains(r.Header.Get("Accept-Encoding"), "gzip"),
	)
}

func writeRaw(w http.ResponseWriter, r *http.Request, body []byte, compress bool) int {
	return writeRawStatus(w, r, http.StatusOK, body, compress)
}

func writeRawStatus(
	w http.ResponseWriter,
	_ *http.Request,
	status int,
	body []byte,
	compress bool,
) int {
	w.Header().Set("Content-Type", "application/json")
	if compress {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(body)
		_ = zw.Close()
		body = buf.Bytes()
		w.Header().Set("Content-Encoding", "gzip")
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
	return status
}

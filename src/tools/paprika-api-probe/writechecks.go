package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
)

const (
	testPrefix        = "MCP-TEST"
	maxUnconfirmedLib = 25
	farFutureDate     = "2099-01-01 00:00:00"
)

// backupResources are saved before any write. Recipes are not included, because
// reading them all is slow and the account owner backs them up separately.
var backupResources = []string{
	"categories",
	"groceries",
	"groceryaisles",
	"groceryingredients",
	"grocerylists",
	"meals",
	"mealtypes",
}

var (
	errNotOwned   = errors.New("refusing to write an object this tool did not create")
	errGuard      = errors.New("write guard")
	errUnexpected = errors.New("stopped on an unexpected response")
)

// writeOptions are the safeguards a caller must satisfy before anything is written.
type writeOptions struct {
	email          string
	confirmAccount string
	allowRealAcct  bool
	backupPath     string
	statePath      string
}

type stateObject struct {
	Resource string `json:"resource"`
	UID      string `json:"uid"`
}

// checker runs write checks. Every object it writes is recorded in the state file
// before the request is sent, so a timeout cannot orphan one.
type checker struct {
	p         *prober
	out       io.Writer
	statePath string
	objects   []stateObject
}

type object = map[string]any

func requireLocalPath(flagName, path string) error {
	if !strings.HasSuffix(path, ".local") {
		return fmt.Errorf("%w: %s must end in .local so that git ignores it", errGuard, flagName)
	}
	return nil
}

func checkConfirmation(opts writeOptions) error {
	if opts.confirmAccount == "" || opts.confirmAccount != opts.email {
		return fmt.Errorf("%w: -confirm-account must repeat the configured email exactly", errGuard)
	}
	return requireLocalPath("-state", opts.statePath)
}

func newWriteProber(baseURL string, delay time.Duration, out io.Writer) *prober {
	p := newProber(baseURL, delay, out)
	p.client.Transport = readOnlyTransport{next: http.DefaultTransport, allowWrites: true}
	return p
}

func newUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := strings.ToUpper(hex.EncodeToString(b[:]))
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func newHash() string {
	sum := sha256.Sum256([]byte(newUID()))
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

func (c *checker) saveState() error {
	data, err := json.MarshalIndent(c.objects, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.statePath, append(data, '\n'), 0o600)
}

func (c *checker) own(resource string) (string, error) {
	uid := newUID()
	c.objects = append(c.objects, stateObject{resource, uid})
	if err := c.saveState(); err != nil {
		return "", fmt.Errorf("save state: %w", err)
	}
	return uid, nil
}

func (c *checker) owns(resource, uid string) bool {
	return slices.Contains(c.objects, stateObject{resource, uid})
}

// assertOwned is the last check before a write: only objects recorded by this tool,
// and named as test objects, can be sent.
func (c *checker) assertOwned(resource string, objects []object) error {
	for _, obj := range objects {
		uid, _ := obj["uid"].(string)
		name, _ := obj["name"].(string)
		if !c.owns(resource, uid) || !strings.HasPrefix(name, testPrefix) {
			return fmt.Errorf("%w: %s", errNotOwned, resource)
		}
	}
	return nil
}

func encodeUpload(payload any) (*bytes.Buffer, string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, "", err
	}
	var compressed bytes.Buffer
	zipper := gzip.NewWriter(&compressed)
	if _, err := zipper.Write(raw); err != nil {
		return nil, "", err
	}
	if err := zipper.Close(); err != nil {
		return nil, "", err
	}

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("data", "file")
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(compressed.Bytes()); err != nil {
		return nil, "", err
	}
	if err := form.Close(); err != nil {
		return nil, "", err
	}
	return &body, form.FormDataContentType(), nil
}

// post sends one write with no retry. A transport failure, server error, or auth
// failure stops the whole run, because the outcome is unknown or the server objects.
func (c *checker) post(ctx context.Context, path string, payload any) (*fetchResult, error) {
	if err := c.p.pause(ctx); err != nil {
		return nil, err
	}
	var body io.Reader
	contentType := ""
	if payload != nil {
		encoded, formType, err := encodeUpload(payload)
		if err != nil {
			return nil, fmt.Errorf("encode upload: %w", err)
		}
		body, contentType = encoded, formType
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.p.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.p.token)
	req.Header.Set("User-Agent", userAgent)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := c.p.send(req)
	if err != nil {
		return nil, fmt.Errorf("%w: POST %s, outcome unknown: %v", errUnexpected, path, err)
	}
	switch {
	case res.status >= 500, res.status == http.StatusUnauthorized, res.status == http.StatusForbidden, res.status == http.StatusTooManyRequests:
		return nil, fmt.Errorf("%w: POST %s returned HTTP %d", errUnexpected, path, res.status)
	}
	return res, nil
}

func accepted(res *fetchResult) bool {
	return res.status == http.StatusOK && res.apiErr == nil && res.result == true
}

func describeOutcome(res *fetchResult) string {
	if accepted(res) {
		return "accepted"
	}
	if res.apiErr != nil {
		return fmt.Sprintf(
			"rejected, HTTP %d, %q, code %d",
			res.status,
			res.apiErr.Message,
			res.apiErr.Code,
		)
	}
	return fmt.Sprintf("not accepted, HTTP %d, result %v", res.status, res.result)
}

func (c *checker) postList(
	ctx context.Context,
	resource string,
	objects ...object,
) (*fetchResult, error) {
	if err := c.assertOwned(resource, objects); err != nil {
		return nil, err
	}
	return c.post(ctx, syncPrefix+resource+"/", objects)
}

func (c *checker) postRecipe(ctx context.Context, recipe object) (*fetchResult, error) {
	if err := c.assertOwned("recipe", []object{recipe}); err != nil {
		return nil, err
	}
	uid, _ := recipe["uid"].(string)
	return c.post(ctx, syncPrefix+"recipe/"+uid+"/", recipe)
}

func (c *checker) readList(ctx context.Context, resource string) ([]object, error) {
	res, err := c.p.fetch(ctx, syncPrefix+resource+"/")
	if err != nil {
		return nil, fmt.Errorf("%w: GET %s: %v", errUnexpected, resource, err)
	}
	items, ok := res.result.([]any)
	if res.status != http.StatusOK || res.apiErr != nil || !ok {
		return nil, fmt.Errorf("%w: GET %s returned HTTP %d", errUnexpected, resource, res.status)
	}
	objects := make([]object, 0, len(items))
	for _, item := range items {
		if obj, ok := item.(object); ok {
			objects = append(objects, obj)
		}
	}
	return objects, nil
}

func (c *checker) findInList(ctx context.Context, resource, uid string) (object, error) {
	objects, err := c.readList(ctx, resource)
	if err != nil {
		return nil, err
	}
	for _, obj := range objects {
		if obj["uid"] == uid {
			return obj, nil
		}
	}
	return nil, nil
}

func (c *checker) readRecipe(ctx context.Context, uid string) (object, error) {
	res, err := c.p.fetch(ctx, syncPrefix+"recipe/"+uid+"/")
	if err != nil {
		return nil, fmt.Errorf("%w: GET recipe: %v", errUnexpected, err)
	}
	if res.status == http.StatusNotFound {
		return nil, nil
	}
	recipe, ok := res.result.(object)
	if res.status != http.StatusOK || !ok {
		return nil, fmt.Errorf("%w: GET recipe returned HTTP %d", errUnexpected, res.status)
	}
	return recipe, nil
}

func (c *checker) report(id, format string, args ...any) {
	fmt.Fprintf(c.out, "- **%s:** %s\n", id, fmt.Sprintf(format, args...))
}

func describeField(obj object, key string) string {
	if obj == nil {
		return "object not listed"
	}
	value, ok := obj[key]
	if !ok {
		return "absent"
	}
	if value == nil {
		return "null"
	}
	return fmt.Sprintf("%q", fmt.Sprint(value))
}

func (c *checker) saveBackup(ctx context.Context, path string) error {
	if err := requireLocalPath("-backup", path); err != nil {
		return err
	}
	backup := object{"saved": time.Now().UTC().Format(time.RFC3339)}
	for _, resource := range backupResources {
		objects, err := c.readList(ctx, resource)
		if err != nil {
			return err
		}
		backup[resource] = objects
		fmt.Fprintf(c.out, "- Backed up `%s`: %d objects\n", resource, len(objects))
	}
	data, err := json.MarshalIndent(backup, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func (c *checker) reportCounters(ctx context.Context, label string) error {
	res, err := c.p.fetch(ctx, syncPrefix+"status/")
	if err != nil {
		return fmt.Errorf("%w: GET status: %v", errUnexpected, err)
	}
	counters, ok := res.result.(object)
	if !ok {
		return fmt.Errorf("%w: GET status returned HTTP %d", errUnexpected, res.status)
	}
	fmt.Fprintf(c.out, "\nCounters %s:", label)
	for _, name := range []string{"groceries", "grocerylists", "meals", "recipes"} {
		fmt.Fprintf(c.out, " %s=%v", name, counters[name])
	}
	fmt.Fprintln(c.out)
	return nil
}

// reportReadOnly answers the questions that need no writes.
func (c *checker) reportReadOnly(ctx context.Context) error {
	fmt.Fprintln(c.out, "\n## Read-only checks")

	types, err := c.readList(ctx, "mealtypes")
	if err != nil {
		return err
	}
	for _, mealType := range types {
		c.report(
			"Meal type",
			"original_type=%v name=%v",
			mealType["original_type"],
			mealType["name"],
		)
	}

	if err := c.p.pause(ctx); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		c.p.baseURL+syncPrefix+"status/",
		nil,
	)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	req.Header.Set("User-Agent", userAgent)
	res, err := c.p.send(req)
	if err != nil {
		return fmt.Errorf("%w: bad token request: %v", errUnexpected, err)
	}
	message := "none"
	if res.apiErr != nil {
		message = fmt.Sprintf("%q code %d", res.apiErr.Message, res.apiErr.Code)
	}
	c.report("Bad token", "HTTP %d, error %s", res.status, message)
	return nil
}

func (c *checker) groceryItem(uid, name, listUID, aisleUID string) object {
	return object{
		"uid":         uid,
		"deleted":     false,
		"name":        name,
		"order_flag":  0,
		"purchased":   false,
		"recipe":      nil,
		"aisle":       testPrefix,
		"ingredient":  name,
		"instruction": "",
		"quantity":    "",
		"separate":    false,
		"aisle_uid":   aisleUID,
		"list_uid":    listUID,
	}
}

func (c *checker) ownedItem(name, listUID, aisleUID string) (object, error) {
	uid, err := c.own("groceries")
	if err != nil {
		return nil, err
	}
	return c.groceryItem(uid, name, listUID, aisleUID), nil
}

func firstUID(objects []object, resource string) (string, object, error) {
	if len(objects) == 0 {
		return "", nil, fmt.Errorf("%w: account has no %s", errGuard, resource)
	}
	uid, ok := objects[0]["uid"].(string)
	if !ok {
		return "", nil, fmt.Errorf("%w: %s has no uid", errGuard, resource)
	}
	return uid, objects[0], nil
}

func (c *checker) checkGroceries(ctx context.Context) error {
	fmt.Fprintln(c.out, "\n## Grocery checks")

	aisles, err := c.readList(ctx, "groceryaisles")
	if err != nil {
		return err
	}
	aisleUID, _, err := firstUID(aisles, "aisles")
	if err != nil {
		return err
	}

	listUID, err := c.own("grocerylists")
	if err != nil {
		return err
	}
	testList := object{
		"uid":            listUID,
		"deleted":        false,
		"name":           testPrefix + " list",
		"order_flag":     999,
		"is_default":     false,
		"reminders_list": "",
	}
	res, err := c.postList(ctx, "grocerylists", testList)
	if err != nil {
		return err
	}
	listed, err := c.findInList(ctx, "grocerylists", listUID)
	if err != nil {
		return err
	}
	c.report(
		"W1 (multipart, User-Agent)",
		"standard Go multipart: %s; list visible on re-read: %t",
		describeOutcome(res),
		listed != nil,
	)
	if !accepted(res) || listed == nil {
		return fmt.Errorf("%w: baseline write failed", errUnexpected)
	}

	full, err := c.ownedItem(testPrefix+" W2 omitted fields", listUID, aisleUID)
	if err != nil {
		return err
	}
	full["quantity"], full["instruction"] = "1", "keep me"
	if res, err = c.postList(ctx, "groceries", full); err != nil {
		return err
	}
	if !accepted(res) {
		return fmt.Errorf("%w: baseline item write: %s", errUnexpected, describeOutcome(res))
	}
	partial := maps.Clone(full)
	delete(partial, "quantity")
	delete(partial, "instruction")
	if res, err = c.postList(ctx, "groceries", partial); err != nil {
		return err
	}
	item, err := c.findInList(ctx, "groceries", full["uid"].(string))
	if err != nil {
		return err
	}
	c.report(
		"W2 (omitted fields)",
		"upload %s; after omitting them, quantity=%s instruction=%s",
		describeOutcome(res),
		describeField(item, "quantity"),
		describeField(item, "instruction"),
	)

	var bulk []object
	for i := range 3 {
		entry, err := c.ownedItem(fmt.Sprintf("%s W9 bulk %d", testPrefix, i+1), listUID, aisleUID)
		if err != nil {
			return err
		}
		bulk = append(bulk, entry)
	}
	if res, err = c.postList(ctx, "groceries", bulk...); err != nil {
		return err
	}
	current, err := c.readList(ctx, "groceries")
	if err != nil {
		return err
	}
	found := 0
	for _, entry := range bulk {
		for _, obj := range current {
			if obj["uid"] == entry["uid"] {
				found++
			}
		}
	}
	c.report(
		"W9 (bulk POST)",
		"upload %s; %d of %d objects listed",
		describeOutcome(res),
		found,
		len(bulk),
	)
	c.report("New-object UUIDs", "%d objects created with uppercase UUIDs were listed", found)

	cases := []struct {
		id, name, field, aisle, list string
	}{
		{"W5 (empty aisle_uid)", "W5 empty aisle", "aisle_uid", "", listUID},
		{"W5 (unknown aisle_uid)", "W5 unknown aisle", "aisle_uid", newHash(), listUID},
		{"Unknown list_uid", "unknown list", "list_uid", aisleUID, newUID()},
	}
	for _, tc := range cases {
		entry, err := c.ownedItem(testPrefix+" "+tc.name, tc.list, tc.aisle)
		if err != nil {
			return err
		}
		if res, err = c.postList(ctx, "groceries", entry); err != nil {
			return err
		}
		stored, err := c.findInList(ctx, "groceries", entry["uid"].(string))
		if err != nil {
			return err
		}
		c.report(
			tc.id,
			"upload %s; listed=%t, stored value unchanged=%t",
			describeOutcome(res),
			stored != nil,
			stored != nil && stored[tc.field] == entry[tc.field],
		)
	}

	restore, err := c.ownedItem(testPrefix+" W6 restore", listUID, aisleUID)
	if err != nil {
		return err
	}
	return c.checkRestore(ctx, "groceries", "W6 (grocery item)", restore)
}

func (c *checker) checkRestore(ctx context.Context, resource, id string, obj object) error {
	uid := obj["uid"].(string)
	res, err := c.postList(ctx, resource, obj)
	if err != nil {
		return err
	}
	if !accepted(res) {
		return fmt.Errorf(
			"%w: baseline %s write: %s",
			errUnexpected,
			resource,
			describeOutcome(res),
		)
	}
	created, err := c.findInList(ctx, resource, uid)
	if err != nil {
		return err
	}
	removed := maps.Clone(obj)
	removed["deleted"] = true
	if _, err := c.postList(ctx, resource, removed); err != nil {
		return err
	}
	afterDelete, err := c.findInList(ctx, resource, uid)
	if err != nil {
		return err
	}
	if res, err = c.postList(ctx, resource, obj); err != nil {
		return err
	}
	afterRestore, err := c.findInList(ctx, resource, uid)
	if err != nil {
		return err
	}
	c.report(
		id,
		"listed after create=%t, after delete=%t, after re-post with deleted:false=%t; re-post %s",
		created != nil,
		afterDelete != nil,
		afterRestore != nil,
		describeOutcome(res),
	)
	return nil
}

func (c *checker) checkMeals(ctx context.Context) error {
	fmt.Fprintln(c.out, "\n## Meal checks")
	types, err := c.readList(ctx, "mealtypes")
	if err != nil {
		return err
	}
	typeUID, mealType, err := firstUID(types, "meal types")
	if err != nil {
		return err
	}
	uid, err := c.own("meals")
	if err != nil {
		return err
	}
	meal := object{
		"uid":        uid,
		"deleted":    false,
		"order_flag": 0,
		"name":       testPrefix + " meal",
		"date":       farFutureDate,
		"type":       mealType["original_type"],
		"recipe_uid": nil,
		"type_uid":   typeUID,
	}
	return c.checkRestore(ctx, "meals", "W6 (meal)", meal)
}

func (c *checker) checkRecipe(ctx context.Context) error {
	fmt.Fprintln(c.out, "\n## Recipe checks")
	uid, err := c.own("recipe")
	if err != nil {
		return err
	}
	base := object{
		"uid":              uid,
		"deleted":          false,
		"created":          time.Now().UTC().Format("2006-01-02 15:04:05"),
		"hash":             newHash(),
		"name":             testPrefix + " recipe",
		"description":      "",
		"ingredients":      "",
		"directions":       "",
		"notes":            "",
		"nutritional_info": "",
		"prep_time":        "",
		"cook_time":        "",
		"total_time":       "",
		"difficulty":       "",
		"servings":         "",
		"rating":           0,
		"scale":            nil,
		"source":           "",
		"source_url":       "",
		"on_favorites":     false,
		"is_pinned":        false,
		"in_trash":         true,
		"photo":            nil,
		"photo_large":      nil,
		"photo_hash":       nil,
		"image_url":        nil,
		"categories":       []string{},
	}
	res, err := c.postRecipe(ctx, base)
	if err != nil {
		return err
	}
	stored, err := c.readRecipe(ctx, uid)
	if err != nil {
		return err
	}
	c.report(
		"Recipe create",
		"upload %s; readable=%t, in_trash=%s",
		describeOutcome(res),
		stored != nil,
		describeField(stored, "in_trash"),
	)
	if !accepted(res) || stored == nil {
		return fmt.Errorf("%w: baseline recipe write failed", errUnexpected)
	}

	newer := maps.Clone(base)
	newer["hash"], newer["servings_min"], newer["servings_max"], newer["prep_minutes"] = newHash(), 2, 4, 5
	if res, err = c.postRecipe(ctx, newer); err != nil {
		return err
	}
	if stored, err = c.readRecipe(ctx, uid); err != nil {
		return err
	}
	c.report(
		"W3 (send newer fields)",
		"upload %s; servings_min=%s servings_max=%s prep_minutes=%s",
		describeOutcome(res),
		describeField(stored, "servings_min"),
		describeField(stored, "servings_max"),
		describeField(stored, "prep_minutes"),
	)

	omitted := maps.Clone(base)
	omitted["hash"] = newHash()
	if res, err = c.postRecipe(ctx, omitted); err != nil {
		return err
	}
	if stored, err = c.readRecipe(ctx, uid); err != nil {
		return err
	}
	c.report(
		"W3 (omit newer fields)",
		"upload %s; servings_min=%s servings_max=%s prep_minutes=%s",
		describeOutcome(res),
		describeField(stored, "servings_min"),
		describeField(stored, "servings_max"),
		describeField(stored, "prep_minutes"),
	)

	const lineBreaks = "LF one\nLF two\r\nCRLF three"
	breaks := maps.Clone(stored)
	breaks["hash"], breaks["directions"] = newHash(), lineBreaks
	if res, err = c.postRecipe(ctx, breaks); err != nil {
		return err
	}
	if stored, err = c.readRecipe(ctx, uid); err != nil {
		return err
	}
	c.report(
		"W7 (line breaks)",
		"upload %s; stored unchanged=%t; check display in the app",
		describeOutcome(res),
		stored != nil && stored["directions"] == lineBreaks,
	)

	lowerHash := strings.ToLower(newHash())
	lower := maps.Clone(stored)
	lower["hash"] = lowerHash
	if res, err = c.postRecipe(ctx, lower); err != nil {
		return err
	}
	if stored, err = c.readRecipe(ctx, uid); err != nil {
		return err
	}
	c.report(
		"Hash case",
		"lowercase upload %s; stored as sent=%t",
		describeOutcome(res),
		stored != nil && stored["hash"] == lowerHash,
	)

	staleBase := maps.Clone(stored)
	second := maps.Clone(stored)
	secondHash := newHash()
	second["hash"], second["description"] = secondHash, "second write"
	if _, err = c.postRecipe(ctx, second); err != nil {
		return err
	}
	stale := maps.Clone(staleBase)
	stale["description"] = "stale write"
	if res, err = c.postRecipe(ctx, stale); err != nil {
		return err
	}
	if stored, err = c.readRecipe(ctx, uid); err != nil {
		return err
	}
	c.report(
		"W4 (stale write)",
		"upload carrying the old hash %s; description=%s, old hash stored=%t",
		describeOutcome(res),
		describeField(stored, "description"),
		stored != nil && stored["hash"] == staleBase["hash"],
	)

	sameHash := maps.Clone(stored)
	sameHash["description"] = "edit with the same hash"
	if res, err = c.postRecipe(ctx, sameHash); err != nil {
		return err
	}
	after, err := c.readRecipe(ctx, uid)
	if err != nil {
		return err
	}
	c.report(
		"Write without a new hash",
		"upload %s; description=%s, hash unchanged=%t; check whether the app shows it",
		describeOutcome(res),
		describeField(after, "description"),
		after != nil && after["hash"] == stored["hash"],
	)
	return nil
}

func (c *checker) notify(ctx context.Context, label string) error {
	res, err := c.post(ctx, syncPrefix+"notify/", nil)
	if err != nil {
		return err
	}
	c.report(label, "notify %s", describeOutcome(res))
	return nil
}

func (c *checker) reportRememberedIngredients(ctx context.Context) error {
	objects, err := c.readList(ctx, "groceryingredients")
	if err != nil {
		return err
	}
	test := 0
	for _, obj := range objects {
		if name, ok := obj["name"].(string); ok && strings.Contains(
			strings.ToUpper(name),
			testPrefix,
		) {
			test++
		}
	}
	c.report("Remembered ingredients", "%d total, %d named like test objects", len(objects), test)
	return nil
}

// login and the guards run before anything else, so a refused run sends no writes.
func prepareWrites(
	ctx context.Context,
	p *prober,
	password string,
	opts writeOptions,
) (*checker, error) {
	if err := checkConfirmation(opts); err != nil {
		return nil, err
	}
	if err := p.login(ctx, opts.email, password); err != nil {
		return nil, err
	}
	c := &checker{p: p, out: p.out, statePath: opts.statePath}
	recipes, err := c.readList(ctx, "recipes")
	if err != nil {
		return nil, err
	}
	if len(recipes) > maxUnconfirmedLib && !opts.allowRealAcct {
		return nil, fmt.Errorf(
			"%w: account has %d recipes, more than %d, so -allow-real-account is required",
			errGuard,
			len(recipes),
			maxUnconfirmedLib,
		)
	}
	return c, nil
}

// runWriteChecks backs up the account, then runs every check once, in order, and
// stops at the first unexpected response. Test objects stay until runCleanup, so
// that their results can be confirmed in the app.
func runWriteChecks(ctx context.Context, p *prober, password string, opts writeOptions) error {
	if err := requireLocalPath("-backup", opts.backupPath); err != nil {
		return err
	}
	c, err := prepareWrites(ctx, p, password, opts)
	if err != nil {
		return err
	}
	if _, err := os.Stat(opts.statePath); err == nil {
		return fmt.Errorf("%w: %s exists, so run -cleanup first", errGuard, opts.statePath)
	}

	fmt.Fprintf(
		c.out,
		"# Paprika write checks\n\nGenerated %s. User-Agent %q.\n\n## Backup\n\n",
		time.Now().UTC().Format(time.RFC3339),
		userAgent,
	)
	if err := c.saveBackup(ctx, opts.backupPath); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	if err := c.reportReadOnly(ctx); err != nil {
		return err
	}
	if err := c.reportCounters(ctx, "before"); err != nil {
		return err
	}
	if err := c.reportRememberedIngredients(ctx); err != nil {
		return err
	}
	for _, check := range []func(context.Context) error{
		c.checkGroceries,
		c.checkMeals,
		c.checkRecipe,
	} {
		if err := check(ctx); err != nil {
			fmt.Fprintf(c.out, "\n**Stopped:** %v\n", err)
			return err
		}
	}
	fmt.Fprintln(c.out, "\n## Finish")
	if err := c.notify(ctx, "W8 (notify)"); err != nil {
		return err
	}
	if err := c.reportCounters(ctx, "after"); err != nil {
		return err
	}
	return c.reportRememberedIngredients(ctx)
}

func knownUIDs(objects []object) map[any]bool {
	known := make(map[any]bool, len(objects))
	for _, obj := range objects {
		known[obj["uid"]] = true
	}
	return known
}

// runInspect reports how the objects in the state file look now, using reads only,
// to show what a client app changed after syncing them.
func runInspect(ctx context.Context, p *prober, email, password, statePath string) error {
	if err := p.login(ctx, email, password); err != nil {
		return err
	}
	c := &checker{p: p, out: p.out, statePath: statePath}
	data, err := os.ReadFile(statePath)
	if err != nil {
		return fmt.Errorf("read state: %w", err)
	}
	if err := json.Unmarshal(data, &c.objects); err != nil {
		return fmt.Errorf("parse state: %w", err)
	}

	aisles, err := c.readList(ctx, "groceryaisles")
	if err != nil {
		return err
	}
	lists, err := c.readList(ctx, "grocerylists")
	if err != nil {
		return err
	}
	groceries, err := c.readList(ctx, "groceries")
	if err != nil {
		return err
	}
	meals, err := c.readList(ctx, "meals")
	if err != nil {
		return err
	}
	knownAisles, knownLists := knownUIDs(aisles), knownUIDs(lists)
	listed := map[string]object{}
	for _, obj := range slices.Concat(groceries, meals, lists) {
		if uid, ok := obj["uid"].(string); ok {
			listed[uid] = obj
		}
	}

	fmt.Fprintln(c.out, "# Paprika write check inspection")
	c.report(
		"Counts",
		"%d aisles, %d lists, %d grocery items, %d meals",
		len(aisles),
		len(lists),
		len(groceries),
		len(meals),
	)
	for _, entry := range c.objects {
		if entry.Resource == "recipe" {
			recipe, err := c.readRecipe(ctx, entry.UID)
			if err != nil {
				return err
			}
			c.report(
				"Recipe",
				"in_trash=%s description=%s directions=%s",
				describeField(recipe, "in_trash"),
				describeField(recipe, "description"),
				describeField(recipe, "directions"),
			)
			continue
		}
		obj, ok := listed[entry.UID]
		if !ok {
			c.report(entry.Resource, "object %s is no longer listed", entry.UID)
			continue
		}
		switch entry.Resource {
		case "groceries":
			c.report(
				"Item",
				"%v: aisle=%s, aisle_uid known=%t, empty=%t, list_uid known=%t",
				obj["name"],
				describeField(obj, "aisle"),
				knownAisles[obj["aisle_uid"]],
				obj["aisle_uid"] == "",
				knownLists[obj["list_uid"]],
			)
		default:
			c.report(entry.Resource, "%v: listed, date=%s", obj["name"], describeField(obj, "date"))
		}
	}
	return nil
}

// runFollowup answers questions that need a device which has already synced the test
// objects. Stage 1 edits the recipe without a new hash, deletes one item, and moves the
// meal to tomorrow. After a device has synced that, stage 2 restores the item and makes
// a recipe edit with a new hash.
func runFollowup(
	ctx context.Context,
	p *prober,
	password string,
	opts writeOptions,
	stage int,
) error {
	if stage != 1 && stage != 2 {
		return fmt.Errorf("%w: -followup must be 1 or 2", errGuard)
	}
	c, err := prepareWrites(ctx, p, password, opts)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(opts.statePath)
	if err != nil {
		return fmt.Errorf("read state: %w", err)
	}
	if err := json.Unmarshal(data, &c.objects); err != nil {
		return fmt.Errorf("parse state: %w", err)
	}

	var recipeUID string
	for _, entry := range c.objects {
		if entry.Resource == "recipe" {
			recipeUID = entry.UID
		}
	}
	recipe, err := c.readRecipe(ctx, recipeUID)
	if err != nil {
		return err
	}
	if recipe == nil {
		return fmt.Errorf("%w: test recipe not found", errUnexpected)
	}
	deletedItemPath := opts.statePath + ".item.local"

	fmt.Fprintf(c.out, "# Paprika write check follow-up, stage %d\n", stage)
	if stage == 1 {
		recipe["description"] = "same hash second edit"
		res, err := c.postRecipe(ctx, recipe)
		if err != nil {
			return err
		}
		c.report("Recipe, same hash", "upload %s", describeOutcome(res))

		item, err := c.findOwnedByName(ctx, "groceries", testPrefix+" W6 restore")
		if err != nil {
			return err
		}
		saved, err := json.Marshal(item)
		if err != nil {
			return err
		}
		if err := os.WriteFile(deletedItemPath, saved, 0o600); err != nil {
			return err
		}
		removed := maps.Clone(item)
		removed["deleted"] = true
		if res, err = c.postList(ctx, "groceries", removed); err != nil {
			return err
		}
		c.report("Item delete", "upload %s", describeOutcome(res))

		meal, err := c.findOwnedByName(ctx, "meals", testPrefix+" meal")
		if err != nil {
			return err
		}
		date := time.Now().AddDate(0, 0, 1).Format("2006-01-02") + " 00:00:00"
		meal["date"] = date
		if res, err = c.postList(ctx, "meals", meal); err != nil {
			return err
		}
		c.report("Meal move", "upload %s; new date %s", describeOutcome(res), date)
		return c.notify(ctx, "Notify")
	}

	saved, err := os.ReadFile(deletedItemPath)
	if err != nil {
		return fmt.Errorf("read deleted item: %w", err)
	}
	var item object
	if err := json.Unmarshal(saved, &item); err != nil {
		return err
	}
	item["deleted"] = false
	res, err := c.postList(ctx, "groceries", item)
	if err != nil {
		return err
	}
	c.report("Item restore", "upload %s", describeOutcome(res))

	recipe["hash"], recipe["description"] = newHash(), "new hash control edit"
	if res, err = c.postRecipe(ctx, recipe); err != nil {
		return err
	}
	c.report("Recipe, new hash", "upload %s", describeOutcome(res))
	if err := c.notify(ctx, "Notify"); err != nil {
		return err
	}
	return os.Remove(deletedItemPath)
}

func (c *checker) findOwnedByName(ctx context.Context, resource, name string) (object, error) {
	objects, err := c.readList(ctx, resource)
	if err != nil {
		return nil, err
	}
	for _, obj := range objects {
		uid, _ := obj["uid"].(string)
		if obj["name"] == name && c.owns(resource, uid) {
			return obj, nil
		}
	}
	return nil, fmt.Errorf("%w: %s not found in %s", errUnexpected, name, resource)
}

// runCleanup removes what the state file names, after confirming from a fresh read
// that each object is still a test object. The recipe stays in the trash for the
// account owner to empty.
func runCleanup(ctx context.Context, p *prober, password string, opts writeOptions) error {
	c, err := prepareWrites(ctx, p, password, opts)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(opts.statePath)
	if err != nil {
		return fmt.Errorf("read state: %w", err)
	}
	if err := json.Unmarshal(data, &c.objects); err != nil {
		return fmt.Errorf("parse state: %w", err)
	}

	fmt.Fprintln(c.out, "# Paprika write check cleanup")
	for _, resource := range []string{"groceries", "meals", "grocerylists"} {
		current, err := c.readList(ctx, resource)
		if err != nil {
			return err
		}
		removed := 0
		for _, obj := range current {
			uid, _ := obj["uid"].(string)
			if !c.owns(resource, uid) {
				continue
			}
			gone := maps.Clone(obj)
			gone["deleted"] = true
			res, err := c.postList(ctx, resource, gone)
			if err != nil {
				return err
			}
			if !accepted(res) {
				return fmt.Errorf(
					"%w: cleanup of %s: %s",
					errUnexpected,
					resource,
					describeOutcome(res),
				)
			}
			removed++
		}
		c.report("Removed", "%d from `%s`", removed, resource)
	}
	for _, entry := range c.objects {
		if entry.Resource != "recipe" {
			continue
		}
		recipe, err := c.readRecipe(ctx, entry.UID)
		if err != nil {
			return err
		}
		c.report(
			"Recipe",
			"left for the owner to empty from the trash; in_trash=%s",
			describeField(recipe, "in_trash"),
		)
	}
	if err := c.notify(ctx, "Notify"); err != nil {
		return err
	}
	if err := c.reportRememberedIngredients(ctx); err != nil {
		return err
	}
	return os.Remove(opts.statePath)
}

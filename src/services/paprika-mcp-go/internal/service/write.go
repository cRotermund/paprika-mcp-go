package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/paprikaclient"
)

// MaxBatch is the most items one batch write accepts.
const MaxBatch = 50

const (
	maxNameLen  = 500
	maxShortLen = 500
	maxTextLen  = 100_000
	notifyWait  = 45 * time.Second
)

// Writer is the part of paprikaclient.Client that the write path uses. A
// *paprikaclient.Client satisfies it. Writes read from the API, never from the
// cache, so that they start from the latest object.
type Writer interface {
	GetRecipe(ctx context.Context, uid string) (paprikaclient.Entry[paprikaclient.Recipe], error)
	ListRecipes(ctx context.Context) ([]paprikaclient.RecipeRef, error)
	Categories(ctx context.Context) ([]paprikaclient.Entry[paprikaclient.Category], error)
	Meals(ctx context.Context) ([]paprikaclient.Entry[paprikaclient.Meal], error)
	MealTypes(ctx context.Context) ([]paprikaclient.Entry[paprikaclient.MealType], error)
	GroceryLists(ctx context.Context) ([]paprikaclient.Entry[paprikaclient.GroceryList], error)
	Groceries(ctx context.Context) ([]paprikaclient.Entry[paprikaclient.Grocery], error)
	GroceryAisles(ctx context.Context) ([]paprikaclient.Entry[paprikaclient.GroceryAisle], error)
	GroceryIngredients(ctx context.Context) (
		[]paprikaclient.Entry[paprikaclient.GroceryIngredient],
		error,
	)
	SaveRecipe(ctx context.Context, uid string, recipe paprikaclient.Object) error
	SaveMeals(ctx context.Context, meals []paprikaclient.Object) error
	SaveGroceryItems(ctx context.Context, items []paprikaclient.Object) error
	SaveGroceryLists(ctx context.Context, lists []paprikaclient.Object) error
	Notify(ctx context.Context) error
}

// Audit resource names.
const (
	resRecipe      = "recipe"
	resMeal        = "meal"
	resGroceryItem = "grocery_item"
	resGroceryList = "grocery_list"
)

// beginWrite takes the process-wide write lock. The caller must call the
// returned function when the whole write is done.
func (s *Service) beginWrite(ctx context.Context) (func(), error) {
	if s.w == nil {
		return nil, &Error{Kind: KindInternal, Message: "Writes are not available on this server."}
	}
	select {
	case s.wlock <- struct{}{}:
		return func() { <-s.wlock }, nil
	case <-ctx.Done():
		return nil, s.mapError("write_lock", ctx.Err())
	}
}

// change names one object a write touches and the fields it changes.
type change struct {
	uid    string
	fields []string
}

// commit sends the write with post, then asks Paprika to notify other
// devices, and writes the audit entries. A failed notify is not a failed write:
// it returns a warning. A POST is never retried; a timeout is an unknown
// outcome.
func (s *Service) commit(
	ctx context.Context,
	tool, resource string,
	changes []change,
	post func(context.Context) error,
) (string, error) {
	if err := post(ctx); err != nil {
		outcome := "failed"
		if errors.Is(err, paprikaclient.ErrOutcomeUnknown) {
			outcome = "outcome_unknown"
		}
		s.audit(tool, resource, changes, outcome)
		return "", s.mapWriteError(tool, err)
	}
	// The change is saved. Notify even if the caller has gone away.
	nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), notifyWait)
	defer cancel()
	if err := s.w.Notify(nctx); err != nil {
		s.log.Warn("saved, but could not notify other devices", "tool", tool, "error", err)
		s.audit(tool, resource, changes, "ok_notify_failed")
		return "The change was saved, but Paprika could not be asked to tell other devices to sync, so they may show it only after their next sync.", nil
	}
	s.audit(tool, resource, changes, "ok")
	return "", nil
}

// audit logs one entry per object: the tool, resource, uid, the names of the
// fields changed, and the outcome. It never logs values.
func (s *Service) audit(tool, resource string, changes []change, outcome string) {
	for _, c := range changes {
		s.log.Info(
			"write",
			"audit",
			true,
			"tool",
			tool,
			"resource",
			resource,
			"uid",
			c.uid,
			"fields",
			strings.Join(c.fields, ","),
			"outcome",
			outcome,
		)
	}
}

// mapWriteError is mapError for a failed POST: an unknown outcome is reported
// as such, whatever else the error says.
func (s *Service) mapWriteError(op string, err error) error {
	if errors.Is(err, paprikaclient.ErrOutcomeUnknown) {
		s.log.Warn("write outcome unknown", "op", op, "error", err)
		return &Error{
			Kind:    KindOutcomeUnknown,
			cause:   err,
			Message: "Paprika did not confirm the write, so it may or may not have been applied. Do not repeat it yet: read the object again (get_recipe, list_meals, or list_grocery_items) and retry only if the change is missing.",
		}
	}
	return s.mapError(op, err)
}

func conflict(format string, args ...any) *Error {
	return &Error{Kind: KindConflict, Message: fmt.Sprintf(format, args...)}
}

// newUID returns an uppercase version 4 UUID, as the official client does.
func newUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%X-%X-%X-%X-%X", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// newHash returns a new recipe change marker: the SHA-256 of a random UUID,
// written as uppercase hex, which is how the official client makes one.
func newHash() string {
	sum := sha256.Sum256([]byte(strings.ToLower(newUID())))
	return fmt.Sprintf("%X", sum)
}

// setField stores v in the field unless it already holds an equal value, and
// reports whether the object changed.
func setField(o paprikaclient.Object, key string, v any) bool {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("service: cannot encode %q: %v", key, err))
	}
	if cur, ok := o[key]; ok && jsonEqual(cur, raw) {
		return false
	}
	o[key] = raw
	return true
}

func jsonEqual(a, b []byte) bool {
	if bytes.Equal(a, b) {
		return true
	}
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// mustSet stores v in the field. The values passed are always encodable.
func mustSet(o paprikaclient.Object, key string, v any) {
	if err := o.Set(key, v); err != nil {
		panic(fmt.Sprintf("service: cannot encode %q: %v", key, err))
	}
}

// normalizeNewlines turns \r\n and \r into \n, which both display as line
// breaks (check W7) and keeps one form in stored text.
func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// ObjectMap returns an object's fields as plain values, for results.
func ObjectMap(o paprikaclient.Object) map[string]any {
	out := make(map[string]any, len(o))
	for k, v := range o {
		var x any
		if json.Unmarshal(v, &x) == nil {
			out[k] = x
		}
	}
	return out
}

// requireText checks a required text input and returns it trimmed.
func requireText(field, value string, max int) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", invalid("%s is required.", field)
	}
	if err := checkText(field, value, max); err != nil {
		return "", err
	}
	return value, nil
}

// checkBatch checks the size of a batch input.
func checkBatch(field string, n int) error {
	if n == 0 {
		return invalid("%s must hold at least one item.", field)
	}
	if n > MaxBatch {
		return invalid("%s holds %d items; send at most %d per call.", field, n, MaxBatch)
	}
	return nil
}

// ItemError reports one item of a batch that was not written.
type ItemError struct {
	Index   int
	Message string
}

// batchFailure is the error for a batch in which no item could be written.
func batchFailure(errs []ItemError) *Error {
	var b strings.Builder
	b.WriteString("Nothing was written; every item failed.")
	for _, e := range errs {
		fmt.Fprintf(&b, " Item %d: %s", e.Index, e.Message)
	}
	return &Error{Kind: KindInvalid, Message: b.String()}
}

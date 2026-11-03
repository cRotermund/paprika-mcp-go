package paprikaclient

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
)

// Status returns the change counters from sync/status/.
func (c *Client) Status(ctx context.Context) (Counters, error) {
	raw, err := c.get(ctx, syncPrefix+"status/")
	if err != nil {
		return nil, err
	}
	var counters Counters
	if err := json.Unmarshal(raw, &counters); err != nil {
		return nil, fmt.Errorf("%w: status: %w", ErrUnexpectedResponse, err)
	}
	return counters, nil
}

// ListRecipes returns the uid and hash of every recipe, trashed ones included.
func (c *Client) ListRecipes(ctx context.Context) ([]RecipeRef, error) {
	raw, err := c.get(ctx, syncPrefix+"recipes/")
	if err != nil {
		return nil, err
	}
	var refs []RecipeRef
	if err := json.Unmarshal(raw, &refs); err != nil {
		return nil, fmt.Errorf("%w: recipes: %w", ErrUnexpectedResponse, err)
	}
	return refs, nil
}

// GetRecipe returns one complete recipe. A missing recipe is [ErrNotFound].
func (c *Client) GetRecipe(ctx context.Context, uid string) (Entry[Recipe], error) {
	if uid == "" {
		return Entry[Recipe]{}, fmt.Errorf("%w: empty recipe uid", ErrInvalidArgument)
	}
	raw, err := c.get(ctx, recipePath(uid))
	if err != nil {
		return Entry[Recipe]{}, recipeReadError(err)
	}
	return decodeEntry[Recipe](raw)
}

// Categories returns every recipe category.
func (c *Client) Categories(ctx context.Context) ([]Entry[Category], error) {
	return listEntries[Category](ctx, c, "categories")
}

// Meals returns every planned meal. The API has no date filter.
func (c *Client) Meals(ctx context.Context) ([]Entry[Meal], error) {
	return listEntries[Meal](ctx, c, "meals")
}

// MealTypes returns the account's meal types.
func (c *Client) MealTypes(ctx context.Context) ([]Entry[MealType], error) {
	return listEntries[MealType](ctx, c, "mealtypes")
}

// GroceryLists returns every grocery list.
func (c *Client) GroceryLists(ctx context.Context) ([]Entry[GroceryList], error) {
	return listEntries[GroceryList](ctx, c, "grocerylists")
}

// Groceries returns every grocery item on every list.
func (c *Client) Groceries(ctx context.Context) ([]Entry[Grocery], error) {
	return listEntries[Grocery](ctx, c, "groceries")
}

// GroceryAisles returns every store aisle.
func (c *Client) GroceryAisles(ctx context.Context) ([]Entry[GroceryAisle], error) {
	return listEntries[GroceryAisle](ctx, c, "groceryaisles")
}

// GroceryIngredients returns the remembered ingredient-to-aisle choices.
func (c *Client) GroceryIngredients(ctx context.Context) ([]Entry[GroceryIngredient], error) {
	return listEntries[GroceryIngredient](ctx, c, "groceryingredients")
}

// SaveRecipe writes one whole recipe. The caller sets a new "hash" first, or
// devices that already hold the recipe will ignore the change. If the object has
// a "uid" it must equal uid.
func (c *Client) SaveRecipe(ctx context.Context, uid string, recipe Object) error {
	if uid == "" {
		return fmt.Errorf("%w: empty recipe uid", ErrInvalidArgument)
	}
	if got := recipe.UID(); got != "" && got != uid {
		return fmt.Errorf("%w: recipe uid does not match the route", ErrInvalidArgument)
	}
	return c.write(ctx, recipePath(uid), recipe)
}

// SaveMeals writes whole meals, adding or updating by uid. A delete is an
// object with "deleted": true. A timeout is [ErrOutcomeUnknown].
func (c *Client) SaveMeals(ctx context.Context, meals []Object) error {
	return c.saveBulk(ctx, "meals", meals)
}

// SaveGroceryItems writes whole grocery items, adding or updating by uid.
func (c *Client) SaveGroceryItems(ctx context.Context, items []Object) error {
	return c.saveBulk(ctx, "groceries", items)
}

// SaveGroceryLists writes whole grocery lists, adding or updating by uid.
func (c *Client) SaveGroceryLists(ctx context.Context, lists []Object) error {
	return c.saveBulk(ctx, "grocerylists", lists)
}

// Notify asks Paprika to tell other devices to sync. It has no body.
func (c *Client) Notify(ctx context.Context) error {
	raw, err := c.do(ctx, http.MethodPost, syncPrefix+"notify/", nil, "")
	if err != nil {
		return err
	}
	return expectTrue(raw)
}

func recipePath(uid string) string {
	return syncPrefix + "recipe/" + url.PathEscape(uid) + "/"
}

func (c *Client) get(ctx context.Context, path string) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, path, nil, "")
}

func listEntries[T any](ctx context.Context, c *Client, resource string) ([]Entry[T], error) {
	raw, err := c.get(ctx, syncPrefix+resource+"/")
	if err != nil {
		return nil, err
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrUnexpectedResponse, resource, err)
	}
	entries := make([]Entry[T], 0, len(items))
	for _, item := range items {
		entry, err := decodeEntry[T](item)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", resource, err)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func (c *Client) saveBulk(ctx context.Context, resource string, objects []Object) error {
	if len(objects) == 0 {
		return nil
	}
	for _, o := range objects {
		if o.UID() == "" {
			return fmt.Errorf("%w: every %s object needs a uid", ErrInvalidArgument, resource)
		}
	}
	return c.write(ctx, syncPrefix+resource+"/", objects)
}

// write sends a payload as a gzipped JSON multipart upload and expects true.
func (c *Client) write(ctx context.Context, path string, payload any) error {
	body, contentType, err := encodeUpload(payload)
	if err != nil {
		return fmt.Errorf("%w: encode upload: %w", ErrInvalidArgument, err)
	}
	raw, err := c.do(ctx, http.MethodPost, path, body, contentType)
	if err != nil {
		return err
	}
	return expectTrue(raw)
}

func expectTrue(raw json.RawMessage) error {
	if !bytes.Equal(bytes.TrimSpace(raw), []byte("true")) {
		return fmt.Errorf(
			"%w: write was not confirmed: %w",
			ErrUnexpectedResponse,
			ErrOutcomeUnknown,
		)
	}
	return nil
}

// encodeUpload builds the multipart body Paprika expects: the JSON, gzipped, in
// a file part named "data".
func encodeUpload(payload any) ([]byte, string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, "", err
	}
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write(raw); err != nil {
		return nil, "", err
	}
	if err := zw.Close(); err != nil {
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
	return body.Bytes(), form.FormDataContentType(), nil
}

// recipeReadError maps the live server's answer for an unknown recipe to
// [ErrNotFound]. For GET sync/recipe/{uid}/ it replies HTTP 200 with an error
// envelope whose message is "Recipe not found." (code 0), not a 404. The match
// is deliberately limited to this one read; other routes keep their errors.
func recipeReadError(err error) error {
	var api *APIError
	if errors.As(err, &api) && strings.Contains(strings.ToLower(api.Message), "not found") {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	return err
}

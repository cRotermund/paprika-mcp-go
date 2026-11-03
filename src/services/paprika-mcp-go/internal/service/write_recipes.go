package service

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/paprikaclient"
)

const maxCategoriesPerRecipe = 50

// RecipeFields holds recipe fields to set. A nil field is left alone (on
// create it takes its default).
type RecipeFields struct {
	Name            *string
	Description     *string
	Ingredients     *string
	Directions      *string
	Notes           *string
	NutritionalInfo *string
	Source          *string
	SourceURL       *string
	Servings        *string
	PrepTime        *string
	CookTime        *string
	TotalTime       *string
	Difficulty      *string
	Rating          *int
	Favorite        *bool
	// Categories is the complete set of categories, as names or uids. Nil
	// leaves them alone; an empty slice removes them all.
	Categories *[]string
}

type textField struct {
	key       string
	val       *string
	max       int
	multiline bool
}

func (f RecipeFields) texts() []textField {
	return []textField{
		{"name", f.Name, maxNameLen, false},
		{"description", f.Description, maxTextLen, true},
		{"ingredients", f.Ingredients, maxTextLen, true},
		{"directions", f.Directions, maxTextLen, true},
		{"notes", f.Notes, maxTextLen, true},
		{"nutritional_info", f.NutritionalInfo, maxTextLen, true},
		{"source", f.Source, maxShortLen, false},
		{"source_url", f.SourceURL, maxShortLen, false},
		{"servings", f.Servings, maxShortLen, false},
		{"prep_time", f.PrepTime, maxShortLen, false},
		{"cook_time", f.CookTime, maxShortLen, false},
		{"total_time", f.TotalTime, maxShortLen, false},
		{"difficulty", f.Difficulty, maxShortLen, false},
	}
}

// any reports whether at least one field is set.
func (f RecipeFields) any() bool {
	for _, t := range f.texts() {
		if t.val != nil {
			return true
		}
	}
	return f.Rating != nil || f.Favorite != nil || f.Categories != nil
}

func (f RecipeFields) validate(requireName bool) error {
	if f.Name == nil && requireName {
		return invalid("name is required.")
	}
	if f.Name != nil && strings.TrimSpace(*f.Name) == "" {
		return invalid("name must not be blank.")
	}
	for _, t := range f.texts() {
		if t.val != nil {
			if err := checkText(t.key, *t.val, t.max); err != nil {
				return err
			}
		}
	}
	if f.Rating != nil && (*f.Rating < 0 || *f.Rating > 5) {
		return invalid("rating must be between 0 and 5.")
	}
	if f.Categories != nil {
		if len(*f.Categories) > maxCategoriesPerRecipe {
			return invalid("categories holds %d entries; use at most %d.", len(*f.Categories), maxCategoriesPerRecipe)
		}
		for _, c := range *f.Categories {
			if strings.TrimSpace(c) == "" {
				return invalid("categories must not contain a blank entry.")
			}
			if err := checkText("category", c, maxRefLen); err != nil {
				return err
			}
		}
	}
	return nil
}

// apply sets the given fields on the raw recipe and returns the names of those
// that changed. catUIDs is the resolved Categories. Fields not named, such as
// photos and anything this server does not model, are not touched.
func (f RecipeFields) apply(o paprikaclient.Object, catUIDs []string) []string {
	var changed []string
	for _, t := range f.texts() {
		if t.val == nil {
			continue
		}
		v := *t.val
		if t.multiline {
			v = normalizeNewlines(v)
		}
		if t.key == "name" {
			v = strings.TrimSpace(v)
		}
		if setField(o, t.key, v) {
			changed = append(changed, t.key)
		}
	}
	if f.Rating != nil && setField(o, "rating", *f.Rating) {
		changed = append(changed, "rating")
	}
	if f.Favorite != nil && setField(o, "on_favorites", *f.Favorite) {
		changed = append(changed, "on_favorites")
	}
	if f.Categories != nil && setField(o, "categories", nonNil(catUIDs)) {
		changed = append(changed, "categories")
	}
	return changed
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// resolveCategories turns names or uids into uids against the current
// categories, dropping repeats. The first unknown one is an error naming it.
func (s *Service) resolveCategories(ctx context.Context, refs *[]string) ([]string, error) {
	if refs == nil {
		return nil, nil
	}
	cats, err := s.w.Categories(ctx)
	if err != nil {
		return nil, s.mapError("write_recipe", err)
	}
	seen := map[string]bool{}
	uids := []string{}
	for _, ref := range *refs {
		uid, err := resolveCategoryIn(cats, ref)
		if err != nil {
			return nil, err
		}
		if !seen[uid] {
			seen[uid] = true
			uids = append(uids, uid)
		}
	}
	return uids, nil
}

// RecipeWrite is the result of a recipe write.
type RecipeWrite struct {
	Recipe RecipeDetail
	// Changed names the fields that changed. Empty when nothing needed to change.
	Changed []string
	// NoChange is true when the recipe already matched, so nothing was sent.
	NoChange bool
	// Warning is set when the write succeeded but something else did not.
	Warning string
}

// CreateRecipe adds a recipe.
func (s *Service) CreateRecipe(ctx context.Context, f RecipeFields) (RecipeWrite, error) {
	const tool = "create_recipe"
	if err := f.validate(true); err != nil {
		return RecipeWrite{}, err
	}
	unlock, err := s.beginWrite(ctx)
	if err != nil {
		return RecipeWrite{}, err
	}
	defer unlock()

	catUIDs, err := s.resolveCategories(ctx, f.Categories)
	if err != nil {
		return RecipeWrite{}, err
	}

	// The shape the official client sends for a new recipe without a photo.
	uid := newUID()
	o := paprikaclient.Object{}
	mustSet(o, "uid", uid)
	mustSet(o, "created", s.now().UTC().Format("2006-01-02 15:04:05"))
	for _, k := range []string{"description", "ingredients", "directions", "notes", "nutritional_info", "prep_time",
		"cook_time", "total_time", "difficulty", "servings", "source", "source_url"} {
		mustSet(o, k, "")
	}
	mustSet(o, "rating", 0)
	mustSet(o, "scale", nil)
	mustSet(o, "on_favorites", false)
	mustSet(o, "is_pinned", false)
	mustSet(o, "in_trash", false)
	for _, k := range []string{"photo", "photo_large", "photo_hash", "image_url"} {
		mustSet(o, k, nil)
	}
	mustSet(o, "categories", []string{})
	changed := f.apply(o, catUIDs)

	return s.saveRecipe(ctx, tool, uid, o, changed)
}

// UpdateRecipe changes the given fields of a recipe. A non-empty expectedHash
// must equal the recipe's current hash, or the update is refused as a conflict.
func (s *Service) UpdateRecipe(ctx context.Context, uid string, f RecipeFields, expectedHash string) (RecipeWrite, error) {
	const tool = "update_recipe"
	uid, err := requireText("uid", uid, maxRefLen)
	if err != nil {
		return RecipeWrite{}, err
	}
	if err := checkText("expected_hash", expectedHash, maxRefLen); err != nil {
		return RecipeWrite{}, err
	}
	if !f.any() {
		return RecipeWrite{}, invalid("Give at least one field to change.")
	}
	if err := f.validate(false); err != nil {
		return RecipeWrite{}, err
	}
	unlock, err := s.beginWrite(ctx)
	if err != nil {
		return RecipeWrite{}, err
	}
	defer unlock()

	o, err := s.readRecipe(ctx, uid)
	if err != nil {
		return RecipeWrite{}, err
	}
	if expectedHash != "" && o.String("hash") != expectedHash {
		s.audit(tool, resRecipe, []change{{uid: uid}}, "conflict")
		return RecipeWrite{}, conflict("The recipe changed since you read it: its hash is no longer %q. Call get_recipe, check the current text, and apply your change to that.", expectedHash)
	}
	catUIDs, err := s.resolveCategories(ctx, f.Categories)
	if err != nil {
		return RecipeWrite{}, err
	}
	changed := f.apply(o, catUIDs)
	if len(changed) == 0 {
		return s.unchangedRecipe(ctx, tool, uid, o), nil
	}
	return s.saveRecipe(ctx, tool, uid, o, changed)
}

// TrashRecipe moves a recipe to the app's trash. It is not a permanent delete.
func (s *Service) TrashRecipe(ctx context.Context, uid string) (RecipeWrite, error) {
	return s.setTrash(ctx, "trash_recipe", uid, true)
}

// RestoreRecipe takes a recipe out of the trash.
func (s *Service) RestoreRecipe(ctx context.Context, uid string) (RecipeWrite, error) {
	return s.setTrash(ctx, "restore_recipe", uid, false)
}

func (s *Service) setTrash(ctx context.Context, tool, uid string, trashed bool) (RecipeWrite, error) {
	uid, err := requireText("uid", uid, maxRefLen)
	if err != nil {
		return RecipeWrite{}, err
	}
	unlock, err := s.beginWrite(ctx)
	if err != nil {
		return RecipeWrite{}, err
	}
	defer unlock()

	o, err := s.readRecipe(ctx, uid)
	if err != nil {
		return RecipeWrite{}, err
	}
	if !setField(o, "in_trash", trashed) {
		return s.unchangedRecipe(ctx, tool, uid, o), nil
	}
	return s.saveRecipe(ctx, tool, uid, o, []string{"in_trash"})
}

// readRecipe reads the current recipe from the API, not the mirror, and
// returns a copy of its raw object.
func (s *Service) readRecipe(ctx context.Context, uid string) (paprikaclient.Object, error) {
	entry, err := s.w.GetRecipe(ctx, uid)
	if err != nil {
		if isNotFound(err) {
			return nil, notFound("No recipe has the uid %q. Use search_recipes to find a valid uid.", uid)
		}
		return nil, s.mapError("read_recipe", err)
	}
	return entry.Raw.Clone(), nil
}

func (s *Service) unchangedRecipe(ctx context.Context, tool, uid string, o paprikaclient.Object) RecipeWrite {
	s.audit(tool, resRecipe, []change{{uid: uid}}, "no_change")
	return RecipeWrite{Recipe: s.recipeDetail(ctx, recipeValue(o)), NoChange: true}
}

// saveRecipe gives the recipe a new hash, sends it, and updates the mirror.
func (s *Service) saveRecipe(ctx context.Context, tool, uid string, o paprikaclient.Object, changed []string) (RecipeWrite, error) {
	mustSet(o, "hash", newHash())
	mustSet(o, "deleted", false)
	warning, err := s.commit(ctx, tool, resRecipe, []change{{uid: uid, fields: changed}}, func(c context.Context) error {
		return s.w.SaveRecipe(c, uid, o)
	})
	if err != nil {
		return RecipeWrite{}, err
	}
	stored := o.Clone()
	delete(stored, "deleted")
	r := recipeValue(stored)
	s.m.UpsertRecipe(paprikaclient.Entry[paprikaclient.Recipe]{Raw: stored, Value: r})
	return RecipeWrite{Recipe: s.recipeDetail(ctx, r), Changed: changed, Warning: warning}, nil
}

// recipeValue decodes the typed view of a raw recipe.
func recipeValue(o paprikaclient.Object) paprikaclient.Recipe {
	var r paprikaclient.Recipe
	if raw, err := json.Marshal(o); err == nil {
		_ = json.Unmarshal(raw, &r)
	}
	return r
}

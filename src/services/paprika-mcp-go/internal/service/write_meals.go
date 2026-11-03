package service

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/mirror"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/paprikaclient"
)

// MealInput is one meal to add. Date is YYYY-MM-DD and MealType a name. At
// least one of RecipeUID and Name is required.
type MealInput struct {
	Date      string
	MealType  string
	RecipeUID string
	Name      string
}

// MealResult is the outcome of one meal in a batch. Failure is empty when the
// meal was added.
type MealResult struct {
	Index   int
	Meal    MealSummary
	Failure string
}

// AddMealsResult is the result of add_meals.
type AddMealsResult struct {
	Items   []MealResult
	Added   int
	Warning string
}

// AddMeals adds meals to the plan in one POST. Items that fail reference
// checks are reported and skipped; the rest are written.
func (s *Service) AddMeals(ctx context.Context, inputs []MealInput) (AddMealsResult, error) {
	const tool = "add_meals"
	if err := checkBatch("meals", len(inputs)); err != nil {
		return AddMealsResult{}, err
	}
	days := make([]string, len(inputs))
	for i, in := range inputs {
		t, err := parseDate("date", in.Date)
		if err != nil {
			return AddMealsResult{}, invalid("Meal %d: %s", i, err.Error())
		}
		days[i] = t.Format(dateLayout)
		if strings.TrimSpace(in.MealType) == "" {
			return AddMealsResult{}, invalid("Meal %d: meal_type is required. Call list_meal_types for the names.", i)
		}
		if strings.TrimSpace(in.RecipeUID) == "" && strings.TrimSpace(in.Name) == "" {
			return AddMealsResult{}, invalid("Meal %d: give a recipe_uid, a name, or both.", i)
		}
		for field, v := range map[string]string{"meal_type": in.MealType, "recipe_uid": in.RecipeUID, "name": in.Name} {
			if err := checkText(field, v, maxNameLen); err != nil {
				return AddMealsResult{}, invalid("Meal %d: %s", i, err.Error())
			}
		}
	}

	unlock, err := s.beginWrite(ctx)
	if err != nil {
		return AddMealsResult{}, err
	}
	defer unlock()

	types, err := s.w.MealTypes(ctx)
	if err != nil {
		return AddMealsResult{}, s.mapError(tool, err)
	}
	mts := newMealTypes(types)
	recipes, err := s.recipeLookup(ctx, tool, inputs)
	if err != nil {
		return AddMealsResult{}, err
	}

	results := make([]MealResult, len(inputs))
	var objects []paprikaclient.Object
	var changes []change
	var failures []ItemError
	perDay := map[string]int{}
	for i, in := range inputs {
		results[i].Index = i
		fail := func(msg string) {
			results[i].Failure = msg
			failures = append(failures, ItemError{Index: i, Message: msg})
		}
		mt, err := mts.byName(in.MealType)
		if err != nil {
			fail(err.Error())
			continue
		}
		recipeUID := strings.TrimSpace(in.RecipeUID)
		name := strings.TrimSpace(in.Name)
		if recipeUID != "" {
			rname, ok := recipes[recipeUID]
			if !ok {
				fail("Unknown recipe_uid " + quote(recipeUID) + ". Use search_recipes to find a valid uid.")
				continue
			}
			if name == "" {
				name = rname
			}
		}

		o := paprikaclient.Object{}
		uid := newUID()
		mustSet(o, "uid", uid)
		mustSet(o, "deleted", false)
		mustSet(o, "order_flag", perDay[days[i]])
		perDay[days[i]]++
		mustSet(o, "name", name)
		mustSet(o, "date", days[i]+" 00:00:00")
		mustSet(o, "type", mt.OriginalType)
		if recipeUID == "" {
			mustSet(o, "recipe_uid", nil)
		} else {
			mustSet(o, "recipe_uid", recipeUID)
		}
		if mt.UID == "" {
			mustSet(o, "type_uid", nil) // the account has no meal types; meals carry only the number
		} else {
			mustSet(o, "type_uid", mt.UID)
		}
		mustSet(o, "is_ingredient", false)
		mustSet(o, "scale", nil)

		objects = append(objects, o)
		changes = append(changes, change{uid: uid, fields: []string{"date", "type", "type_uid", "name", "recipe_uid"}})
		results[i].Meal = s.mealSummary(o, mts)
	}
	if len(objects) == 0 {
		return AddMealsResult{}, batchFailure(failures)
	}

	warning, err := s.commit(ctx, tool, resMeal, changes, func(c context.Context) error { return s.w.SaveMeals(c, objects) })
	if err != nil {
		return AddMealsResult{}, err
	}
	s.m.Invalidate(mirror.ResourceMeals)
	return AddMealsResult{Items: results, Added: len(objects), Warning: warning}, nil
}

// recipeLookup checks the recipe_uid values of a batch against the current
// recipe list and returns each existing one with its name. It makes no request
// when no meal links a recipe.
func (s *Service) recipeLookup(ctx context.Context, tool string, inputs []MealInput) (map[string]string, error) {
	need := map[string]bool{} // uid -> a meal that links it has no name
	for _, in := range inputs {
		if uid := strings.TrimSpace(in.RecipeUID); uid != "" {
			need[uid] = need[uid] || strings.TrimSpace(in.Name) == ""
		}
	}
	found := map[string]string{}
	if len(need) == 0 {
		return found, nil
	}
	refs, err := s.w.ListRecipes(ctx)
	if err != nil {
		return nil, s.mapError(tool, err)
	}
	exists := make(map[string]bool, len(refs))
	for _, r := range refs {
		exists[r.UID] = true
	}
	for uid, needName := range need {
		if !exists[uid] {
			continue
		}
		found[uid] = ""
		if needName {
			r, err := s.w.GetRecipe(ctx, uid)
			switch {
			case err == nil:
				found[uid] = r.Value.Name
			case isNotFound(err):
				delete(found, uid)
			default:
				return nil, s.mapError(tool, err)
			}
		}
	}
	return found, nil
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// mealSummary describes a raw meal for results.
func (s *Service) mealSummary(o paprikaclient.Object, mts mealTypes) MealSummary {
	var m paprikaclient.Meal
	if raw, err := json.Marshal(o); err == nil {
		_ = json.Unmarshal(raw, &m)
	}
	typeName, _ := mts.forMeal(m)
	sum := MealSummary{UID: m.UID, Name: m.Name, MealType: typeName}
	if len(m.Date) >= len(dateLayout) {
		sum.Date = m.Date[:len(dateLayout)]
	}
	if m.RecipeUID != nil && *m.RecipeUID != "" {
		sum.RecipeUID = *m.RecipeUID
		sum.RecipeName = s.m.RecipeName(sum.RecipeUID)
	}
	return sum
}

// MealChanges holds the changes for UpdateMeal. A nil field is left alone.
type MealChanges struct {
	Date     *string
	MealType *string
	Name     *string
	// RecipeUID links a recipe. An empty string removes the link.
	RecipeUID *string
}

// MealWrite is the result of a meal update.
type MealWrite struct {
	Meal     MealSummary
	Changed  []string
	NoChange bool
	Warning  string
}

// UpdateMeal moves, renames, re-types, or re-links a meal.
func (s *Service) UpdateMeal(ctx context.Context, uid string, c MealChanges) (MealWrite, error) {
	const tool = "update_meal"
	uid, err := requireText("uid", uid, maxRefLen)
	if err != nil {
		return MealWrite{}, err
	}
	if c.Date == nil && c.MealType == nil && c.Name == nil && c.RecipeUID == nil {
		return MealWrite{}, invalid("Give at least one of date, meal_type, name, or recipe_uid to change.")
	}
	var day string
	if c.Date != nil {
		t, err := parseDate("date", *c.Date)
		if err != nil {
			return MealWrite{}, err
		}
		day = t.Format(dateLayout)
	}
	if c.Name != nil {
		if _, err := requireText("name", *c.Name, maxNameLen); err != nil {
			return MealWrite{}, err
		}
	}
	if c.MealType != nil {
		if _, err := requireText("meal_type", *c.MealType, maxNameLen); err != nil {
			return MealWrite{}, err
		}
	}
	if c.RecipeUID != nil {
		if err := checkText("recipe_uid", *c.RecipeUID, maxRefLen); err != nil {
			return MealWrite{}, err
		}
	}

	unlock, err := s.beginWrite(ctx)
	if err != nil {
		return MealWrite{}, err
	}
	defer unlock()

	o, mts, err := s.readMeal(ctx, tool, uid)
	if err != nil {
		return MealWrite{}, err
	}
	var changed []string
	if c.Date != nil {
		cur := o.String("date")
		if len(cur) < len(dateLayout) || cur[:len(dateLayout)] != day {
			mustSet(o, "date", day+" 00:00:00")
			changed = append(changed, "date")
		}
	}
	if c.MealType != nil {
		mt, err := mts.byName(*c.MealType)
		if err != nil {
			return MealWrite{}, err
		}
		if setField(o, "type", mt.OriginalType) {
			changed = append(changed, "type")
		}
		var typeUID any
		if mt.UID != "" {
			typeUID = mt.UID
		}
		if setField(o, "type_uid", typeUID) {
			changed = append(changed, "type_uid")
		}
	}
	if c.Name != nil && setField(o, "name", strings.TrimSpace(*c.Name)) {
		changed = append(changed, "name")
	}
	if c.RecipeUID != nil {
		var link any
		if r := strings.TrimSpace(*c.RecipeUID); r != "" {
			if _, err := s.w.GetRecipe(ctx, r); err != nil {
				if isNotFound(err) {
					return MealWrite{}, notFound("Unknown recipe_uid %q. Use search_recipes to find a valid uid.", r)
				}
				return MealWrite{}, s.mapError(tool, err)
			}
			link = r
		}
		if setField(o, "recipe_uid", link) {
			changed = append(changed, "recipe_uid")
		}
	}
	if len(changed) == 0 {
		s.audit(tool, resMeal, []change{{uid: uid}}, "no_change")
		return MealWrite{Meal: s.mealSummary(o, mts), NoChange: true}, nil
	}
	mustSet(o, "deleted", false)
	warning, err := s.commit(ctx, tool, resMeal, []change{{uid: uid, fields: changed}}, func(c context.Context) error {
		return s.w.SaveMeals(c, []paprikaclient.Object{o})
	})
	if err != nil {
		return MealWrite{}, err
	}
	s.m.Invalidate(mirror.ResourceMeals)
	return MealWrite{Meal: s.mealSummary(o, mts), Changed: changed, Warning: warning}, nil
}

// DeleteResult is the result of a delete. Before holds the whole object as it
// was just before the delete.
type DeleteResult struct {
	Before  map[string]any
	Warning string
}

// DeleteMeal removes a meal from the plan. Meal deletes may not be undoable.
func (s *Service) DeleteMeal(ctx context.Context, uid string) (DeleteResult, error) {
	const tool = "delete_meal"
	uid, err := requireText("uid", uid, maxRefLen)
	if err != nil {
		return DeleteResult{}, err
	}
	unlock, err := s.beginWrite(ctx)
	if err != nil {
		return DeleteResult{}, err
	}
	defer unlock()

	o, _, err := s.readMeal(ctx, tool, uid)
	if err != nil {
		return DeleteResult{}, err
	}
	before := ObjectMap(o)
	mustSet(o, "deleted", true)
	warning, err := s.commit(ctx, tool, resMeal, []change{{uid: uid, fields: []string{"deleted"}}}, func(c context.Context) error {
		return s.w.SaveMeals(c, []paprikaclient.Object{o})
	})
	if err != nil {
		return DeleteResult{}, err
	}
	s.m.Invalidate(mirror.ResourceMeals)
	return DeleteResult{Before: before, Warning: warning}, nil
}

// readMeal reads the current meals from the API and returns a copy of one raw
// meal, with the account's meal types.
func (s *Service) readMeal(ctx context.Context, tool, uid string) (paprikaclient.Object, mealTypes, error) {
	meals, err := s.w.Meals(ctx)
	if err != nil {
		return nil, mealTypes{}, s.mapError(tool, err)
	}
	var found paprikaclient.Object
	for _, m := range meals {
		if m.Raw.UID() == uid {
			found = m.Raw.Clone()
			break
		}
	}
	if found == nil {
		return nil, mealTypes{}, notFound("No meal has the uid %q. Use list_meals to find a valid uid.", uid)
	}
	types, err := s.w.MealTypes(ctx)
	if err != nil {
		return nil, mealTypes{}, s.mapError(tool, err)
	}
	return found, newMealTypes(types), nil
}

package fakepaprika

import "strings"

// Synthetic identifiers used by [Server.SeedDefault].
const (
	CategoryDinnerUID  = "CAT00000-0000-4000-8000-000000000001"
	CategoryDessertUID = "CAT00000-0000-4000-8000-000000000002"
	RecipeSoupUID      = "REC00000-0000-4000-8000-000000000001"
	RecipeTrashedUID   = "REC00000-0000-4000-8000-000000000002"
	AisleProduceUID    = "AISLE000000000000000000000000000000000000000000000000000000000001"
	MealTypeLunchUID   = "MEALTYPE0000000000000000000000000000000000000000000000000000002"
	GroceryListUID     = "LIST0000-0000-4000-8000-000000000001"
)

// SampleRecipe returns a synthetic recipe. It includes a field named
// "future_field" so tests can check that unknown fields survive a round trip.
func SampleRecipe(uid, name string) map[string]any {
	return map[string]any{
		"uid":              uid,
		"hash":             strings.Repeat("a", 64),
		"name":             name,
		"created":          "2030-01-01 00:00:00",
		"description":      "",
		"ingredients":      "1 cup water\n2 carrots",
		"directions":       "Boil.\nServe.",
		"notes":            "",
		"nutritional_info": "",
		"prep_time":        "5 min",
		"cook_time":        "10 min",
		"total_time":       "15 min",
		"difficulty":       "",
		"servings":         "4",
		"rating":           0,
		"scale":            nil,
		"source":           "Example Kitchen",
		"source_url":       "",
		"on_favorites":     false,
		"is_pinned":        false,
		"in_trash":         false,
		"photo":            "",
		"photo_large":      nil,
		"photo_hash":       nil,
		"image_url":        nil,
		"categories":       []any{CategoryDinnerUID},
		"servings_min":     4,
		"future_field":     map[string]any{"keep": 1.50},
	}
}

// SampleCategory returns a synthetic category.
func SampleCategory(uid, name string) map[string]any {
	return map[string]any{
		"uid": uid, "name": name, "order_flag": 0, "parent_uid": nil, "cookbook_uid": nil,
	}
}

// SampleMealType returns a synthetic meal type.
func SampleMealType(uid, name string, original int) map[string]any {
	return map[string]any{
		"uid": uid, "name": name, "order_flag": original, "original_type": original,
		"color": "#112233", "export_time": 43200, "export_all_day": false,
	}
}

// SampleMeal returns a synthetic meal. Date has the form "YYYY-MM-DD HH:MM:SS".
func SampleMeal(uid, name, date, typeUID string) map[string]any {
	return map[string]any{
		"uid": uid, "name": name, "date": date, "order_flag": 0, "type": 1,
		"type_uid": typeUID, "recipe_uid": nil, "is_ingredient": false, "scale": nil,
	}
}

// SampleGroceryList returns a synthetic grocery list.
func SampleGroceryList(uid, name string, isDefault bool) map[string]any {
	return map[string]any{
		"uid": uid, "name": name, "order_flag": 0, "is_default": isDefault, "reminders_list": "",
	}
}

// SampleAisle returns a synthetic aisle.
func SampleAisle(uid, name string) map[string]any {
	return map[string]any{"uid": uid, "name": name, "order_flag": 0}
}

// SampleGrocery returns a synthetic grocery item.
func SampleGrocery(uid, name, listUID, aisleUID string) map[string]any {
	return map[string]any{
		"uid": uid, "name": name, "order_flag": 0, "purchased": false, "recipe": nil,
		"aisle": "Produce", "ingredient": name, "instruction": "", "quantity": "",
		"separate": false, "aisle_uid": aisleUID, "list_uid": listUID, "recipe_uid": nil,
	}
}

// SampleLegacyMeal returns a meal as an account without meal types has them: a
// null type_uid, a null recipe_uid, and only the numeric type.
func SampleLegacyMeal(uid, name, date string, typ int) map[string]any {
	return map[string]any{
		"uid": uid, "name": name, "date": date, "order_flag": 0, "type": typ,
		"type_uid": nil, "recipe_uid": nil, "is_ingredient": false, "scale": nil,
	}
}

// SeedNoMealTypes stores the [Server.SeedDefault] library without meal types,
// and two meals in the shape a live account without meal types has: a null
// type_uid and only the numeric type (1 is Lunch, 2 is Dinner).
func (s *Server) SeedNoMealTypes() {
	s.seedLibrary()
	s.Seed("meals",
		SampleLegacyMeal("LEGACY-MEAL-1", "Leftovers", "2030-05-01 00:00:00", 1),
		SampleLegacyMeal("LEGACY-MEAL-2", "Pizza", "2030-05-01 00:00:00", 2),
	)
}

// SeedDefault stores a small synthetic library: two categories, two recipes
// (one in the trash), one aisle, four meal types, a default grocery list with
// one item, and no meals.
func (s *Server) SeedDefault() {
	s.seedLibrary()
	s.Seed("mealtypes",
		SampleMealType("MEALTYPE0000000000000000000000000000000000000000000000000000001", "Breakfast", 0),
		SampleMealType(MealTypeLunchUID, "Lunch", 1),
		SampleMealType("MEALTYPE0000000000000000000000000000000000000000000000000000003", "Dinner", 2),
		SampleMealType("MEALTYPE0000000000000000000000000000000000000000000000000000004", "Snacks", 3),
	)
}

func (s *Server) seedLibrary() {
	s.Seed("categories",
		SampleCategory(CategoryDinnerUID, "Dinner"),
		SampleCategory(CategoryDessertUID, "Dessert"),
	)
	trashed := SampleRecipe(RecipeTrashedUID, "Old Stew")
	trashed["in_trash"] = true
	trashed["hash"] = strings.Repeat("b", 64)
	s.SeedRecipe(SampleRecipe(RecipeSoupUID, "Carrot Soup"))
	s.SeedRecipe(trashed)
	s.Seed("groceryaisles", SampleAisle(AisleProduceUID, "Produce"))
	s.Seed("grocerylists", SampleGroceryList(GroceryListUID, "Groceries", true))
	s.Seed("groceries", SampleGrocery("ITEM0000-0000-4000-8000-000000000001", "carrots", GroceryListUID, AisleProduceUID))
}

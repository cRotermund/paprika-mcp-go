package paprikaclient

// Counters maps a resource name to its change counter from sync/status/.
type Counters map[string]int64

// RecipeRef is one row of the recipe list. Treat Hash as an opaque string.
type RecipeRef struct {
	UID  string `json:"uid"`
	Hash string `json:"hash"`
}

// Recipe is the typed view of a full recipe. Fields not listed here are only
// available through the raw [Object].
type Recipe struct {
	UID         string   `json:"uid"`
	Hash        string   `json:"hash"`
	Name        string   `json:"name"`
	Created     string   `json:"created"`
	Description string   `json:"description"`
	Ingredients string   `json:"ingredients"`
	Directions  string   `json:"directions"`
	Notes       string   `json:"notes"`
	Source      string   `json:"source"`
	SourceURL   string   `json:"source_url"`
	Servings    string   `json:"servings"`
	PrepTime    string   `json:"prep_time"`
	CookTime    string   `json:"cook_time"`
	TotalTime   string   `json:"total_time"`
	Rating      int      `json:"rating"`
	OnFavorites bool     `json:"on_favorites"`
	InTrash     bool     `json:"in_trash"`
	Categories  []string `json:"categories"`
}

// Category is a recipe category. Categories nest through ParentUID.
type Category struct {
	UID       string  `json:"uid"`
	Name      string  `json:"name"`
	OrderFlag int     `json:"order_flag"`
	ParentUID *string `json:"parent_uid"`
}

// Meal is a planned meal. Date has the form "YYYY-MM-DD HH:MM:SS".
type Meal struct {
	UID       string  `json:"uid"`
	Name      string  `json:"name"`
	Date      string  `json:"date"`
	OrderFlag int     `json:"order_flag"`
	Type      int     `json:"type"`
	TypeUID   string  `json:"type_uid"`
	RecipeUID *string `json:"recipe_uid"`
}

// MealType is a kind of meal, such as Lunch.
type MealType struct {
	UID          string `json:"uid"`
	Name         string `json:"name"`
	OrderFlag    int    `json:"order_flag"`
	OriginalType int    `json:"original_type"`
	ExportTime   int    `json:"export_time"`
	ExportAllDay bool   `json:"export_all_day"`
}

// GroceryList is a grocery list.
type GroceryList struct {
	UID       string `json:"uid"`
	Name      string `json:"name"`
	OrderFlag int    `json:"order_flag"`
	IsDefault bool   `json:"is_default"`
}

// Grocery is one grocery item.
type Grocery struct {
	UID         string `json:"uid"`
	Name        string `json:"name"`
	OrderFlag   int    `json:"order_flag"`
	Purchased   bool   `json:"purchased"`
	Aisle       string `json:"aisle"`
	AisleUID    string `json:"aisle_uid"`
	ListUID     string `json:"list_uid"`
	Ingredient  string `json:"ingredient"`
	Instruction string `json:"instruction"`
	Quantity    string `json:"quantity"`
}

// GroceryAisle is a store aisle.
type GroceryAisle struct {
	UID       string `json:"uid"`
	Name      string `json:"name"`
	OrderFlag int    `json:"order_flag"`
}

// GroceryIngredient remembers which aisle an ingredient belongs to.
type GroceryIngredient struct {
	UID      string  `json:"uid"`
	Name     string  `json:"name"`
	AisleUID *string `json:"aisle_uid"`
}

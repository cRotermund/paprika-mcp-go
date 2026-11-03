package service

import (
	"fmt"
	"slices"
	"strings"

	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/paprikaclient"
)

// builtinMealTypes are Paprika's four original meal types, keyed by the
// numeric "type" that meals carry. They are used only when an account has no
// meal types of its own: a live test account had none, and every meal there had
// a null type_uid and a numeric type of 1 to 3. The names are what a real
// account's types with these original_type values were called. The server's
// own fallback for the numbers is inferred, not observed.
var builtinMealTypes = []paprikaclient.MealType{
	{Name: "Breakfast", OrderFlag: 0, OriginalType: 0},
	{Name: "Lunch", OrderFlag: 1, OriginalType: 1},
	{Name: "Dinner", OrderFlag: 2, OriginalType: 2},
	{Name: "Snacks", OrderFlag: 3, OriginalType: 3},
}

// MealTypeInfo is a meal type as shown to clients.
type MealTypeInfo struct {
	// UID is empty for a built-in type.
	UID          string
	Name         string
	OriginalType int
	// Builtin is true when the account has no meal types and this is one of
	// the four defaults.
	Builtin bool
}

// mealTypes is the meal types of one account, or the four built-in ones.
type mealTypes struct {
	list    []paprikaclient.MealType // display order
	builtin bool
}

func newMealTypes(entries []paprikaclient.Entry[paprikaclient.MealType]) mealTypes {
	if len(entries) == 0 {
		return mealTypes{list: slices.Clone(builtinMealTypes), builtin: true}
	}
	vals := make([]paprikaclient.MealType, len(entries))
	for i, e := range entries {
		vals[i] = e.Value
	}
	slices.SortStableFunc(vals, func(a, b paprikaclient.MealType) int { return a.OrderFlag - b.OrderFlag })
	return mealTypes{list: vals}
}

func (mt mealTypes) infos() []MealTypeInfo {
	out := make([]MealTypeInfo, len(mt.list))
	for i, t := range mt.list {
		out[i] = MealTypeInfo{UID: t.UID, Name: t.Name, OriginalType: t.OriginalType, Builtin: mt.builtin}
	}
	return out
}

// names lists the valid names, for error messages.
func (mt mealTypes) names() string {
	names := make([]string, len(mt.list))
	for i, t := range mt.list {
		names[i] = t.Name
	}
	return strings.Join(names, ", ")
}

// byName finds a type by case-insensitive name.
func (mt mealTypes) byName(name string) (paprikaclient.MealType, error) {
	want := strings.TrimSpace(name)
	for _, t := range mt.list {
		if strings.EqualFold(strings.TrimSpace(t.Name), want) {
			return t, nil
		}
	}
	return paprikaclient.MealType{}, notFound("Unknown meal type %q. Valid meal types are: %s. Call list_meal_types to see them.", name, mt.names())
}

// forMeal names a meal's type: by type_uid when it resolves, else by matching
// the numeric type to a type's original_type, else by the built-in defaults,
// else "Unknown (type N)". The second result is the sort position.
func (mt mealTypes) forMeal(m paprikaclient.Meal) (string, int) {
	if m.TypeUID != "" {
		for _, t := range mt.list {
			if t.UID == m.TypeUID {
				return t.Name, t.OrderFlag
			}
		}
	}
	for _, t := range mt.list {
		if t.OriginalType == m.Type {
			return t.Name, t.OrderFlag
		}
	}
	for _, t := range builtinMealTypes {
		if t.OriginalType == m.Type {
			return t.Name, t.OrderFlag
		}
	}
	return fmt.Sprintf("Unknown (type %d)", m.Type), 1000 + m.Type
}

package mcpserver_test

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/mcpserver"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/paprikaclient/fakepaprika"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/testsupport"
)

var writeToolNames = []string{
	"create_recipe", "update_recipe", "trash_recipe", "restore_recipe",
	"add_meals", "update_meal", "delete_meal",
	"add_grocery_items", "update_grocery_item", "delete_grocery_item", "create_grocery_list", "delete_grocery_list",
}

var destructiveTools = map[string]bool{
	"trash_recipe": true, "delete_meal": true, "delete_grocery_item": true, "delete_grocery_list": true,
}

func writeSession(t *testing.T) (*mcp.ClientSession, *testsupport.Stack) {
	t.Helper()
	st := testsupport.NewStack(t, nil, seed)
	st.Fake.ResetRequests()
	return connect(t, st.Service, mcpserver.Options{WritesEnabled: true}), st
}

func toolsByName(t *testing.T, s *mcp.ClientSession) map[string]*mcp.Tool {
	t.Helper()
	res, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		got[tool.Name] = tool
	}
	return got
}

func TestWriteToolsAbsentWhenDisabled(t *testing.T) {
	s, _ := newSession(t)
	got := toolsByName(t, s)
	for _, name := range writeToolNames {
		if _, ok := got[name]; ok {
			t.Errorf("write tool %s is registered although writes are disabled", name)
		}
	}
	if _, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "create_recipe", Arguments: map[string]any{"name": "X"}}); err == nil {
		t.Error("calling an unregistered write tool should fail")
	}
}

func TestWriteToolsPresentWhenEnabled(t *testing.T) {
	s, _ := writeSession(t)
	got := toolsByName(t, s)
	for _, name := range writeToolNames {
		tool, ok := got[name]
		if !ok {
			t.Errorf("write tool %s is missing", name)
			continue
		}
		if tool.InputSchema == nil || tool.OutputSchema == nil {
			t.Errorf("%s lacks a typed schema", name)
		}
		a := tool.Annotations
		if a == nil || a.ReadOnlyHint || a.DestructiveHint == nil {
			t.Errorf("%s annotations = %+v, want readOnlyHint false and an explicit destructiveHint", name, a)
			continue
		}
		if *a.DestructiveHint != destructiveTools[name] {
			t.Errorf("%s destructiveHint = %v", name, *a.DestructiveHint)
		}
	}
	if len(got) != len(writeToolNames)+9 {
		t.Errorf("tools = %d, want read tools + ping + write tools", len(got))
	}
	if !got["ping"].Annotations.ReadOnlyHint {
		t.Error("ping should be read-only")
	}
	if d := got["delete_meal"].Description; !strings.Contains(d, "may not be undoable") {
		t.Errorf("delete_meal description: %q", d)
	}
}

type recipeWritten struct {
	UID        string   `json:"uid"`
	Hash       string   `json:"hash"`
	Name       string   `json:"name"`
	InTrash    bool     `json:"in_trash"`
	Changed    []string `json:"changed"`
	NoChange   bool     `json:"no_change"`
	Warning    string   `json:"warning"`
	Categories []struct {
		UID  string `json:"uid"`
		Name string `json:"name"`
	} `json:"categories"`
}

func TestRecipeWriteTools(t *testing.T) {
	s, st := writeSession(t)
	var created recipeWritten
	res := call(t, s, "create_recipe", map[string]any{"name": "MCP-TEST Stew", "ingredients": "beef", "categories": []string{"dinner"}}, &created)
	if res.IsError || created.UID == "" || len(created.Categories) != 1 || created.Categories[0].Name != "Dinner" {
		t.Fatalf("create_recipe = %+v %+v", created, text(res))
	}

	var found searchResult
	call(t, s, "search_recipes", map[string]any{"query": "stew"}, &found)
	if len(found.Recipes) != 1 || found.Recipes[0].UID != created.UID {
		t.Fatalf("search after create = %+v", found)
	}

	res = call(t, s, "update_recipe", map[string]any{"uid": created.UID, "expected_hash": "STALE", "rating": 5}, nil)
	if !res.IsError || !strings.Contains(text(res), "get_recipe") {
		t.Fatalf("stale update = %v %s", res.IsError, text(res))
	}
	var updated recipeWritten
	call(t, s, "update_recipe", map[string]any{"uid": created.UID, "expected_hash": created.Hash, "rating": 5}, &updated)
	if len(updated.Changed) != 1 || updated.Changed[0] != "rating" || updated.Hash == created.Hash {
		t.Fatalf("update = %+v", updated)
	}
	var again recipeWritten
	call(t, s, "update_recipe", map[string]any{"uid": created.UID, "rating": 5}, &again)
	if !again.NoChange || len(again.Changed) != 0 {
		t.Fatalf("repeat update = %+v", again)
	}

	var trashed recipeWritten
	call(t, s, "trash_recipe", map[string]any{"uid": created.UID}, &trashed)
	if !trashed.InTrash {
		t.Fatalf("trash = %+v", trashed)
	}
	var restored recipeWritten
	call(t, s, "restore_recipe", map[string]any{"uid": created.UID}, &restored)
	if restored.InTrash {
		t.Fatalf("restore = %+v", restored)
	}

	res = call(t, s, "create_recipe", map[string]any{"name": "X", "categories": []string{"Brunchy"}}, nil)
	if !res.IsError || !strings.Contains(text(res), `"Brunchy"`) {
		t.Fatalf("unknown category = %v %s", res.IsError, text(res))
	}
	res = call(t, s, "trash_recipe", map[string]any{"uid": "NOPE"}, nil)
	if !res.IsError || !strings.Contains(text(res), `No recipe has the uid "NOPE"`) {
		t.Fatalf("unknown uid = %s", text(res))
	}
	if n := len(st.Fake.RequestsTo("POST", "/api/v2/sync/notify/")); n != 4 {
		t.Errorf("notify calls = %d, want one per successful write", n)
	}
}

func TestGetRecipeUnknownUIDMessage(t *testing.T) {
	s, _ := newSession(t)
	res := call(t, s, "get_recipe", map[string]any{"uid": "NOPE"}, nil)
	msg := text(res)
	if !res.IsError || !strings.Contains(msg, `No recipe has the uid "NOPE"`) || strings.Contains(strings.ToLower(msg), "unusable") {
		t.Fatalf("message = %q", msg)
	}
}

func TestMealAndGroceryWriteTools(t *testing.T) {
	s, _ := writeSession(t)

	var added struct {
		Added   int `json:"added"`
		Failed  int `json:"failed"`
		Results []struct {
			Index int    `json:"index"`
			OK    bool   `json:"ok"`
			Error string `json:"error"`
			Meal  *struct {
				UID      string `json:"uid"`
				Date     string `json:"date"`
				MealType string `json:"meal_type"`
			} `json:"meal"`
		} `json:"results"`
	}
	res := call(t, s, "add_meals", map[string]any{"meals": []map[string]any{
		{"date": "2030-07-01", "meal_type": "dinner", "recipe_uid": "R1"},
		{"date": "2030-07-01", "meal_type": "Elevenses", "name": "Tea"},
	}}, &added)
	if res.IsError || added.Added != 1 || added.Failed != 1 || !added.Results[0].OK || added.Results[0].Meal.MealType != "Dinner" ||
		!strings.Contains(added.Results[1].Error, `"Elevenses"`) {
		t.Fatalf("add_meals = %+v %s", added, text(res))
	}
	mealUID := added.Results[0].Meal.UID

	var moved struct {
		Meal    struct{ Date string } `json:"meal"`
		Changed []string              `json:"changed"`
	}
	call(t, s, "update_meal", map[string]any{"uid": mealUID, "date": "2030-07-02"}, &moved)
	if moved.Meal.Date != "2030-07-02" || len(moved.Changed) != 1 {
		t.Fatalf("update_meal = %+v", moved)
	}
	var deleted struct {
		Deleted bool           `json:"deleted"`
		Before  map[string]any `json:"before"`
		Note    string         `json:"note"`
	}
	call(t, s, "delete_meal", map[string]any{"uid": mealUID}, &deleted)
	if !deleted.Deleted || deleted.Before["uid"] != mealUID || deleted.Before["date"] != "2030-07-02 00:00:00" || !strings.Contains(deleted.Note, "undoable") {
		t.Fatalf("delete_meal = %+v", deleted)
	}
	if res := call(t, s, "add_meals", map[string]any{"meals": []map[string]any{}}, nil); !res.IsError {
		t.Error("an empty batch should be an error")
	}

	var groceries struct {
		ListName string `json:"list_name"`
		Added    int    `json:"added"`
		Results  []struct {
			OK   bool `json:"ok"`
			Item *struct {
				UID   string `json:"uid"`
				Aisle string `json:"aisle"`
			} `json:"item"`
		} `json:"results"`
	}
	call(t, s, "add_grocery_items", map[string]any{"items": []map[string]any{{"text": "2 leeks"}, {"text": "milk", "aisle": "Produce"}}}, &groceries)
	if groceries.Added != 2 || groceries.ListName != "Groceries" || groceries.Results[0].Item == nil || groceries.Results[0].Item.Aisle != "Produce" {
		t.Fatalf("add_grocery_items = %+v", groceries)
	}
	itemUID := groceries.Results[0].Item.UID

	var checked struct {
		Item    struct{ Purchased bool } `json:"item"`
		Changed []string                 `json:"changed"`
	}
	call(t, s, "update_grocery_item", map[string]any{"uid": itemUID, "purchased": true}, &checked)
	if !checked.Item.Purchased || len(checked.Changed) != 1 {
		t.Fatalf("update_grocery_item = %+v", checked)
	}
	var listed struct {
		Items []struct{ UID string } `json:"items"`
	}
	call(t, s, "list_grocery_items", map[string]any{}, &listed)
	for _, it := range listed.Items {
		if it.UID == itemUID {
			t.Error("the checked-off item is still listed as open")
		}
	}
	call(t, s, "delete_grocery_item", map[string]any{"uid": itemUID}, &deleted)
	if !deleted.Deleted {
		t.Fatal("delete_grocery_item did not report a delete")
	}

	var newList struct {
		List struct {
			UID  string `json:"uid"`
			Name string `json:"name"`
		} `json:"list"`
	}
	call(t, s, "create_grocery_list", map[string]any{"name": "MCP-TEST Trip"}, &newList)
	if newList.List.UID == "" {
		t.Fatal("no list uid")
	}
	if res := call(t, s, "delete_grocery_list", map[string]any{"list": "Groceries"}, nil); !res.IsError || !strings.Contains(text(res), "default") {
		t.Errorf("default list delete = %s", text(res))
	}
	call(t, s, "delete_grocery_list", map[string]any{"list": newList.List.UID}, &deleted)
	if !deleted.Deleted || deleted.Before["name"] != "MCP-TEST Trip" {
		t.Fatalf("delete_grocery_list = %+v", deleted)
	}
}

func TestMealTypesWhenTheAccountHasNone(t *testing.T) {
	st := testsupport.NewStack(t, nil, func(f *fakepaprika.Server) { f.SeedNoMealTypes() })
	s := connect(t, st.Service, mcpserver.Options{WritesEnabled: true})

	var types struct {
		MealTypes []struct {
			UID          string `json:"uid"`
			Name         string `json:"name"`
			OriginalType int    `json:"original_type"`
			Builtin      bool   `json:"builtin"`
		} `json:"meal_types"`
	}
	call(t, s, "list_meal_types", nil, &types)
	if len(types.MealTypes) != 4 || types.MealTypes[1].Name != "Lunch" || !types.MealTypes[1].Builtin || types.MealTypes[1].UID != "" || types.MealTypes[1].OriginalType != 1 {
		t.Fatalf("meal types = %+v", types)
	}

	var meals struct {
		Meals []struct {
			Name     string `json:"name"`
			MealType string `json:"meal_type"`
		} `json:"meals"`
	}
	call(t, s, "list_meals", map[string]any{"start_date": "2030-05-01", "end_date": "2030-05-01"}, &meals)
	if len(meals.Meals) != 2 || meals.Meals[0].MealType != "Lunch" || meals.Meals[1].MealType != "Dinner" {
		t.Fatalf("meals = %+v", meals)
	}

	var added struct{ Added int }
	call(t, s, "add_meals", map[string]any{"meals": []map[string]any{{"date": "2030-05-03", "meal_type": "snacks", "name": "Popcorn"}}}, &added)
	if added.Added != 1 {
		t.Fatalf("add_meals = %+v", added)
	}
	objs := st.Fake.Objects("meals")
	last := objs[len(objs)-1]
	if last["name"] != "Popcorn" || last["type_uid"] != nil {
		t.Errorf("stored meal = %v", last)
	}
}

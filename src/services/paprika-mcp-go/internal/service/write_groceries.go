package service

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/cache"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/paprikaclient"
)

// GroceryItemInput is one item to add. Text goes in both name and ingredient
// unless Ingredient is given. Quantity and Instruction stay empty unless given.
type GroceryItemInput struct {
	Text        string
	Quantity    string
	Instruction string
	Ingredient  string
	// Aisle is an aisle name. When empty the aisle comes from the remembered
	// ingredient, or is chosen by the server.
	Aisle string
}

// GroceryItemResult is the outcome of one item of a batch. Failure is empty
// when the item was added.
type GroceryItemResult struct {
	Index   int
	Item    GroceryItemSummary
	Failure string
}

// AddGroceryItemsResult is the result of add_grocery_items.
type AddGroceryItemsResult struct {
	ListUID  string
	ListName string
	Items    []GroceryItemResult
	Added    int
	Warning  string
}

// AddGroceryItems adds items to a list (the default list when list is empty)
// in one POST. Items whose aisle cannot be resolved are reported and skipped.
func (s *Service) AddGroceryItems(ctx context.Context, list string, items []GroceryItemInput) (AddGroceryItemsResult, error) {
	const tool = "add_grocery_items"
	if err := checkBatch("items", len(items)); err != nil {
		return AddGroceryItemsResult{}, err
	}
	if err := checkText("list", list, maxRefLen); err != nil {
		return AddGroceryItemsResult{}, err
	}
	for i, it := range items {
		if strings.TrimSpace(it.Text) == "" {
			return AddGroceryItemsResult{}, invalid("Item %d: text is required.", i)
		}
		for field, v := range map[string]string{"text": it.Text, "quantity": it.Quantity, "instruction": it.Instruction,
			"ingredient": it.Ingredient, "aisle": it.Aisle} {
			if err := checkText(field, v, maxShortLen); err != nil {
				return AddGroceryItemsResult{}, invalid("Item %d: %s", i, err.Error())
			}
		}
	}

	unlock, err := s.beginWrite(ctx)
	if err != nil {
		return AddGroceryItemsResult{}, err
	}
	defer unlock()

	lists, err := s.w.GroceryLists(ctx)
	if err != nil {
		return AddGroceryItemsResult{}, s.mapError(tool, err)
	}
	target, err := pickList(lists, list) // the list must exist, or the app deletes the item
	if err != nil {
		return AddGroceryItemsResult{}, err
	}
	aisles, err := s.w.GroceryAisles(ctx)
	if err != nil {
		return AddGroceryItemsResult{}, s.mapError(tool, err)
	}
	ar := newAisleResolver(aisles)
	for _, it := range items {
		if strings.TrimSpace(it.Aisle) == "" {
			ing, err := s.w.GroceryIngredients(ctx)
			if err != nil {
				return AddGroceryItemsResult{}, s.mapError(tool, err)
			}
			ar.remembered(ing)
			break
		}
	}

	results := make([]GroceryItemResult, len(items))
	var objects []paprikaclient.Object
	var changes []change
	var failures []ItemError
	for i, in := range items {
		results[i].Index = i
		text := strings.TrimSpace(in.Text)
		ingredient := strings.TrimSpace(in.Ingredient)
		if ingredient == "" {
			ingredient = text
		}
		aisle, err := ar.resolve(in.Aisle, ingredient)
		if err != nil {
			results[i].Failure = err.Error()
			failures = append(failures, ItemError{Index: i, Message: err.Error()})
			continue
		}

		o := paprikaclient.Object{}
		uid := newUID()
		mustSet(o, "uid", uid)
		mustSet(o, "deleted", false)
		mustSet(o, "name", text)
		mustSet(o, "order_flag", 0)
		mustSet(o, "purchased", false)
		mustSet(o, "recipe", nil)
		mustSet(o, "aisle", aisle.Name)
		mustSet(o, "ingredient", ingredient)
		mustSet(o, "instruction", strings.TrimSpace(in.Instruction))
		mustSet(o, "quantity", strings.TrimSpace(in.Quantity))
		mustSet(o, "separate", false)
		mustSet(o, "aisle_uid", aisle.UID)
		mustSet(o, "list_uid", target.UID)
		objects = append(objects, o)
		changes = append(changes, change{uid: uid, fields: []string{"name", "ingredient", "instruction", "quantity", "aisle", "aisle_uid", "list_uid"}})
		results[i].Item = groceryItemSummary(o, "")
	}
	if len(objects) == 0 {
		return AddGroceryItemsResult{}, batchFailure(failures)
	}

	warning, err := s.commit(ctx, tool, resGroceryItem, changes, func(c context.Context) error { return s.w.SaveGroceryItems(c, objects) })
	if err != nil {
		return AddGroceryItemsResult{}, err
	}
	s.cache.Invalidate(cache.ResourceGroceries)
	return AddGroceryItemsResult{ListUID: target.UID, ListName: target.Name, Items: results, Added: len(objects), Warning: warning}, nil
}

// aisleResolver picks the aisle for a new item: the given aisle name, then the
// remembered ingredient's aisle, then an existing aisle chosen by the server.
// An unknown or empty aisle_uid is never sent (check W5).
type aisleResolver struct {
	aisles   []paprikaclient.GroceryAisle
	byName   map[string]paprikaclient.GroceryAisle
	byUID    map[string]paprikaclient.GroceryAisle
	remember map[string]string // lower-case ingredient -> aisle uid
}

func newAisleResolver(entries []paprikaclient.Entry[paprikaclient.GroceryAisle]) *aisleResolver {
	r := &aisleResolver{
		byName:   map[string]paprikaclient.GroceryAisle{},
		byUID:    map[string]paprikaclient.GroceryAisle{},
		remember: map[string]string{},
	}
	for _, e := range entries {
		r.aisles = append(r.aisles, e.Value)
	}
	slices.SortStableFunc(r.aisles, func(a, b paprikaclient.GroceryAisle) int { return a.OrderFlag - b.OrderFlag })
	for _, a := range r.aisles {
		key := strings.ToLower(strings.TrimSpace(a.Name))
		if _, dup := r.byName[key]; !dup {
			r.byName[key] = a
		}
		r.byUID[a.UID] = a
	}
	return r
}

func (r *aisleResolver) remembered(entries []paprikaclient.Entry[paprikaclient.GroceryIngredient]) {
	for _, e := range entries {
		if e.Value.AisleUID != nil {
			key := strings.ToLower(strings.TrimSpace(e.Value.Name))
			if _, dup := r.remember[key]; !dup && key != "" {
				r.remember[key] = *e.Value.AisleUID
			}
		}
	}
}

// fallback is the aisle chosen when nothing else matches: one named
// Miscellaneous (or Other), else the first aisle in store order.
func (r *aisleResolver) fallback() (paprikaclient.GroceryAisle, bool) {
	for _, name := range []string{"miscellaneous", "misc", "other"} {
		if a, ok := r.byName[name]; ok {
			return a, true
		}
	}
	if len(r.aisles) > 0 {
		return r.aisles[0], true
	}
	return paprikaclient.GroceryAisle{}, false
}

func (r *aisleResolver) resolve(given, ingredient string) (paprikaclient.GroceryAisle, error) {
	if given = strings.TrimSpace(given); given != "" {
		if a, ok := r.byName[strings.ToLower(given)]; ok {
			return a, nil
		}
		if a, ok := r.byUID[given]; ok {
			return a, nil
		}
		return paprikaclient.GroceryAisle{}, notFound("Unknown aisle %q. Call list_grocery_aisles for the valid names.", given)
	}
	if uid, ok := r.remember[strings.ToLower(strings.TrimSpace(ingredient))]; ok {
		if a, ok := r.byUID[uid]; ok {
			return a, nil
		}
	}
	if a, ok := r.fallback(); ok {
		return a, nil
	}
	return paprikaclient.GroceryAisle{}, invalid("The account has no grocery aisles, so the item cannot be placed. Create an aisle in the Paprika app first.")
}

// groceryItemSummary describes a raw item. aisleName is used when the item has
// no aisle text.
func groceryItemSummary(o paprikaclient.Object, aisleName string) GroceryItemSummary {
	var g paprikaclient.Grocery
	if raw, err := json.Marshal(o); err == nil {
		_ = json.Unmarshal(raw, &g)
	}
	aisle := g.Aisle
	if aisle == "" {
		aisle = aisleName
	}
	return GroceryItemSummary{UID: g.UID, Name: g.Name, Quantity: g.Quantity, Instruction: g.Instruction,
		Aisle: aisle, Purchased: g.Purchased}
}

// GroceryChanges holds the changes for UpdateGroceryItem. A nil field is left
// alone.
type GroceryChanges struct {
	Name *string
	// Aisle is an aisle name to move the item to.
	Aisle     *string
	Purchased *bool
}

// GroceryItemWrite is the result of a grocery item update.
type GroceryItemWrite struct {
	Item     GroceryItemSummary
	Changed  []string
	NoChange bool
	Warning  string
}

// UpdateGroceryItem renames an item, moves it to another aisle, or checks it
// off or back on.
func (s *Service) UpdateGroceryItem(ctx context.Context, uid string, c GroceryChanges) (GroceryItemWrite, error) {
	const tool = "update_grocery_item"
	uid, err := requireText("uid", uid, maxRefLen)
	if err != nil {
		return GroceryItemWrite{}, err
	}
	if c.Name == nil && c.Aisle == nil && c.Purchased == nil {
		return GroceryItemWrite{}, invalid("Give at least one of name, aisle, or purchased to change.")
	}
	var name string
	if c.Name != nil {
		if name, err = requireText("name", *c.Name, maxShortLen); err != nil {
			return GroceryItemWrite{}, err
		}
	}
	if c.Aisle != nil {
		if _, err := requireText("aisle", *c.Aisle, maxShortLen); err != nil {
			return GroceryItemWrite{}, err
		}
	}

	unlock, err := s.beginWrite(ctx)
	if err != nil {
		return GroceryItemWrite{}, err
	}
	defer unlock()

	o, err := s.readGroceryItem(ctx, tool, uid)
	if err != nil {
		return GroceryItemWrite{}, err
	}
	var changed []string
	if c.Name != nil {
		old := o.String("name")
		if setField(o, "name", name) {
			changed = append(changed, "name")
			// Items made by this server (and the app) repeat the text in ingredient.
			if o.String("ingredient") == old && setField(o, "ingredient", name) {
				changed = append(changed, "ingredient")
			}
		}
	}
	if c.Aisle != nil {
		aisles, err := s.w.GroceryAisles(ctx)
		if err != nil {
			return GroceryItemWrite{}, s.mapError(tool, err)
		}
		a, err := newAisleResolver(aisles).resolve(*c.Aisle, "")
		if err != nil {
			return GroceryItemWrite{}, err
		}
		if setField(o, "aisle_uid", a.UID) {
			changed = append(changed, "aisle_uid")
		}
		if setField(o, "aisle", a.Name) {
			changed = append(changed, "aisle")
		}
	}
	if c.Purchased != nil && setField(o, "purchased", *c.Purchased) {
		changed = append(changed, "purchased")
	}
	if len(changed) == 0 {
		s.audit(tool, resGroceryItem, []change{{uid: uid}}, "no_change")
		return GroceryItemWrite{Item: groceryItemSummary(o, ""), NoChange: true}, nil
	}
	mustSet(o, "deleted", false)
	warning, err := s.commit(ctx, tool, resGroceryItem, []change{{uid: uid, fields: changed}}, func(c context.Context) error {
		return s.w.SaveGroceryItems(c, []paprikaclient.Object{o})
	})
	if err != nil {
		return GroceryItemWrite{}, err
	}
	s.cache.Invalidate(cache.ResourceGroceries)
	return GroceryItemWrite{Item: groceryItemSummary(o, ""), Changed: changed, Warning: warning}, nil
}

// DeleteGroceryItem removes an item from its list.
func (s *Service) DeleteGroceryItem(ctx context.Context, uid string) (DeleteResult, error) {
	const tool = "delete_grocery_item"
	uid, err := requireText("uid", uid, maxRefLen)
	if err != nil {
		return DeleteResult{}, err
	}
	unlock, err := s.beginWrite(ctx)
	if err != nil {
		return DeleteResult{}, err
	}
	defer unlock()

	o, err := s.readGroceryItem(ctx, tool, uid)
	if err != nil {
		return DeleteResult{}, err
	}
	before := ObjectMap(o)
	mustSet(o, "deleted", true)
	warning, err := s.commit(ctx, tool, resGroceryItem, []change{{uid: uid, fields: []string{"deleted"}}}, func(c context.Context) error {
		return s.w.SaveGroceryItems(c, []paprikaclient.Object{o})
	})
	if err != nil {
		return DeleteResult{}, err
	}
	s.cache.Invalidate(cache.ResourceGroceries)
	return DeleteResult{Before: before, Warning: warning}, nil
}

// readGroceryItem reads the current items from the API and returns a copy of
// one raw item.
func (s *Service) readGroceryItem(ctx context.Context, tool, uid string) (paprikaclient.Object, error) {
	items, err := s.w.Groceries(ctx)
	if err != nil {
		return nil, s.mapError(tool, err)
	}
	for _, it := range items {
		if it.Raw.UID() == uid {
			return it.Raw.Clone(), nil
		}
	}
	return nil, notFound("No grocery item has the uid %q. Use list_grocery_items to find a valid uid.", uid)
}

// GroceryListWrite is the result of creating a grocery list.
type GroceryListWrite struct {
	List    GroceryListSummary
	Warning string
}

// CreateGroceryList adds an empty, non-default list.
func (s *Service) CreateGroceryList(ctx context.Context, name string) (GroceryListWrite, error) {
	const tool = "create_grocery_list"
	name, err := requireText("name", name, maxShortLen)
	if err != nil {
		return GroceryListWrite{}, err
	}
	unlock, err := s.beginWrite(ctx)
	if err != nil {
		return GroceryListWrite{}, err
	}
	defer unlock()

	lists, err := s.w.GroceryLists(ctx)
	if err != nil {
		return GroceryListWrite{}, s.mapError(tool, err)
	}
	order := 0
	for _, l := range lists {
		if strings.EqualFold(strings.TrimSpace(l.Value.Name), name) {
			return GroceryListWrite{}, invalid("A grocery list named %q already exists (uid %s). Use it, or pick another name.", l.Value.Name, l.Value.UID)
		}
		order = max(order, l.Value.OrderFlag+1)
	}
	uid := newUID()
	o := paprikaclient.Object{}
	mustSet(o, "uid", uid)
	mustSet(o, "deleted", false)
	mustSet(o, "name", name)
	mustSet(o, "order_flag", order)
	mustSet(o, "is_default", false)
	mustSet(o, "reminders_list", "Paprika") // what the official client sends for a new list
	warning, err := s.commit(ctx, tool, resGroceryList, []change{{uid: uid, fields: []string{"name", "order_flag", "is_default", "reminders_list"}}}, func(c context.Context) error {
		return s.w.SaveGroceryLists(c, []paprikaclient.Object{o})
	})
	if err != nil {
		return GroceryListWrite{}, err
	}
	s.cache.Invalidate(cache.ResourceGroceryLists)
	return GroceryListWrite{List: GroceryListSummary{UID: uid, Name: name}, Warning: warning}, nil
}

// DeleteGroceryList removes a list. It refuses the default list and any list
// that still has items.
func (s *Service) DeleteGroceryList(ctx context.Context, list string) (DeleteResult, error) {
	const tool = "delete_grocery_list"
	list, err := requireText("list", list, maxRefLen)
	if err != nil {
		return DeleteResult{}, err
	}
	unlock, err := s.beginWrite(ctx)
	if err != nil {
		return DeleteResult{}, err
	}
	defer unlock()

	lists, err := s.w.GroceryLists(ctx)
	if err != nil {
		return DeleteResult{}, s.mapError(tool, err)
	}
	target, err := pickList(lists, list)
	if err != nil {
		return DeleteResult{}, err
	}
	if target.IsDefault {
		return DeleteResult{}, invalid("%q is the default grocery list and cannot be deleted.", target.Name)
	}
	items, err := s.w.Groceries(ctx)
	if err != nil {
		return DeleteResult{}, s.mapError(tool, err)
	}
	count := 0
	for _, it := range items {
		if it.Value.ListUID == target.UID {
			count++
		}
	}
	if count > 0 {
		return DeleteResult{}, invalid("The grocery list %q still has %d item(s). Delete or move them first; only empty lists can be deleted.", target.Name, count)
	}
	var raw paprikaclient.Object
	for _, l := range lists {
		if l.Value.UID == target.UID {
			raw = l.Raw.Clone()
		}
	}
	before := ObjectMap(raw)
	mustSet(raw, "deleted", true)
	warning, err := s.commit(ctx, tool, resGroceryList, []change{{uid: target.UID, fields: []string{"deleted"}}}, func(c context.Context) error {
		return s.w.SaveGroceryLists(c, []paprikaclient.Object{raw})
	})
	if err != nil {
		return DeleteResult{}, err
	}
	s.cache.Invalidate(cache.ResourceGroceryLists)
	return DeleteResult{Before: before, Warning: warning}, nil
}

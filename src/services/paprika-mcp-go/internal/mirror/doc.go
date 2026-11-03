// Package mirror keeps a local view of the Paprika library so that search and
// reads do not need one API call per recipe.
//
// # What it holds
//
// A small index entry per recipe (see [IndexEntry]), a short most-recently-used
// cache of recipe bodies, and the small lists (categories, meals, meal types,
// grocery lists, grocery items, aisles, remembered ingredients) as whole lists.
// Entries and lists returned by the mirror are shared and must not be modified;
// use paprikaclient.Object.Clone before changing a raw object.
//
// # Freshness
//
// Before serving a read the mirror checks sync/status/ if the last check is
// more than a minute old. A resource is downloaded again only when its counter
// changed. Recipes are compared by hash, as opaque strings: new and changed
// recipes are fetched and removed ones leave the index.
//
// # Startup
//
// [Mirror.Run] loads the snapshot (if a store is configured), reads
// sync/recipes/ once, compares every uid and hash with the index, and fetches
// what differs. Search works as soon as a complete snapshot is loaded. Without
// one, [Mirror.Search] returns a [*LoadingError] with progress until the first
// pass finishes, and [Mirror.Recipe] reads the API directly.
//
// # Locking
//
// mu (RWMutex) guards the index, flags, progress, and the intern table. Index
// entries are immutable once stored; updates replace the pointer. No lock is
// held across a network call except syncMu (one recipe sync at a time), statusMu
// (one status call at a time), and each list's own mutex (one load of that list
// at a time). Lock order is syncMu, then a list mutex or statusMu, then mu;
// bodyCache has its own leaf mutex.
package mirror

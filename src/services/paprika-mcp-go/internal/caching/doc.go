// Package caching keeps a disposable local cache of Paprika data so that search and
// reads do not need one API call per recipe.
//
// # What it holds
//
// A small index entry per recipe (see [IndexEntry]), a short most-recently-used
// cache of recipe bodies, and the small lists (categories, meals, meal types,
// grocery lists, grocery items, aisles, remembered ingredients) as whole lists.
// Entries and lists returned by the cache are shared and must not be modified;
// use paprikaclient.Object.Clone before changing a raw object.
//
// # Freshness
//
// Before serving a read the cache checks sync/status/ if the last check is
// more than a minute old. A resource is downloaded again only when its counter
// changed. Recipes are compared by hash, as opaque strings: new and changed
// recipes are fetched and removed ones leave the index. Each entry also records
// when its recipe was last read (IndexEntry.ReadAt). A recipe older than the
// expiry is due for a re-read even if its hash is unchanged, which catches an
// edit made without a new hash. A pass reads new and changed recipes first,
// then due ones, oldest first, so an interrupted pass resumes with what is
// still due. Stamps of first reads and age-based re-reads are moved earlier by
// a per-uid jitter of up to a tenth of the expiry, so due dates do not bunch.
//
// # Startup
//
// [Cache.Run] loads the snapshot (if a store is configured), reads
// sync/recipes/ once, compares every uid and hash with the index, and fetches
// what differs. Search works as soon as a complete snapshot is loaded. Otherwise
// [Cache.Search] returns a [*LoadingError] until every recipe in the latest list
// is in the index at its listed hash, however many passes that takes; the
// error reports that count out of the list's size. Readiness is rechecked after
// each recipe is stored, and once reached it is never withdrawn. [Cache.Recipe]
// reads the API directly until then.
//
// # Locking
//
// mu (RWMutex) guards the index, flags, progress, and the intern table. Index
// entries are immutable once stored; updates replace the pointer. No lock is
// held across a network call except syncMu (one recipe sync at a time), statusMu
// (one status call at a time), and each list's own mutex (one load of that list
// at a time). Lock order is syncMu, then a list mutex or statusMu, then mu;
// bodyCache has its own leaf mutex.
package caching

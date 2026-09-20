package v050

import "slices"

// maxRecords bounds the per-Adapter record cache. The cache exists to keep the
// per-reconcile scan proportional to live and changed records rather than to
// fleet age, and a fleet whose history has outgrown this bound simply re-reads
// the overflow: the cache is an optimization, never a source of evidence.
const maxRecords = 4096

// cachedRecord is one parsed node record plus the listing metadata that has to
// match, exactly, before it may be reused. It is process memory only: a restart
// performs one full read, which is the pre-cache behavior.
type cachedRecord struct {
	etag string
	size int64
	node Node
}

// clone keeps the cache's Node and the caller's Node from sharing a slice, so a
// reused record cannot be mutated through an earlier pass's copy.
func (n Node) clone() Node {
	n.Ensemble = slices.Clone(n.Ensemble)
	return n
}

// reuse returns the record parsed from an earlier pass when the fresh listing
// proves its body is unchanged. Equality must be exact on both the ETag and the
// size; a key the listing gave no ETag for never matches, so it is re-read.
func (a *Adapter) reuse(entry listed) (Node, bool) {
	if entry.etag == "" {
		return Node{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	cached, ok := a.records[entry.key]
	if !ok || cached.etag != entry.etag || cached.size != entry.size {
		return Node{}, false
	}
	return cached.node.clone(), true
}

// remember stores a record whose body was read and whose ETag the read itself
// confirmed. Callers must not call it for an unconfirmed ETag or a body that
// failed to parse: a parse failure has to recur on the next pass, from S3.
func (a *Adapter) remember(entry listed, n Node) {
	if entry.etag == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.records == nil {
		a.records = map[string]cachedRecord{}
	}
	if _, replacing := a.records[entry.key]; !replacing && len(a.records) >= maxRecords {
		return
	}
	a.records[entry.key] = cachedRecord{etag: entry.etag, size: entry.size, node: n.clone()}
}

// retain evicts every cached key the fresh listing no longer names. A record the
// runtime collected must not survive in memory: its later reappearance is a new
// object and is read again.
func (a *Adapter) retain(entries []listed) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.records) == 0 {
		return
	}
	live := make(map[string]bool, len(entries))
	for _, entry := range entries {
		live[entry.key] = true
	}
	for key := range a.records {
		if !live[key] {
			delete(a.records, key)
		}
	}
}

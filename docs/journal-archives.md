# Retained lifecycle journal archives

The reservation continues to be the only mutable pointer to lifecycle authority.
Once its encoded journal exceeds 180 KiB, the controller stores large JSON fields
in immutable ConfigMap pages in the fleet namespace. The reservation stores a
small archive index instead. This removes the previous 200 KiB annotation cliff
without discarding historical generations, positive stopped receipts, loss
fences, recovery epochs, claims, or completion records.

Each field is paged independently at 128 KiB. Page names are content-addressed
and bound to the reservation UID; each index includes exact byte length, full
SHA-256 digest, and the Kubernetes page UID when assigned. The entire reassembled
canonical journal also has a digest. Small fields stay inline, so ordinary
observation timestamps and operation progress reuse unchanged history pages.
Pages have no owner references and therefore are not garbage-collected with the
CelldFleet. The operator has ConfigMap get/create permissions only for this path.
It never edits, deletes or silently adopts mismatching archive contents.

Pages are durably created and verified before the reservation resourceVersion
CAS publishes the new index. Crashes or conflicting controllers can leave
unreferenced immutable pages, but cannot publish a partial journal. Reconciliation
loads every referenced page and validates the complete journal before applying
any lifecycle decision. Missing, deleting, mutable, replaced, foreign-owned,
truncated, or corrupt pages block reconciliation. The plain journal parser rejects
unresolved archive indexes, preventing callers from acting on partial history.

Retain the fleet namespace and its archive ConfigMaps along with the reservation
and storage. Namespace deletion destroys namespaced archives and makes the
remaining reservation deliberately unusable until exact authority is restored.
No automatic archive garbage collection or namespace-deletion recovery exists.
Pages can accumulate as historical fields change; content addressing reuses
identical pages, but does not bound total retained Kubernetes storage. This is
an operational storage cost, not permission to prune evidence.

Hydrated authority is limited to 16 MiB per reservation and archive indexes remain
limited to 200 KiB. These explicit memory/API budgets fail closed. This supports
much longer histories than the previous annotation-only scheme, but is not an
unlimited-history claim. Monitor reservation size and ConfigMap storage; a future
larger-scale archive backend must preserve the same integrity and fencing rules.

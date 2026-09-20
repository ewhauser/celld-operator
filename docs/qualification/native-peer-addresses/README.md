# Retained-disk peer-address recovery

The released `0.5.1-ewhauser.2` macOS ARM64 binary was tested with two Fleet
members, retained disks and the same stored S3 objects across recovery.

| Advertised endpoint after restart | Acknowledged writes recovered through each node | Loss records |
| --- | --- | --- |
| Changed `localhost:port` | 0/10 | 2 |
| Same `localhost:port` | 10/10 | 0 |

Both schedules acknowledged ten exact IDs/values, including two peer-only writes
while MinIO was paused. Both runtimes self-fenced with exit code 3. MinIO was
killed and restarted on the same mounted tmpfs volume to drop queued requests
while preserving objects. The runtimes restarted together after another 33 seconds.
No disk or bucket state was reset. The matched stable case recovered every value
through each node without loss markers. All owned containers and volumes were removed.

[The comparison](comparison.json) records exact source/binary/harness hashes,
commands, endpoints, exit codes and loss-record bodies. The native reproduction
script and full logs remain in the recorded local artifact directory. An earlier
MinIO507 attempt was excluded; the table compares the matched tmpfs schedules.
The maintained Kubernetes regression is the `faults` integration suite.

Predecessor recovery uses the previous lease's peer address before a new lease
can publish its replacement. PersistentFleet therefore advertises stable
StatefulSet Pod DNS through its existing headless Service, which publishes
addresses before readiness. Native unchanged endpoints demonstrate the protocol;
actual DNS rebinding after Pod IP replacement requires the separate Kind run.

This does not establish arbitrary delayed-peer recovery. The current runtime can
declare permanent bounded loss after a member lease expires by
`max(3 × TTL, 20 seconds)` and a witness request fails. A refused connection or
DNS error can return immediately; the HTTP timeout is not a minimum grace.
Retaining the writer's local image alone is not a complete recovery witness.
Simultaneous reachable-peer recovery, slow attachment, delayed startup and
cross-host recovery are distinct boundaries.

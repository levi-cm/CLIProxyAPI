# History recovery prerequisite for deployment

The combined renewal-routing image passed its small fixture rehearsal, but a
cold synthetic archive with 96,991 requests and 105,429,217 raw bytes did not
become ready within 600 seconds. Production was left running throughout.

Recovery previously called the normal durable lookup insertion for every ID.
That repeatedly rewrote and synced derived leaf buckets. Recovery now batches
one segment at a time and writes each affected leaf once per segment. Memory
remains bounded by segment size, rather than total retained history. Duplicate
IDs retain their first segment; leaf capacity and radix depth limits remain.

Only reconstruction of disposable lookup metadata skips individual file and
directory syncs. Atomic replacement, namespace confinement, unsafe-file checks
and write/close/rename error handling remain. Every startup compares the entire
lookup signature with segment indexes before enabling normal appends; partial
reconstruction is discarded and rebuilt. Authoritative raw history, segment
indexes, ordinary lookup insertions and all policy/ownership writes retain
their existing durability and format. This changes recovery cost, not routing.

Regression coverage includes nested splits, first-segment ownership, durable
appends after recovery, symlink targets and temporary files, missing/corrupt/empty
children, both derived indexes missing, repeated reopen and unchanged raw bytes.

The user subsequently authorized system updates, deployment and a host reboot,
which necessarily entails a maintenance interruption. Before that interruption,
the exact release image must pass the realistic-volume recovery and rollback
rehearsal. Drain established HTTP/WebSocket sessions, preserve the single writer,
retain rollback against current state, and verify all three network bindings.
Do not restore an old policy/history backup over state written by the new image.

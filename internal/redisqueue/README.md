# Usage delivery and replay

This package owns usage queue ordering, retention and delivery. Every nonempty
payload accepted while enabled gets a process-local `queue_seq`, starting at 1.
The process has one random `boot_id`; disabling and re-enabling usage does not
reset either identity or sequence. Subscription refresh controls and the error
channel do not consume usage sequences.

JSON objects gain top-level `queue_seq` and `boot_id` fields before any delivery.
Other payloads retain their original bytes for subscribe and pop. Subscribe
does not drain old records. Records enter the destructive pop queue only when
no subscriber exists, including the existing overflow behavior.

Replay retains records independently of subscribers and pops. Retention uses
`redis-usage-queue-retention-seconds` (default 60, maximum 3600), pruned on enqueue
and read. The oldest records are also evicted when retained payloads exceed
16 MiB or the log exceeds 16,384 records. The byte cap bounds large records;
the record cap bounds metadata overhead for small records. A payload larger
than the byte cap still reaches the legacy delivery path but is immediately
evicted from replay. Disabling usage clears both buffers.

`GET /v8/management/observability/usage/queue?after_seq=N&count=C` reads replay
without consuming records. `after_seq=0` starts at the oldest retained record.
Malformed cursors return 400. `count` defaults to 1, must be positive, and is
clamped to 1000 for replay only. Without `after_seq`, the existing pop response
and count behavior apply. Replay is exposed only on the v8 route.

The response has `boot_id`, `oldest_seq`, `next_seq`, and `records`:

```json
{"boot_id":"example-boot","oldest_seq":8,"next_seq":9,"records":[{"queue_seq":8,"payload":{"id":1,"queue_seq":8,"boot_id":"example-boot"}},{"queue_seq":9,"payload":"raw-record"}]}
```

The envelope always carries the sequence. Valid JSON payloads are JSON values;
non-JSON payloads are encoded as strings, as on the existing HTTP pop path.
For an empty log, `oldest_seq` is the last assigned sequence plus one.
`next_seq` is the last returned sequence, or the input cursor advanced past
pruned records if none remain. Within one boot, a usage collector resuming
after N computes missed records as `max(0, oldest_seq - N - 1)` and persists
`next_seq` after processing the page. A cursor older than retention is accepted.
On a changed `boot_id`, restart with cursor 0; the previous process's remaining
records and final sequence are unavailable, so loss across a restart cannot be
counted exactly. This is an in-memory log, not durable or exactly-once delivery.

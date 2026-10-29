# Analytics measurement contract

Additive producer contract for CPA integration. Existing IDs, token arithmetic,
`accounting_version`, `token_breakdown.quality`, `latency_ms`, `ttft_ms`, and their
units retain their existing meanings. No queue consumer or durable delivery is added.

The following fields are added to each usage queue JSON object:

```json
{
  "instance_id": "codex-primary",
  "usage_presence": "reported",
  "usage_fields": {
    "input_tokens": true,
    "output_tokens": true,
    "reasoning_tokens": false,
    "cache_read_tokens": false,
    "cache_creation_tokens": false,
    "total_tokens": true
  },
  "ttft_method": "first-byte",
  "timing": {
    "origin": "execution-start",
    "upstream_start_ms": 3,
    "first_response_ms": 12,
    "first_content_ms": null,
    "recorded_ms": 45
  },
  "record_kind": "execution",
  "parent_execution_id": ""
}
```

`instance_id` is optional operator-supplied provenance from `CLIPROXY_INSTANCE_ID`,
captured at process start. Set it to the matching Prox deployment manifest instance
ID. Missing or invalid values remain absent; historical source attribution is not
inferred from credentials, ports or model names. IDs use lowercase letters,
digits and hyphens, start with a letter and are at most 64 characters.

- `usage_presence`: `reported` when a supported parser observes at least one
  numeric usage field; otherwise `unknown`. Zero is reported when explicitly
  numeric zero. This is independent of arithmetic quality. Legacy SDK callers
  without presence metadata remain unknown, even if token totals are nonzero.
- `usage_fields`: object shown above, or `null` for uninstrumented/absent usage.
  True means that raw component was reported (including zero), not inferred by
  arithmetic. False does not mean zero. Fields merge across Claude stream events.
  Initially instrumented parsers: OpenAI-compatible (including Codex and Kimi),
  Claude. Other parser families remain unknown until instrumented.
- `ttft_method`: `substantive-content`, `first-byte`, `fallback-packet`, or
  `unavailable`. The existing `ttft_ms` still uses its historical upstream-start
  origin. Do not subtract it from `latency_ms`.
- `timing`: object shown above, or `null` for uninstrumented SDK records.
  Every offset is integer milliseconds from the reporter's execution start,
  measured using the same process monotonic clock. Missing observations are null;
  zero is a valid measured duration. `first_response_ms` is the first observed
  body byte/packet (not HTTP headers); `first_content_ms` is only present when
  protocol-aware content detection ran. `recorded_ms` is the record snapshot,
  not proof of client delivery or inbound completion. `upstream_start_ms` marks
  the existing timing start, normally transport dispatch; response-only wrappers
  start when the response is observed. These are not separate retry attempt IDs.
- `record_kind`: `execution`, `supplementary`, or `unknown` for legacy SDK records.
  Supplementary records retain their distinct existing `execution_id` and carry
  the original execution ID in `parent_execution_id`. They are model accounting,
  not an additional upstream attempt. This relationship alone does not establish
  additive versus overlapping cost; consumers must not blindly sum parent costs.
- `parent_execution_id`: string; empty for regular executions or unknown parent.

Claude and Kimi stream finalization emits one record even without usage, with
unknown presence. Failure/cancellation retains observed partial usage and failure
status. Claude's existing completed-terminal cancellation behavior is preserved.
Historical rows lacking these additions remain unknown, not backfilled by inference.
This contract does not claim ingress completeness, durable delivery, or exact
client outcomes. The parent owns those integration and release requirements.

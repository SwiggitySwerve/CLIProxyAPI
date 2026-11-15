-- Frozen pre-swap schema used only by synthetic label-compatibility tests.

CREATE TABLE usage_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT, timestamp_ms INTEGER, provider TEXT, requested_model TEXT,
    model TEXT, resolved_model TEXT, account_snapshot TEXT, auth_label_snapshot TEXT, auth_index TEXT,
    api_key_hash TEXT, failed INTEGER, input_tokens INTEGER, output_tokens INTEGER, cached_tokens INTEGER,
    reasoning_tokens INTEGER, total_tokens INTEGER, normalized_uncached_input_tokens INTEGER,
    normalized_cache_read_tokens INTEGER, normalized_cache_creation_tokens INTEGER,
    normalized_total_input_tokens INTEGER, latency_ms INTEGER, ttft_ms INTEGER, service_tier TEXT);
CREATE TABLE model_prices (model TEXT, prompt_per_1m REAL, completion_per_1m REAL, cache_per_1m REAL,
    cache_read_per_1m REAL, cache_creation_per_1m REAL, cache_read_configured INTEGER,
    cache_creation_configured INTEGER);
CREATE TABLE model_price_context_tiers (model TEXT, threshold_tokens INTEGER, prompt_per_1m REAL,
    completion_per_1m REAL, cache_per_1m REAL, cache_read_per_1m REAL, cache_creation_per_1m REAL,
    prompt_configured INTEGER, completion_configured INTEGER, cache_configured INTEGER,
    cache_read_configured INTEGER, cache_creation_configured INTEGER);
CREATE TABLE model_price_service_tiers (model TEXT, mode TEXT, service_tier TEXT, prompt_per_1m REAL,
    completion_per_1m REAL, cache_per_1m REAL, cache_read_per_1m REAL, cache_creation_per_1m REAL,
    prompt_configured INTEGER, completion_configured INTEGER, cache_configured INTEGER,
    cache_read_configured INTEGER, cache_creation_configured INTEGER);
CREATE TABLE usage_data_migrations (name TEXT PRIMARY KEY, status TEXT, applied_rows INTEGER,
    updated_at_ms INTEGER);
INSERT INTO model_prices VALUES ('m1', 2.0, 10.0, NULL, NULL, NULL, 0, 0);
INSERT INTO model_price_context_tiers VALUES ('m1', 1000, 4.0, 20.0, NULL, NULL, NULL, 1, 1, 0, 0, 0);
INSERT INTO usage_data_migrations VALUES ('usage_cache_accounting_v2', 'completed', 0, 1);

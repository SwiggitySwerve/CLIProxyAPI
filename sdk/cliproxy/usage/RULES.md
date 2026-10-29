# Usage measurement rules

This package owns the public usage record, measurement presence and accounting
contracts. Token arithmetic remains in accounting.go. Measurement types and
presence merging live in measurement.go; producers report observations and sinks
serialize them without deriving presence from numeric totals or accounting quality.

See ../../../docs/analytics-measurement-contract.md for the additive JSON contract.
Unknown is distinct from measured zero. Timing offsets share execution start;
legacy TTFT retains its existing origin and units. Supplementary accounting is
linked to an execution and must not be counted as another upstream attempt.

Focused measurement, reporter, stream and serialization tests enforce these
properties. They do not establish durable delivery or ingress completeness.

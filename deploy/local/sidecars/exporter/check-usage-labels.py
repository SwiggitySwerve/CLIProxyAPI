#!/usr/bin/env python3
"""Required Step E gate: compare legacy metric label sets with the Prox endpoint.

Run before replacing the old exporter. Exit 0: sets match; 1: differences; 2: bad input.
Files allow offline tests. Live defaults read only loopback HTTP, never either database.
"""
import argparse
import json
import re
import sys
import urllib.request
from pathlib import Path

LABELS = ("account", "client", "provider", "model")
SAMPLE = re.compile(r'^(cliproxy_requests_total|cliproxy_tokens_total)\{(.*)\}\s+\S+(?:\s+\d+)?$')
LABEL = re.compile(r'([a-zA-Z_][a-zA-Z0-9_]*)="((?:\\.|[^"\\])*)"')


def decode_label(value):
    def escaped(match):
        values = {'n': '\n', '\\': '\\', '"': '"'}
        if match[1] not in values:
            raise ValueError("unsupported Prometheus label escape")
        return values[match[1]]
    return re.sub(r'\\(.)', escaped, value)


def old_label_sets(text):
    result = {name: set() for name in LABELS}
    found = False
    for line in text.splitlines():
        match = SAMPLE.fullmatch(line)
        if not match:
            continue
        labels = {key: decode_label(value) for key, value in LABEL.findall(match[2])}
        if "source" in labels:
            raise ValueError("baseline already has a source label; capture the old exporter before swapping")
        if any(name not in labels for name in LABELS):
            raise ValueError("legacy usage sample is missing a required label")
        for name in LABELS:
            result[name].add(labels[name])
        found = True
    if not found:
        raise ValueError("baseline has no legacy request/token samples")
    return result


def prox_label_sets(data):
    if data.get("schema_version") != 1 or data.get("rate_book_loaded") is not True or data.get("client_aliases_loaded") is not True:
        raise ValueError("Prox rates and aliases must have loaded before cutover")
    if not isinstance(data.get("groups"), list):
        raise ValueError("invalid Prox exporter groups")
    result = {name: set() for name in LABELS}
    for group in data["groups"]:
        for name in LABELS:
            value = group[name]
            if value is not None and not isinstance(value, str):
                raise ValueError("invalid Prox label value")
            # Same visible value as the deployed Metrics.esc (escapes are decoded on the old side).
            result[name].add(("unknown" if value in (None, "") else value).replace('\n', ' '))
    return result


def compare(old, prox):
    return {name: {"only_old": sorted(old[name] - prox[name]),
                   "only_prox": sorted(prox[name] - old[name])}
            for name in LABELS if old[name] != prox[name]}


def read_url(url, key=None):
    request = urllib.request.Request(url, headers={"Authorization": f"Bearer {key}"} if key else {})
    with urllib.request.urlopen(request, timeout=4) as response:
        return response.read().decode('utf-8')


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    old = parser.add_mutually_exclusive_group()
    old.add_argument('--old-metrics', type=Path)
    old.add_argument('--old-metrics-url', default='http://127.0.0.1:9787/metrics')
    parser.add_argument('--exporter-json', type=Path, help='saved /v1/exporter response, for offline checks')
    parser.add_argument('--usage-url', default='http://127.0.0.1:18320/v1/exporter')
    parser.add_argument('--key-file', type=Path, default=Path.home()/'.config/cli-proxy-rerank/management-key')
    args = parser.parse_args(argv)
    try:
        text = args.old_metrics.read_text() if args.old_metrics else read_url(args.old_metrics_url)
        if args.exporter_json:
            data = json.loads(args.exporter_json.read_text())
        else:
            key = args.key_file.read_text().strip()
            if not key:
                raise ValueError("management key file is empty")
            data = json.loads(read_url(args.usage_url, key))
        baseline, current = old_label_sets(text), prox_label_sets(data)
        differences = compare(baseline, current)
    except Exception:
        # Do not echo HTTP exception strings, headers or response bodies that may contain secrets.
        print('Label gate could not read valid old metrics and ready Prox data; swap blocked.', file=sys.stderr)
        return 2
    if differences:
        print(json.dumps({"swap_blocked": True, "differences": differences}, ensure_ascii=False, indent=2))
        return 1
    print(json.dumps({"swap_blocked": False, "distinct_labels": {name: len(current[name]) for name in LABELS}}))
    return 0


if __name__ == '__main__':
    sys.exit(main())

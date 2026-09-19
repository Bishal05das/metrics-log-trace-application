#!/usr/bin/env bash
# Apply the Elasticsearch ILM policy, index template and Kibana data view.
#
# ORDER MATTERS, and this is the one piece of ELK setup that is genuinely
# unforgiving: an index template only applies to indices created AFTER it
# exists. If Logstash writes a document first, Elasticsearch invents a mapping
# by guessing from the first value it sees — every string becomes text+keyword
# (double storage), duration_ms may become a string that sorts 9ms above 100ms,
# and you cannot change any of it without reindexing. Run this before, or
# immediately after, standing up the stack; it is idempotent.
set -euo pipefail

ES="${ES_URL:-http://localhost:9201}"
KB="${KIBANA_URL:-http://localhost:5602}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

wait_for() {
  local name="$1" url="$2" tries="${3:-60}"
  printf '  waiting for %s' "$name"
  for _ in $(seq 1 "$tries"); do
    if curl -sf -o /dev/null "$url"; then echo " ok"; return 0; fi
    printf '.'; sleep 2
  done
  echo " TIMED OUT"
  echo "  $name did not become ready at $url" >&2
  return 1
}

wait_for "elasticsearch" "$ES/_cluster/health"

echo "  applying ILM policy orders-logs-policy"
curl -sf -X PUT "$ES/_ilm/policy/orders-logs-policy" \
  -H 'Content-Type: application/json' \
  --data-binary "@$HERE/elasticsearch/ilm-policy.json" > /dev/null
echo "    ok"

echo "  applying index template orders-logs"
curl -sf -X PUT "$ES/_index_template/orders-logs" \
  -H 'Content-Type: application/json' \
  --data-binary "@$HERE/elasticsearch/index-template.json" > /dev/null
echo "    ok"

# Kibana is optional for the pipeline to work; only the views need it.
#
# Saved objects are IMPORTED from a file in git rather than created through the
# API one at a time, for the same reason the Grafana dashboards are provisioned:
# a view that exists only in Kibana's saved-object index cannot be reviewed or
# diffed, and disappears with the container. The file also owns the data view,
# so its id is fixed and the saved searches' references always resolve.
if wait_for "kibana" "$KB/api/status" 90; then
  echo "  importing Kibana saved objects"
  out=$(curl -sf -X POST "$KB/api/saved_objects/_import?overwrite=true" \
    -H 'kbn-xsrf: true' \
    --form "file=@$HERE/kibana/saved-objects.ndjson" 2>&1) || {
      echo "    import failed: $out" >&2
      exit 1
    }
  python3 - "$out" <<'PY'
import json, sys
d = json.loads(sys.argv[1])
print(f"    {d.get('successCount', 0)} object(s) imported")
for e in d.get("errors", []) or []:
    print(f"    ERROR {e.get('type')}/{e.get('id')}: {e.get('error', {}).get('type')}", file=sys.stderr)
PY

  # An earlier run created the data view through the API, which assigns a random
  # id. Left in place it shows up as a second, identical "orders-logs-*" entry in
  # every data-view picker. Remove any duplicate that is not the one we own.
  python3 - "$KB" <<'PY'
import json, sys, urllib.request
kb = sys.argv[1]
req = urllib.request.Request(
    f"{kb}/api/saved_objects/_find?type=index-pattern&per_page=100",
    headers={"kbn-xsrf": "true"})
found = json.load(urllib.request.urlopen(req, timeout=15))
for o in found.get("saved_objects", []):
    if o["attributes"].get("title") == "orders-logs-*" and o["id"] != "orders-logs":
        d = urllib.request.Request(
            f"{kb}/api/saved_objects/index-pattern/{o['id']}?force=true",
            headers={"kbn-xsrf": "true"}, method="DELETE")
        urllib.request.urlopen(d, timeout=15)
        print(f"    removed duplicate data view {o['id']}")
PY
fi

echo
echo "  Kibana    $KB/app/dashboards"
echo "  Discover  $KB/app/discover"
echo "  ES        $ES/orders-logs-*/_search?size=1&pretty"

#!/usr/bin/env bash
set -euo pipefail

base_url=${BASE_URL:-http://127.0.0.1:8007}
db() { docker compose exec -T db psql -X -v ON_ERROR_STOP=1 -U images -d images "$@"; }
sql() { db -Atqc "$1"; }
wait_for() {
  local timeout=$1
  shift
  local deadline=$((SECONDS + timeout))
  until "$@"; do
    if (( SECONDS >= deadline )); then
      echo "Timed out waiting for $*" >&2
      return 1
    fi
    sleep 0.25
  done
}

cleanup() {
  sql 'DROP TRIGGER IF EXISTS qa_pause_finish ON image_jobs; DROP FUNCTION IF EXISTS qa_pause_finish();' || true
  docker compose start worker >/dev/null 2>&1 || true
  rm -f "$fixture"
}
fixture=$(mktemp)
trap cleanup EXIT
base64 -d qa/fixture.base64 > "$fixture"
username="qa_recovery_$(date +%s%N)"
user=$(jq -nc --arg username "$username" '{username:$username,password:"qa-recovery-password"}')
curl -fsS -H 'Content-Type: application/json' -d "$user" "$base_url/register" >/dev/null
token=$(curl -fsS -H 'Content-Type: application/json' -d "$user" "$base_url/login" | jq -er .token)

# This trigger executes only in the isolated QA stack, only for this run's user.
db -q <<SQL
CREATE FUNCTION qa_pause_finish() RETURNS trigger LANGUAGE plpgsql AS \$\$
BEGIN
  IF NEW.owner = '$username' AND NEW.attempts = 1 AND NEW.status = 'succeeded' THEN
    PERFORM pg_sleep(60);
  END IF;
  RETURN NEW;
END
\$\$;
CREATE TRIGGER qa_pause_finish BEFORE UPDATE ON image_jobs
FOR EACH ROW EXECUTE FUNCTION qa_pause_finish();
SQL

id=$(curl -fsS -H "Authorization: Bearer $token" -H 'Content-Type: image/png' \
  --data-binary @"$fixture" "$base_url/task?filter=Negative" | jq -er .task_id)
[[ "$id" =~ ^[0-9a-f-]{36}$ ]]
waiting_worker() {
  worker_pid=$(sql "SELECT pid FROM pg_stat_activity WHERE wait_event='PgSleep'
    AND query LIKE 'UPDATE image_jobs SET status=%' LIMIT 1")
  [[ "$worker_pid" =~ ^[0-9]+$ ]]
}
wait_for 25 waiting_worker
lease=$(sql "SELECT lease_token FROM image_jobs WHERE id='$id'")
docker compose exec -T processor python -c \
  'import sys; from processor.app import s3, bucket; s3.head_object(Bucket=bucket, Key=sys.argv[1])' \
  "result/$id/$lease"

docker compose kill -s SIGKILL worker
sql "SELECT pg_cancel_backend($worker_pid)" >/dev/null
docker compose start worker
succeeded() {
  [[ $(curl -fsS -H "Authorization: Bearer $token" "$base_url/status/$id" | jq -r .status) == succeeded ]]
}
wait_for 45 succeeded
curl -fsS -H "Authorization: Bearer $token" "$base_url/result/$id" >/dev/null
[[ $(sql "SELECT attempts FROM image_jobs WHERE id='$id'") == 2 ]]
final_result=$(sql "SELECT result_key FROM image_jobs WHERE id='$id'")
[[ "$final_result" != "result/$id/$lease" ]]

# Republish the same ID through RabbitMQ after success.
sql "UPDATE image_outbox SET published_at=NULL,available_at=now() WHERE job_id='$id'"
published() {
  [[ $(sql "SELECT published_at IS NOT NULL FROM image_outbox WHERE job_id='$id'") == t ]]
}
drained() {
  local queue
  queue=$(docker compose exec -T rabbitmq rabbitmqctl -q list_queues name messages_ready messages_unacknowledged)
  awk '$1 == "image.jobs.v2" && $2 == 0 && $3 == 0 {found=1} END {exit !found}' <<< "$queue"
}
wait_for 20 published
wait_for 20 drained
[[ $(sql "SELECT attempts FROM image_jobs WHERE id='$id'") == 2 ]]
[[ $(sql "SELECT result_key FROM image_jobs WHERE id='$id'") == "$final_result" ]]
echo "Recovery and duplicate-delivery checks passed for $id"

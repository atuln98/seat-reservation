#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
	printf 'usage: %s <BASE_URL>\n' "$0" >&2
	exit 2
fi

for command in curl; do
	if ! command -v "$command" >/dev/null 2>&1; then
		printf 'missing required command: %s\n' "$command" >&2
		exit 2
	fi
done

: "${ADMIN_EMAIL:?ADMIN_EMAIL is required}"
: "${ADMIN_PASSWORD:?ADMIN_PASSWORD is required}"

base_url=${1%/}
requests=${BURST_REQUESTS:-20000}
concurrency=${BURST_CONCURRENCY:-$requests}
users=${BURST_USERS:-32}
timeout_seconds=${BURST_TIMEOUT_SECONDS:-120}

if (( requests < 12 || concurrency < 1 || users < 8 || timeout_seconds < 1 )); then
	printf 'BURST_REQUESTS must be at least 12, BURST_USERS must be at least 8, and concurrency and timeout must be positive\n' >&2
	exit 2
fi

if (( concurrency > requests )); then
	concurrency=$requests
fi

temporary_directory=$(mktemp -d)
trap 'rm -rf "$temporary_directory"' EXIT HUP INT TERM

json_escape() {
	local value=$1
	value=${value//\\/\\\\}
	value=${value//\"/\\\"}
	value=${value//$'\n'/\\n}
	value=${value//$'\r'/\\r}
	value=${value//$'\t'/\\t}
	printf '%s' "$value"
}

json_string() {
	local key=$1
	sed -nE "s/.*\"$key\":\"([^\"]*)\".*/\\1/p"
}

json_number() {
	local key=$1
	sed -nE "s/.*\"$key\":([0-9]+).*/\\1/p"
}

metric_number() {
	local metric=$1
	local show=$2
	local source=$3
	awk -v sample="$metric{show_id=\"$show\"}" '
		$1 == sample {
			count++
			if ($2 !~ /^[0-9]+([.]0+)?$/) {
				invalid = 1
			}
			value = $2
		}
		END {
			if (count != 1 || invalid) {
				exit 1
			}
			printf "%.0f\n", value
		}
	' "$source"
}

call_api() {
	local expected_status=$1
	local method=$2
	local path=$3
	local token=$4
	local payload=$5
	local body_file
	local status
	local curl_arguments

	body_file=$(mktemp "$temporary_directory/setup.XXXXXX")
	curl_arguments=(
		--silent
		--show-error
		--connect-timeout 10
		--max-time "$timeout_seconds"
		--request "$method"
		--header 'Content-Type: application/json'
		--output "$body_file"
		--write-out '%{http_code}'
	)
	if [[ -n $token ]]; then
		curl_arguments+=(--header "Authorization: Bearer $token")
	fi
	if [[ -n $payload ]]; then
		curl_arguments+=(--data "$payload")
	fi
	status=$(
		curl "${curl_arguments[@]}" "$base_url$path"
	)
	if [[ $status != "$expected_status" ]]; then
		printf 'request %s %s returned %s: ' "$method" "$path" "$status" >&2
		cat "$body_file" >&2
		printf '\n' >&2
		return 1
	fi
	cat "$body_file"
	rm -f "$body_file"
}

admin_payload=$(printf \
	'{"email":"%s","password":"%s"}' \
	"$(json_escape "$ADMIN_EMAIL")" \
	"$(json_escape "$ADMIN_PASSWORD")")
admin_response=$(call_api 200 POST /auth/login "" "$admin_payload")
admin_token=$(json_string access_token <<<"$admin_response")
if [[ -z $admin_token ]]; then
	printf 'login response did not contain access_token\n' >&2
	exit 1
fi

run_id=$(printf '%s-%s' "$(date +%s)" "$$")
tokens=()
for (( index = 0; index <= users; index++ )); do
	email="burst-$run_id-$index@example.com"
	user_payload=$(printf '{"email":"%s","password":"password123"}' "$email")
	user_response=$(call_api 201 POST /auth/register "" "$user_payload")
	user_token=$(json_string access_token <<<"$user_response")
	if [[ -z $user_token ]]; then
		printf 'registration response did not contain access_token\n' >&2
		exit 1
	fi
	tokens+=("$user_token")
done

show_payload=$(printf \
	'{"name":"burst-%s","seats":["HOT","TAKEN","PROBE","IDEMP","IDEMP_ALT","CANCEL","A1","A2","A3","A4","A5","A6","A7","A8","A9","A10"],"price_paise":25000,"per_user_limit":16}' \
	"$run_id")
show_response=$(call_api 201 POST /shows "$admin_token" "$show_payload")
show_id=$(json_string show_id <<<"$show_response")
if [[ -z $show_id ]]; then
	printf 'show response did not contain show_id\n' >&2
	exit 1
fi

taken_payload=$(printf \
	'{"seats":["TAKEN"],"idempotency_key":"%s-taken"}' \
	"$run_id")
call_api 201 POST "/shows/$show_id/reserve" "${tokens[$users]}" "$taken_payload" >/dev/null

idempotency_key="$run_id-idempotency"
idempotency_payload=$(printf \
	'{"seats":["IDEMP"],"idempotency_key":"%s"}' \
	"$idempotency_key")
idempotency_original=$(call_api 201 POST "/shows/$show_id/reserve" "${tokens[$users]}" "$idempotency_payload")
idempotency_replay=$(call_api 200 POST "/shows/$show_id/reserve" "${tokens[$users]}" "$idempotency_payload")
idempotency_original_id=$(json_string reservation_id <<<"$idempotency_original")
idempotency_replay_id=$(json_string reservation_id <<<"$idempotency_replay")
idempotency_conflict_payload=$(printf \
	'{"seats":["IDEMP_ALT"],"idempotency_key":"%s"}' \
	"$idempotency_key")
idempotency_conflict=$(call_api 409 POST "/shows/$show_id/reserve" "${tokens[$users]}" "$idempotency_conflict_payload")
idempotency_conflict_reason=$(json_string error <<<"$idempotency_conflict")
idempotency_show=$(call_api 200 GET "/shows/$show_id" "" "")
idempotency_confirmed=$(json_number confirmed <<<"$idempotency_show")
idempotency_alt_available=false
if [[ $idempotency_show == *'"seat_number":"IDEMP_ALT","state":"available"'* ]]; then
	idempotency_alt_available=true
fi
printf 'idempotency original_id=%s replay_id=%s confirmed=%s conflict=%s alternate_seat_available=%s\n' \
	"$idempotency_original_id" \
	"$idempotency_replay_id" \
	"$idempotency_confirmed" \
	"$idempotency_conflict_reason" \
	"$idempotency_alt_available"
if [[ -z $idempotency_original_id ||
	$idempotency_replay_id != "$idempotency_original_id" ||
	$idempotency_replay != "$idempotency_original" ||
	$idempotency_conflict_reason != idempotency_conflict ||
	$idempotency_confirmed != 2 ||
	$idempotency_alt_available != true ]]; then
	printf 'result=fail\n' >&2
	exit 1
fi

cancellation_key="$run_id-cancellation"
cancellation_payload=$(printf \
	'{"seats":["CANCEL"],"idempotency_key":"%s"}' \
	"$cancellation_key")
cancellation_original=$(call_api 201 POST "/shows/$show_id/reserve" "${tokens[$users]}" "$cancellation_payload")
cancellation_original_id=$(json_string reservation_id <<<"$cancellation_original")
cancellation_response=$(call_api 200 POST "/reservations/$cancellation_original_id/cancel" "${tokens[$users]}" "")
cancellation_repeat=$(call_api 200 POST "/reservations/$cancellation_original_id/cancel" "${tokens[$users]}" "")
cancellation_status=$(json_string status <<<"$cancellation_response")
cancellation_repeat_status=$(json_string status <<<"$cancellation_repeat")
cancellation_replay=$(call_api 200 POST "/shows/$show_id/reserve" "${tokens[$users]}" "$cancellation_payload")
cancellation_replay_id=$(json_string reservation_id <<<"$cancellation_replay")
cancellation_replay_status=$(json_string status <<<"$cancellation_replay")
cancellation_new_payload=$(printf \
	'{"seats":["CANCEL"],"idempotency_key":"%s-cancellation-new"}' \
	"$run_id")
cancellation_new=$(call_api 201 POST "/shows/$show_id/reserve" "${tokens[$users]}" "$cancellation_new_payload")
cancellation_new_id=$(json_string reservation_id <<<"$cancellation_new")
printf 'cancellation original_id=%s replay_id=%s replay_status=%s rebooked_id=%s\n' \
	"$cancellation_original_id" \
	"$cancellation_replay_id" \
	"$cancellation_replay_status" \
	"$cancellation_new_id"
if [[ -z $cancellation_original_id ||
	$cancellation_status != cancelled ||
	$cancellation_repeat_status != cancelled ||
	$cancellation_replay_id != "$cancellation_original_id" ||
	$cancellation_replay_status != cancelled ||
	-z $cancellation_new_id ||
	$cancellation_new_id == "$cancellation_original_id" ]]; then
	printf 'result=fail\n' >&2
	exit 1
fi

mkdir "$temporary_directory/bodies"
curl_config="$temporary_directory/burst.curl"
statuses="$temporary_directory/statuses"
outcomes="$temporary_directory/outcomes"
all_or_nothing_expected=0
{
	printf 'parallel\n'
	printf 'parallel-max = %d\n' "$concurrency"
} >"$curl_config"

for (( index = 0; index < requests; index++ )); do
	pattern=$((index % 12))
	case $pattern in
		0 | 1 | 2 | 3 | 4 | 5)
			seats=HOT
			category=hot_seat
			token=${tokens[$((index % (users - 6)))]}
			;;
		6)
			seats=A1,A2
			category=multi_a
			token=${tokens[$((users - 6))]}
			;;
		7)
			seats=A2,A3
			category=multi_b
			token=${tokens[$((users - 5))]}
			;;
		8)
			seats=A4,A5,A6
			category=multi_c
			token=${tokens[$((users - 4))]}
			;;
		9)
			seats=A6,A7
			category=multi_d
			token=${tokens[$((users - 3))]}
			;;
		10)
			seats=A8,A9,A10
			category=multi_e
			token=${tokens[$((users - 2))]}
			;;
		11)
			seats=PROBE,TAKEN
			category=all_or_nothing
			token=${tokens[$((users - 1))]}
			all_or_nothing_expected=$((all_or_nothing_expected + 1))
			;;
	esac
	seat_values=${seats//,/\",\"}
	payload=$(printf \
		'{"seats":["%s"],"idempotency_key":"%s-%d"}' \
		"$seat_values" "$run_id" "$index")
	config_payload=${payload//\"/\\\"}
	{
		if (( index > 0 )); then
			printf 'next\n'
		fi
		printf 'url = "%s/shows/%s/reserve"\n' "$base_url" "$show_id"
		printf 'request = "POST"\n'
		printf 'header = "Authorization: Bearer %s"\n' "$token"
		printf 'header = "Content-Type: application/json"\n'
		printf 'data = "%s"\n' "$config_payload"
		printf 'output = "%s/bodies/%d"\n' "$temporary_directory" "$index"
		printf 'write-out = "%d %%{http_code} %s %s\\n"\n' "$index" "$seats" "$category"
		printf 'silent\n'
		printf 'show-error\n'
		printf 'connect-timeout = 10\n'
		printf 'max-time = %d\n' "$timeout_seconds"
	} >>"$curl_config"
done

started_at=$SECONDS
curl --config "$curl_config" >"$statuses" || true
duration_seconds=$((SECONDS - started_at))

: >"$outcomes"
while read -r index status seats category; do
	if [[ ! $index =~ ^[0-9]+$ || ! $status =~ ^[0-9]{3}$ ]]; then
		continue
	fi
	body_file="$temporary_directory/bodies/$index"
	case $status in
		201)
			reservation_id=$(sed -nE "s/.*\"reservation_id\":\"([^\"]*)\".*/\1/p" "$body_file")
			printf "confirmed\t201\t%s\t%s\t%s\n" "$seats" "$category" "$reservation_id" >>"$outcomes"
			;;
		409)
			reason=$(sed -nE "s/.*\"error\":\"([^\"]*)\".*/\1/p" "$body_file")
			if [[ -z $reason ]]; then
				reason=unknown
			fi
			printf "declined\t%s\t%s\t%s\t\n" "$reason" "$seats" "$category" >>"$outcomes"
			;;
		5??)
			printf "server_error\t%s\t%s\t%s\t\n" "$status" "$seats" "$category" >>"$outcomes"
			;;
		000)
			printf "client_error\t000\t%s\t%s\t\n" "$seats" "$category" >>"$outcomes"
			;;
		*)
			printf "unexpected\t%s\t%s\t%s\t\n" "$status" "$seats" "$category" >>"$outcomes"
			;;
	esac
done <"$statuses"

completed=$(wc -l <"$outcomes" | tr -d ' ')
confirmed=$(awk -F '\t' '$1 == "confirmed" {count++} END {print count+0}' "$outcomes")
server_errors=$(awk -F '\t' '$1 == "server_error" {count++} END {print count+0}' "$outcomes")
reported_client_errors=$(awk -F '\t' '$1 == "client_error" {count++} END {print count+0}' "$outcomes")
missing_results=$((requests - completed))
client_errors=$((reported_client_errors + missing_results))
unexpected=$(awk -F '\t' '$1 == "unexpected" {count++} END {print count+0}' "$outcomes")
seat_declines=$(awk -F '\t' '$1 == "declined" && $2 == "seats_unavailable" {count++} END {print count+0}' "$outcomes")
all_or_nothing_declines=$(awk -F '\t' \
	'$1 == "declined" && $2 == "seats_unavailable" && $4 == "all_or_nothing" {count++} END {print count+0}' \
	"$outcomes")
seat_stats=$(awk -F '\t' '
	$1 == "confirmed" {
		count = split($3, seats, ",")
		for (position = 1; position <= count; position++) {
			owners[seats[position]]++
		}
	}
	END {
		unique = 0
		duplicates = 0
		for (seat in owners) {
			unique++
			if (owners[seat] > 1) {
				duplicates++
			}
		}
		print unique, duplicates
	}' "$outcomes")
successful_seats=${seat_stats%% *}
duplicate_seats=${seat_stats##* }
reservation_ids=$(awk -F '\t' '$1 == "confirmed" && $5 != "" {print $5}' "$outcomes" | sort -u | wc -l | tr -d ' ')

printf 'requests=%d concurrency=%d duration_seconds=%d confirmed=%d server_5xx=%d client_errors=%d\n' \
	"$requests" "$concurrency" "$duration_seconds" "$confirmed" "$server_errors" "$client_errors"
awk -F '\t' '$1 == "confirmed" {counts[$3]++} END {for (seats in counts) print seats, counts[seats]}' "$outcomes" |
	sort |
	while read -r seats count; do
		printf 'confirmed seats=%s count=%d\n' "$seats" "$count"
	done
awk -F '\t' '$1 == "declined" {counts[$2]++} END {for (reason in counts) print reason, counts[reason]}' "$outcomes" |
	sort |
	while read -r reason count; do
		printf 'declined reason=%s count=%d\n' "$reason" "$count"
	done
printf 'all_or_nothing attempted=%d declined=%d probe_seat=PROBE conflicting_seat=TAKEN\n' \
	"$all_or_nothing_expected" "$all_or_nothing_declines"
printf 'ownership unique_confirmed_seats=%d duplicate_confirmed_seats=%d reservation_ids=%d\n' \
	"$successful_seats" "$duplicate_seats" "$reservation_ids"

final_response=$(call_api 200 GET "/shows/$show_id" "" "")
available=$(json_number available <<<"$final_response")
held=$(json_number held <<<"$final_response")
confirmed_seats=$(json_number confirmed <<<"$final_response")
total=$(json_number total_seats <<<"$final_response")
if [[ -z $available || -z $held || -z $confirmed_seats || -z $total ]]; then
	printf 'show response did not contain reconciliation counts\n' >&2
	exit 1
fi
probe_available=false
if [[ $final_response == *'"seat_number":"PROBE","state":"available"'* ]]; then
	probe_available=true
fi
idempotency_alt_available=false
if [[ $final_response == *'"seat_number":"IDEMP_ALT","state":"available"'* ]]; then
	idempotency_alt_available=true
fi
reconciled=false
if (( available + held + confirmed_seats == total )); then
	reconciled=true
fi
printf 'final available=%d held=%d confirmed=%d total=%d reconciled=%s probe_available=%s idempotency_alternate_available=%s\n' \
	"$available" \
	"$held" \
	"$confirmed_seats" \
	"$total" \
	"$reconciled" \
	"$probe_available" \
	"$idempotency_alt_available"

if (( confirmed != 4 ||
	seat_declines != requests - 4 ||
	all_or_nothing_declines != all_or_nothing_expected ||
	server_errors != 0 ||
	client_errors != 0 ||
	unexpected != 0 ||
	held != 0 ||
	duplicate_seats != 0 ||
	reservation_ids != confirmed ||
	confirmed_seats != successful_seats + 3 ||
	available + confirmed_seats != total ||
	total != 16 )) ||
	[[ $probe_available != true || $idempotency_alt_available != true ]]; then
	printf 'result=fail\n' >&2
	exit 1
fi

limit_show_payload=$(printf \
	'{"name":"burst-%s-limit","seats":["L1","L2","L3","L4","L5","L6","L7","L8","L9","L10"],"price_paise":25000,"per_user_limit":4}' \
	"$run_id")
limit_show_response=$(call_api 201 POST /shows "$admin_token" "$limit_show_payload")
limit_show_id=$(json_string show_id <<<"$limit_show_response")
if [[ -z $limit_show_id ]]; then
	printf 'limit show response did not contain show_id\n' >&2
	exit 1
fi

mkdir "$temporary_directory/limit-bodies" "$temporary_directory/limit-results"
limit_gate="$temporary_directory/limit-start"
for (( index = 1; index <= 10; index++ )); do
	(
		while [[ ! -e $limit_gate ]]; do
			sleep 0.01
		done
		body_file="$temporary_directory/limit-bodies/$index"
		status=$(
			curl \
				--silent \
				--show-error \
				--connect-timeout 10 \
				--max-time "$timeout_seconds" \
				--request POST \
				--header "Authorization: Bearer ${tokens[0]}" \
				--header "Content-Type: application/json" \
				--data "{\"seats\":[\"L$index\"],\"idempotency_key\":\"$run_id-limit-$index\"}" \
				--output "$body_file" \
				--write-out "%{http_code}" \
				"$base_url/shows/$limit_show_id/reserve"
		) || status=000
		reason=
		if [[ $status == 409 ]]; then
			reason=$(sed -nE "s/.*\"error\":\"([^\"]*)\".*/\1/p" "$body_file")
		fi
		printf '%s\t%s\n' "$status" "$reason" >"$temporary_directory/limit-results/$index"
	) &
done
touch "$limit_gate"
wait

limit_outcomes="$temporary_directory/limit-outcomes"
find "$temporary_directory/limit-results" -type f -exec cat {} + >"$limit_outcomes"
limit_confirmed=$(awk -F '\t' '$1 == "201" {count++} END {print count+0}' "$limit_outcomes")
limit_declined=$(awk -F '\t' '$1 == "409" && $2 == "seat_limit_exceeded" {count++} END {print count+0}' "$limit_outcomes")
limit_server_errors=$(awk -F '\t' '$1 ~ /^5/ {count++} END {print count+0}' "$limit_outcomes")
limit_client_errors=$(awk -F '\t' '$1 == "000" {count++} END {print count+0}' "$limit_outcomes")

limit_final_response=$(call_api 200 GET "/shows/$limit_show_id" "" "")
limit_available=$(json_number available <<<"$limit_final_response")
limit_held=$(json_number held <<<"$limit_final_response")
limit_confirmed_seats=$(json_number confirmed <<<"$limit_final_response")
limit_total=$(json_number total_seats <<<"$limit_final_response")
printf 'user_limit requests=10 concurrency=10 confirmed=%d declined_limit=%d server_5xx=%d client_errors=%d\n' \
	"$limit_confirmed" "$limit_declined" "$limit_server_errors" "$limit_client_errors"
printf 'user_limit_final available=%d held=%d confirmed=%d total=%d reconciled=%s\n' \
	"$limit_available" \
	"$limit_held" \
	"$limit_confirmed_seats" \
	"$limit_total" \
	"$([[ $((limit_available + limit_held + limit_confirmed_seats)) -eq $limit_total ]] && printf true || printf false)"

if (( limit_confirmed != 4 ||
	limit_declined != 6 ||
	limit_server_errors != 0 ||
	limit_client_errors != 0 ||
	limit_available != 6 ||
	limit_held != 0 ||
	limit_confirmed_seats != 4 ||
	limit_total != 10 )); then
	printf 'result=fail\n' >&2
	exit 1
fi

metrics_file="$temporary_directory/metrics"
metrics_cache_seconds=5
printf 'metrics waiting_seconds=%d reason=seat_gauge_cache\n' "$((metrics_cache_seconds + 1))"
sleep "$((metrics_cache_seconds + 1))"
call_api 200 GET /metrics "${METRICS_BEARER_TOKEN:-}" "" >"$metrics_file"

if ! metrics_available=$(metric_number seat_reservation_seats_available "$show_id" "$metrics_file") ||
	! metrics_held=$(metric_number seat_reservation_seats_held "$show_id" "$metrics_file") ||
	! metrics_confirmed=$(metric_number seat_reservation_seats_confirmed "$show_id" "$metrics_file") ||
	! metrics_total=$(metric_number seat_reservation_seats_total "$show_id" "$metrics_file") ||
	! limit_metrics_available=$(metric_number seat_reservation_seats_available "$limit_show_id" "$metrics_file") ||
	! limit_metrics_held=$(metric_number seat_reservation_seats_held "$limit_show_id" "$metrics_file") ||
	! limit_metrics_confirmed=$(metric_number seat_reservation_seats_confirmed "$limit_show_id" "$metrics_file") ||
	! limit_metrics_total=$(metric_number seat_reservation_seats_total "$limit_show_id" "$metrics_file"); then
	printf 'metrics response did not contain one numeric sample per seat gauge and show\n' >&2
	exit 1
fi

printf 'metrics show_id=%s available=%d held=%d confirmed=%d total=%d api_reconciled=%s\n' \
	"$show_id" \
	"$metrics_available" \
	"$metrics_held" \
	"$metrics_confirmed" \
	"$metrics_total" \
	"$([[ $metrics_available -eq $available &&
		$metrics_held -eq $held &&
		$metrics_confirmed -eq $confirmed_seats &&
		$metrics_total -eq $total ]] && printf true || printf false)"
printf 'metrics show_id=%s available=%d held=%d confirmed=%d total=%d api_reconciled=%s\n' \
	"$limit_show_id" \
	"$limit_metrics_available" \
	"$limit_metrics_held" \
	"$limit_metrics_confirmed" \
	"$limit_metrics_total" \
	"$([[ $limit_metrics_available -eq $limit_available &&
		$limit_metrics_held -eq $limit_held &&
		$limit_metrics_confirmed -eq $limit_confirmed_seats &&
		$limit_metrics_total -eq $limit_total ]] && printf true || printf false)"

if (( metrics_available != available ||
	metrics_held != held ||
	metrics_confirmed != confirmed_seats ||
	metrics_total != total ||
	limit_metrics_available != limit_available ||
	limit_metrics_held != limit_held ||
	limit_metrics_confirmed != limit_confirmed_seats ||
	limit_metrics_total != limit_total )); then
	printf 'result=fail\n' >&2
	exit 1
fi

printf 'result=pass\n'

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

if (( requests < 12 || concurrency < 1 || users < 27 || timeout_seconds < 1 )); then
	printf 'BURST_REQUESTS must be at least 12, BURST_USERS must be at least 27, and concurrency and timeout must be positive\n' >&2
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

request_status() {
	local method=$1
	local path=$2
	local token=$3
	local payload=$4
	local output=$5
	local arguments=(
		--silent
		--show-error
		--connect-timeout 10
		--max-time "$timeout_seconds"
		--request "$method"
		--header 'Content-Type: application/json'
		--output "$output"
		--write-out '%{http_code}'
	)
	if [[ -n $token ]]; then
		arguments+=(--header "Authorization: Bearer $token")
	fi
	if [[ -n $payload ]]; then
		arguments+=(--data "$payload")
	fi
	curl "${arguments[@]}" "$base_url$path" 2>/dev/null || printf '000'
}

fire() {
	local directory=$1
	local index=$2
	local method=$3
	local path=$4
	local token=$5
	local payload=$6
	local status
	status=$(request_status "$method" "$path" "$token" "$payload" "$directory/$index.body")
	printf '%s\n' "$status" >"$directory/$index.status"
}

count_status() {
	cat "$1"/*.status | awk -v wanted="$2" '$1 == wanted { count++ } END { print count + 0 }'
}

count_error() {
	local directory=$1
	local wanted=$2
	local total=0
	local file
	for file in "$directory"/*.body; do
		if grep -q "\"error\":\"$wanted\"" "$file"; then
			total=$((total + 1))
		fi
	done
	printf '%d\n' "$total"
}

distinct_reservation_ids() {
	local directory=$1
	local file
	for file in "$directory"/*.status; do
		local base=${file%.status}
		if [[ $(cat "$file") == 200 || $(cat "$file") == 201 ]]; then
			sed -nE 's/.*"reservation_id":"([^"]*)".*/\1/p' "$base.body"
		fi
	done | sort -u | wc -l | tr -d ' '
}

counter_value() {
	awk -v sample="$1" '$1 == sample { value = $2; found = 1 } END { if (!found) exit 1; printf "%.0f\n", value }' "$2"
}

seat_state() {
	local response=$1
	local seat=$2
	sed -nE "s/.*\"seat_number\":\"$seat\",\"state\":\"([a-z]*)\".*/\\1/p" <<<"$response"
}

fail_burst() {
	printf 'result=fail: %s\n' "$1" >&2
	exit 1
}

expected_confirmed=0
expected_seat_taken=0
expected_limit=0
expected_replay=0
expected_conflict=0

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
user_ids=()
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
	user_ids+=("$(json_string user_id <<<"$user_response")")
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

phase_show_payload=$(printf \
	'{"name":"burst-%s-phase","seats":["SPOOF","CK1","CK2","CK3","RACE"],"price_paise":25000,"per_user_limit":16}' \
	"$run_id")
phase_show_response=$(call_api 201 POST /shows "$admin_token" "$phase_show_payload")
phase_show_id=$(json_string show_id <<<"$phase_show_response")
if [[ -z $phase_show_id ]]; then
	fail_burst "phase show response did not contain show_id"
fi

metrics_before="$temporary_directory/metrics-before"
if ! request_status GET /metrics "${METRICS_BEARER_TOKEN:-}" "" "$metrics_before" | grep -q '^200$'; then
	fail_burst "GET /metrics did not return 200; set METRICS_BEARER_TOKEN when the target protects /metrics"
fi

taken_payload=$(printf \
	'{"seats":["TAKEN"],"idempotency_key":"%s-taken"}' \
	"$run_id")
call_api 201 POST "/shows/$show_id/reserve" "${tokens[$users]}" "$taken_payload" >/dev/null
expected_confirmed=$((expected_confirmed + 1))

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
expected_confirmed=$((expected_confirmed + 1))
expected_replay=$((expected_replay + 1))
expected_conflict=$((expected_conflict + 1))

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
expected_confirmed=$((expected_confirmed + 2))
expected_replay=$((expected_replay + 1))

owner_user_id=$(json_string user_id <<<"$idempotency_original")
spoof_victim_id=${user_ids[1]}
no_token_status=$(request_status POST "/shows/$phase_show_id/reserve" "" \
	"$(printf '{"seats":["SPOOF"],"idempotency_key":"%s-no-token"}' "$run_id")" /dev/null)
bad_token_status=$(request_status POST "/shows/$phase_show_id/reserve" "not.a.valid.token" \
	"$(printf '{"seats":["SPOOF"],"idempotency_key":"%s-bad-token"}' "$run_id")" /dev/null)
spoof_status=$(request_status POST "/shows/$phase_show_id/reserve" "${tokens[0]}" \
	"$(printf '{"seats":["SPOOF"],"idempotency_key":"%s-spoof","user_id":"%s"}' "$run_id" "$spoof_victim_id")" /dev/null)
phase_after_spoof=$(call_api 200 GET "/shows/$phase_show_id" "" "")
spoof_seat_state=$(seat_state "$phase_after_spoof" SPOOF)
cross_cancel_status=$(request_status POST "/reservations/$idempotency_original_id/cancel" "${tokens[0]}" "" /dev/null)
no_token_cancel_status=$(request_status POST "/reservations/$idempotency_original_id/cancel" "" "" /dev/null)
main_after_cross_cancel=$(call_api 200 GET "/shows/$show_id" "" "")
victim_seat_state=$(seat_state "$main_after_cross_cancel" IDEMP)
printf 'identity no_token=%s bad_token=%s spoofed_body_user_id=%s spoof_seat=%s cross_user_cancel=%s no_token_cancel=%s victim_seat=%s reservation_user_is_token_user=%s\n' \
	"$no_token_status" "$bad_token_status" "$spoof_status" "$spoof_seat_state" \
	"$cross_cancel_status" "$no_token_cancel_status" "$victim_seat_state" \
	"$([[ $owner_user_id == "${user_ids[$users]}" ]] && printf true || printf false)"
if [[ $no_token_status != 401 ||
	$bad_token_status != 401 ||
	$spoof_status != 400 ||
	$spoof_seat_state != available ||
	$cross_cancel_status != 404 ||
	$no_token_cancel_status != 401 ||
	$victim_seat_state != confirmed ||
	$owner_user_id != "${user_ids[$users]}" ]]; then
	fail_burst "identity checks"
fi

mkdir "$temporary_directory/bodies"
curl_config="$temporary_directory/burst.curl"
statuses="$temporary_directory/statuses"
outcomes="$temporary_directory/outcomes"
all_or_nothing_expected=0
printf 'run_id=%s show_id=%s sample_request_id=burst-%s-0\n' "$run_id" "$show_id" "$run_id"
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
		printf 'header = "X-Request-ID: burst-%s-%d"\n' "$run_id" "$index"
		printf 'data = "%s"\n' "$config_payload"
		printf 'output = "%s/bodies/%d"\n' "$temporary_directory" "$index"
		printf 'write-out = "%d %%{http_code} %s %s\\n"\n' "$index" "$seats" "$category"
		printf 'silent\n'
		printf 'show-error\n'
		printf 'connect-timeout = 10\n'
		printf 'max-time = %d\n' "$timeout_seconds"
	} >>"$curl_config"
done

poll_file="$temporary_directory/invariant"
poll_stop="$temporary_directory/invariant-stop"
poll_errors="$temporary_directory/invariant-errors"
: >"$poll_file"
: >"$poll_errors"
(
	while [[ ! -e $poll_stop ]]; do
		poll_body=$(curl --silent --max-time 10 "$base_url/shows/$show_id" 2>/dev/null || true)
		poll_available=$(json_number available <<<"$poll_body")
		poll_held=$(json_number held <<<"$poll_body")
		poll_confirmed=$(json_number confirmed <<<"$poll_body")
		poll_total=$(json_number total_seats <<<"$poll_body")
		if [[ -n $poll_available && -n $poll_held && -n $poll_confirmed && -n $poll_total ]]; then
			printf '%s %s %s %s\n' "$poll_available" "$poll_held" "$poll_confirmed" "$poll_total" >>"$poll_file"
		else
			printf 'x\n' >>"$poll_errors"
		fi
		sleep 0.2
	done
) &
poll_pid=$!

started_at=$SECONDS
curl --config "$curl_config" >"$statuses" || true
duration_seconds=$((SECONDS - started_at))
touch "$poll_stop"
wait "$poll_pid" || true

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

expected_confirmed=$((expected_confirmed + confirmed))
expected_seat_taken=$((expected_seat_taken + seat_declines))
poll_samples=$(wc -l <"$poll_file" | tr -d ' ')
poll_failures=$(wc -l <"$poll_errors" | tr -d ' ')
poll_violations=$(awk '$1 + $2 + $3 != $4 || $4 != 16 { count++ } END { print count + 0 }' "$poll_file")
printf 'invariant_during_burst samples=%d violations=%d unavailable_samples=%d\n' \
	"$poll_samples" "$poll_violations" "$poll_failures"
if (( poll_samples < 3 || poll_violations != 0 )); then
	fail_burst "invariant available+held+confirmed=total during the burst"
fi

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

expected_confirmed=$((expected_confirmed + limit_confirmed))
expected_limit=$((expected_limit + limit_declined))

same_directory="$temporary_directory/same-key"
mkdir "$same_directory"
same_key="$run_id-same-key"
same_payload=$(printf '{"seats":["CK1"],"idempotency_key":"%s"}' "$same_key")
same_count=60
pids=()
for (( index = 1; index <= same_count; index++ )); do
	fire "$same_directory" "$index" POST "/shows/$phase_show_id/reserve" "${tokens[0]}" "$same_payload" &
	pids+=($!)
done
wait "${pids[@]}"
same_created=$(count_status "$same_directory" 201)
same_replays=$(count_status "$same_directory" 200)
same_ids=$(distinct_reservation_ids "$same_directory")
same_other=$((same_count - same_created - same_replays))
same_after=$(call_api 200 GET "/shows/$phase_show_id" "" "")
same_after_replay=$(call_api 200 POST "/shows/$phase_show_id/reserve" "${tokens[0]}" "$same_payload")
same_after_replay_id=$(json_string reservation_id <<<"$same_after_replay")
same_first_id=$(sed -nE 's/.*"reservation_id":"([^"]*)".*/\1/p' "$same_directory"/*.body | sort -u | head -1)
printf 'same_key_concurrent requests=%d created=%d replays=%d other=%d distinct_reservations=%d seat_state=%s later_replay_same_id=%s\n' \
	"$same_count" "$same_created" "$same_replays" "$same_other" "$same_ids" \
	"$(seat_state "$same_after" CK1)" \
	"$([[ $same_after_replay_id == "$same_first_id" ]] && printf true || printf false)"
if (( same_created != 1 || same_replays != same_count - 1 || same_other != 0 || same_ids != 1 )) ||
	[[ $(seat_state "$same_after" CK1) != confirmed || $same_after_replay_id != "$same_first_id" ]]; then
	fail_burst "concurrent retries of one idempotency key"
fi
expected_confirmed=$((expected_confirmed + same_created))
expected_replay=$((expected_replay + same_replays + 1))

diff_directory="$temporary_directory/diff-key"
mkdir "$diff_directory"
diff_key="$run_id-diff-key"
diff_count=40
pids=()
for (( index = 1; index <= diff_count; index++ )); do
	if (( index % 2 == 0 )); then
		diff_seat=CK2
	else
		diff_seat=CK3
	fi
	fire "$diff_directory" "$index" POST "/shows/$phase_show_id/reserve" "${tokens[1]}" \
		"$(printf '{"seats":["%s"],"idempotency_key":"%s"}' "$diff_seat" "$diff_key")" &
	pids+=($!)
done
wait "${pids[@]}"
diff_created=$(count_status "$diff_directory" 201)
diff_replays=$(count_status "$diff_directory" 200)
diff_conflicts=$(count_status "$diff_directory" 409)
diff_conflict_reason=$(count_error "$diff_directory" idempotency_conflict)
diff_ids=$(distinct_reservation_ids "$diff_directory")
diff_other=$((diff_count - diff_created - diff_replays - diff_conflicts))
diff_after=$(call_api 200 GET "/shows/$phase_show_id" "" "")
diff_ck2=$(seat_state "$diff_after" CK2)
diff_ck3=$(seat_state "$diff_after" CK3)
printf 'same_key_different_seats_concurrent requests=%d created=%d replays=%d conflicts=%d other=%d distinct_reservations=%d ck2=%s ck3=%s\n' \
	"$diff_count" "$diff_created" "$diff_replays" "$diff_conflicts" "$diff_other" "$diff_ids" "$diff_ck2" "$diff_ck3"
diff_one_winner=false
if [[ $diff_ck2 == confirmed && $diff_ck3 == available ]] || [[ $diff_ck2 == available && $diff_ck3 == confirmed ]]; then
	diff_one_winner=true
fi
if (( diff_created != 1 || diff_other != 0 || diff_ids != 1 || diff_conflicts != diff_conflict_reason || diff_replays + diff_conflicts != diff_count - 1 )) ||
	[[ $diff_one_winner != true ]]; then
	fail_burst "concurrent same key with different seats"
fi
expected_confirmed=$((expected_confirmed + diff_created))
expected_replay=$((expected_replay + diff_replays))
expected_conflict=$((expected_conflict + diff_conflicts))

race_key="$run_id-race-owner"
race_owner_response=$(call_api 201 POST "/shows/$phase_show_id/reserve" "${tokens[2]}" \
	"$(printf '{"seats":["RACE"],"idempotency_key":"%s"}' "$race_key")")
race_owner_id=$(json_string reservation_id <<<"$race_owner_response")
expected_confirmed=$((expected_confirmed + 1))
race_cancel_directory="$temporary_directory/race-cancel"
race_reserve_directory="$temporary_directory/race-reserve"
mkdir "$race_cancel_directory" "$race_reserve_directory"
pids=()
for (( index = 1; index <= 5; index++ )); do
	fire "$race_cancel_directory" "$index" POST "/reservations/$race_owner_id/cancel" "${tokens[2]}" "" &
	pids+=($!)
done
race_challengers=24
for (( index = 1; index <= race_challengers; index++ )); do
	fire "$race_reserve_directory" "$index" POST "/shows/$phase_show_id/reserve" "${tokens[$((index + 2))]}" \
		"$(printf '{"seats":["RACE"],"idempotency_key":"%s-race-%d"}' "$run_id" "$index")" &
	pids+=($!)
done
wait "${pids[@]}"
race_cancel_ok=$(count_status "$race_cancel_directory" 200)
race_winners=$(count_status "$race_reserve_directory" 201)
race_declined=$(count_status "$race_reserve_directory" 409)
race_other=$((race_challengers - race_winners - race_declined))
race_seat_declined=$(count_error "$race_reserve_directory" seats_unavailable)
race_final_cancel=$(call_api 200 POST "/reservations/$race_owner_id/cancel" "${tokens[2]}" "")
race_cancel_final_status=$(json_string status <<<"$race_final_cancel")
race_state=$(seat_state "$(call_api 200 GET "/shows/$phase_show_id" "" "")" RACE)
race_followup_status=none
if (( race_winners == 1 )); then
	race_followup_status=$(request_status POST "/shows/$phase_show_id/reserve" "${tokens[3]}" \
		"$(printf '{"seats":["RACE"],"idempotency_key":"%s-race-followup"}' "$run_id")" /dev/null)
fi
race_end=$(call_api 200 GET "/shows/$phase_show_id" "" "")
race_end_state=$(seat_state "$race_end" RACE)
phase_available=$(json_number available <<<"$race_end")
phase_held=$(json_number held <<<"$race_end")
phase_confirmed=$(json_number confirmed <<<"$race_end")
phase_total=$(json_number total_seats <<<"$race_end")
printf 'cancel_vs_reserve_race cancels_ok=%d/5 winners=%d declined=%d other=%d seat_after_race=%s repeat_cancel_status=%s followup_status=%s seat_final=%s\n' \
	"$race_cancel_ok" "$race_winners" "$race_declined" "$race_other" "$race_state" \
	"$race_cancel_final_status" "$race_followup_status" "$race_end_state"
race_expected_state=available
race_expected_followup=none
if (( race_winners == 1 )); then
	race_expected_state=confirmed
	race_expected_followup=409
fi
if (( race_cancel_ok != 5 || race_winners > 1 || race_other != 0 || race_declined != race_seat_declined )) ||
	[[ $race_cancel_final_status != cancelled || $race_followup_status != "$race_expected_followup" || $race_end_state != "$race_expected_state" || $race_state != "$race_expected_state" ]]; then
	fail_burst "cancel racing a competing reservation"
fi
expected_confirmed=$((expected_confirmed + race_winners))
expected_seat_taken=$((expected_seat_taken + race_declined))
if [[ $race_followup_status == 409 ]]; then
	expected_seat_taken=$((expected_seat_taken + 1))
fi
printf 'phase_show final available=%s held=%s confirmed=%s total=%s reconciled=%s\n' \
	"$phase_available" "$phase_held" "$phase_confirmed" "$phase_total" \
	"$([[ $((phase_available + phase_held + phase_confirmed)) -eq $phase_total ]] && printf true || printf false)"
if (( phase_held != 0 || phase_confirmed != 2 + race_winners || phase_total != 5 || phase_available + phase_confirmed != phase_total )); then
	fail_burst "phase show reconciliation"
fi

cycle_show_payload=$(printf \
	'{"name":"burst-%s-cycle","seats":["C1","C2","C3","C4","C5","C6","C7","C8"],"price_paise":25000,"per_user_limit":4}' \
	"$run_id")
cycle_show_response=$(call_api 201 POST /shows "$admin_token" "$cycle_show_payload")
cycle_show_id=$(json_string show_id <<<"$cycle_show_response")
cycle_user=${tokens[5]}
cycle_reserve() {
	local expected=$1
	local key=$2
	local seats=$3
	local output=$4
	local token=${5:-$cycle_user}
	local status
	status=$(request_status POST "/shows/$cycle_show_id/reserve" "$token" \
		"$(printf '{"seats":[%s],"idempotency_key":"%s-cycle-%s"}' "$seats" "$run_id" "$key")" "$output")
	if [[ $status != "$expected" ]]; then
		fail_burst "limit cycle step $key expected $expected got $status"
	fi
}
cycle_first="$temporary_directory/cycle-first"
cycle_replay="$temporary_directory/cycle-replay"
cycle_reserve 201 first '"C1","C2"' "$cycle_first"
cycle_first_id=$(json_string reservation_id <<<"$(cat "$cycle_first")")
cycle_reserve 201 second '"C3","C4"' /dev/null
cycle_reserve 409 over-limit-full '"C5"' /dev/null
cycle_cancel_status=$(request_status POST "/reservations/$cycle_first_id/cancel" "$cycle_user" "" /dev/null)
cycle_reserve 201 after-cancel '"C5","C6"' /dev/null
cycle_reserve 409 over-limit-again '"C7"' /dev/null
cycle_replay_status=$(request_status POST "/shows/$cycle_show_id/reserve" "$cycle_user" \
	"$(printf '{"seats":["C1","C2"],"idempotency_key":"%s-cycle-first"}' "$run_id")" "$cycle_replay")
cycle_replay_body=$(cat "$cycle_replay")
cycle_replay_state=$(json_string status <<<"$cycle_replay_body")
cycle_replay_id=$(json_string reservation_id <<<"$cycle_replay_body")
cycle_repeat_cancel=$(request_status POST "/reservations/$cycle_first_id/cancel" "$cycle_user" "" /dev/null)
cycle_reserve 201 other-user-takes-freed '"C1"' /dev/null "${tokens[6]}"
cycle_end=$(call_api 200 GET "/shows/$cycle_show_id" "" "")
cycle_available=$(json_number available <<<"$cycle_end")
cycle_confirmed=$(json_number confirmed <<<"$cycle_end")
cycle_total=$(json_number total_seats <<<"$cycle_end")
printf 'limit_cycle book_2+2 over_limit=409 cancel=%s rebook_2=201 over_limit_again=409 replay_of_cancelled=%s/%s same_reservation=%s repeat_cancel=%s freed_seat_rebooked_by_other=201 final available=%s confirmed=%s total=%s\n' \
	"$cycle_cancel_status" "$cycle_replay_status" "$cycle_replay_state" \
	"$([[ $cycle_replay_id == "$cycle_first_id" ]] && printf true || printf false)" \
	"$cycle_repeat_cancel" "$cycle_available" "$cycle_confirmed" "$cycle_total"
if [[ $cycle_cancel_status != 200 || $cycle_replay_status != 200 || $cycle_replay_state != cancelled ||
	$cycle_replay_id != "$cycle_first_id" || $cycle_repeat_cancel != 200 ]] ||
	[[ $(seat_state "$cycle_end" C2) != available || $(seat_state "$cycle_end" C1) != confirmed ]] ||
	(( cycle_confirmed != 5 || cycle_available != 3 || cycle_total != 8 )); then
	fail_burst "booking, cancelling part of the limit, and booking again"
fi
expected_confirmed=$((expected_confirmed + 4))
expected_limit=$((expected_limit + 2))
expected_replay=$((expected_replay + 1))

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

metric_delta() {
	local sample=$1
	local before
	local after
	before=$(counter_value "$sample" "$metrics_before" 2>/dev/null || printf '0')
	after=$(counter_value "$sample" "$metrics_file")
	printf '%d\n' "$((after - before))"
}
delta_confirmed=$(metric_delta 'seat_reservation_reservations_confirmed_total')
delta_seat_taken=$(metric_delta 'seat_reservation_reservations_declined_total{reason="seat_taken"}')
delta_limit=$(metric_delta 'seat_reservation_reservations_declined_total{reason="per_user_limit"}')
delta_conflict=$(metric_delta 'seat_reservation_reservations_declined_total{reason="idempotency_conflict"}')
delta_replay=$(metric_delta 'seat_reservation_reservations_declined_total{reason="idempotent_replay"}')
delta_replay_total=$(metric_delta 'seat_reservation_idempotent_replays_total')
printf 'metric_counters confirmed=%d/%d seat_taken=%d/%d per_user_limit=%d/%d idempotency_conflict=%d/%d idempotent_replay=%d/%d idempotent_replays_total=%d/%d (metric/observed)\n' \
	"$delta_confirmed" "$expected_confirmed" "$delta_seat_taken" "$expected_seat_taken" \
	"$delta_limit" "$expected_limit" "$delta_conflict" "$expected_conflict" \
	"$delta_replay" "$expected_replay" "$delta_replay_total" "$expected_replay"
if (( delta_confirmed != expected_confirmed ||
	delta_seat_taken != expected_seat_taken ||
	delta_limit != expected_limit ||
	delta_conflict != expected_conflict ||
	delta_replay != expected_replay ||
	delta_replay_total != expected_replay )); then
	fail_burst "metric counters do not match observed outcomes (is another client using the service?)"
fi

printf 'result=pass\n'

#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
	printf 'usage: %s <BASE_URL>\n' "$0" >&2
	exit 2
fi

for command in curl awk sort; do
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
users=${BURST_USERS:-60}
timeout_seconds=${BURST_TIMEOUT_SECONDS:-120}
abort_percent=${BURST_ABORT_PERCENT:-4}
per_user_limit=4

if (( requests < 3000 || concurrency < 1 || users < 60 || timeout_seconds < 1 || abort_percent < 0 || abort_percent > 50 )); then
	printf 'BURST_REQUESTS must be at least 3000, BURST_USERS at least 60, BURST_ABORT_PERCENT between 0 and 50, and concurrency and timeout positive\n' >&2
	exit 2
fi
if (( concurrency > requests )); then
	concurrency=$requests
fi

temporary_directory=$(mktemp -d)
trap 'rm -rf "$temporary_directory"' EXIT HUP INT TERM
receipts="$temporary_directory/receipts"
: >"$receipts"

json_escape() {
	local value=$1
	value=${value//\\/\\\\}
	value=${value//\"/\\\"}
	printf '%s' "$value"
}

json_string() {
	sed -nE "s/.*\"$1\":\"([^\"]*)\".*/\\1/p"
}

json_number() {
	sed -nE "s/.*\"$1\":([0-9]+).*/\\1/p"
}

fail_burst() {
	printf 'result=fail: %s\n' "$1" >&2
	exit 1
}

record_receipt() {
	local method=$1
	local path=$2
	local status=$3
	local key=$4
	local body_file=$5
	local type=
	case $path in
		*/reserve) type=R ;;
		*/cancel) type=C ;;
	esac
	if [[ -n $type && $method == POST ]]; then
		printf '%s\t%s\t%s\t%s\n' "$type" "$status" "${key:--}" "$(tr -d '\n' <"$body_file")" >>"$receipts"
	fi
}

request_status() {
	local method=$1
	local path=$2
	local token=$3
	local payload=$4
	local output=$5
	local key=${6:-}
	local body_file
	local status
	body_file=$(mktemp "$temporary_directory/request.XXXXXX")
	local arguments=(
		--silent
		--connect-timeout 10
		--max-time "$timeout_seconds"
		--request "$method"
		--header 'Content-Type: application/json'
		--output "$body_file"
		--write-out '%{http_code}'
	)
	if [[ -n $token ]]; then
		arguments+=(--header "Authorization: Bearer $token")
	fi
	if [[ -n $payload ]]; then
		arguments+=(--data "$payload")
	fi
	status=$(curl "${arguments[@]}" "$base_url$path" 2>/dev/null) || status=000
	record_receipt "$method" "$path" "$status" "$key" "$body_file"
	if [[ $output != /dev/null ]]; then
		cat "$body_file" >"$output"
	fi
	rm -f "$body_file"
	printf '%s' "$status"
}

call_api() {
	local expected_status=$1
	local method=$2
	local path=$3
	local token=$4
	local payload=$5
	local key=${6:-}
	local body_file
	local status
	body_file=$(mktemp "$temporary_directory/call.XXXXXX")
	status=$(request_status "$method" "$path" "$token" "$payload" "$body_file" "$key")
	if [[ $status != "$expected_status" ]]; then
		printf 'request %s %s returned %s: ' "$method" "$path" "$status" >&2
		cat "$body_file" >&2
		printf '\n' >&2
		return 1
	fi
	cat "$body_file"
	rm -f "$body_file"
}

reserve_payload() {
	local key=$1
	local seats=$2
	printf '{"seats":["%s"],"idempotency_key":"%s"}' "${seats//,/\",\"}" "$key"
}

seat_state() {
	sed -nE "s/.*\"seat_number\":\"$2\",\"state\":\"([a-z]*)\".*/\\1/p" <<<"$1"
}

counter_value() {
	awk -v sample="$1" '$1 == sample { value = $2; found = 1 } END { if (!found) exit 1; printf "%.0f\n", value }' "$2"
}

metric_number() {
	awk -v sample="$1{show_id=\"$2\"}" '
		$1 == sample { count++; value = $2 }
		END { if (count != 1) exit 1; printf "%.0f\n", value }
	' "$3"
}

admin_response=$(call_api 200 POST /auth/login "" \
	"$(printf '{"email":"%s","password":"%s"}' "$(json_escape "$ADMIN_EMAIL")" "$(json_escape "$ADMIN_PASSWORD")")")
admin_token=$(json_string access_token <<<"$admin_response")
if [[ -z $admin_token ]]; then
	fail_burst "login response did not contain access_token"
fi

run_id=$(printf '%s-%s' "$(date +%s)" "$$")
tokens_file="$temporary_directory/tokens"
: >"$tokens_file"
tokens=()
user_ids=()
registration_started=$SECONDS
for (( index = 0; index < users; index++ )); do
	attempts=0
	while :; do
		attempts=$((attempts + 1))
		registration_body="$temporary_directory/registration"
		registration_status=$(request_status POST /auth/register "" \
			"$(printf '{"email":"burst-%s-%d@example.com","password":"password123"}' "$run_id" "$index")" "$registration_body")
		if [[ $registration_status == 201 ]]; then
			break
		fi
		if [[ $registration_status != 429 && $registration_status != 000 ]] || (( attempts > 200 )); then
			fail_burst "registering user $index returned $registration_status"
		fi
		sleep 0.2
	done
	user_token=$(json_string access_token <"$registration_body")
	tokens+=("$user_token")
	user_ids+=("$(json_string user_id <"$registration_body")")
	printf '%s\n' "$user_token" >>"$tokens_file"
done
printf 'registered users=%d seconds=%d\n' "$users" "$((SECONDS - registration_started))"

spam_users=5
spam_seats=8
spam_requests=40
same_users=10
diff_users=10
storm_requests=40
cancel_owners=10
cancel_owner_requests=3
cancel_challengers=12
same_first=$spam_users
diff_first=$((same_first + same_users))
cancel_first=$((diff_first + diff_users))
multi_first=$((cancel_first + cancel_owners))
probe_user=$((multi_first + 5))
taken_user=$((probe_user + 1))
owner_user=$((probe_user + 2))
attacker_user=$((probe_user + 3))
cycle_user=$((probe_user + 4))
cycle_other_user=$((probe_user + 5))
pool_first=$((probe_user + 6))
pool_size=$((users - pool_first))
soup_seats=30

seat_list=(HOT PROBE TAKEN IDEMP IDEMP_ALT CANCEL SPOOF)
for (( index = 1; index <= 10; index++ )); do seat_list+=("M$index"); done
for (( index = 1; index <= 8; index++ )); do seat_list+=("C$index"); done
for (( index = 1; index <= soup_seats; index++ )); do seat_list+=("$(printf 'R%02d' "$index")"); done
for (( user = 0; user < spam_users; user++ )); do
	for (( index = 1; index <= spam_seats; index++ )); do seat_list+=("SP${user}_$index"); done
done
for (( index = 0; index < same_users; index++ )); do seat_list+=("SS$index"); done
for (( index = 0; index < diff_users; index++ )); do seat_list+=("SDA$index" "SDB$index"); done
for (( index = 0; index < cancel_owners; index++ )); do seat_list+=("CX$index"); done
seat_total=${#seat_list[@]}
seats_json=$(printf '"%s",' "${seat_list[@]}")
seats_json=${seats_json%,}

show_response=$(call_api 201 POST /shows "$admin_token" \
	"$(printf '{"name":"burst-%s","seats":[%s],"price_paise":25000,"per_user_limit":%d}' "$run_id" "$seats_json" "$per_user_limit")")
show_id=$(json_string show_id <<<"$show_response")
if [[ -z $show_id ]]; then
	fail_burst "show response did not contain show_id"
fi
printf 'run_id=%s show_id=%s seats=%d per_user_limit=%d users=%d\n' "$run_id" "$show_id" "$seat_total" "$per_user_limit" "$users"

check_integrity() {
	local label=$1
	local body
	body=$(call_api 200 GET "/admin/shows/$show_id/integrity" "$admin_token" "")
	printf 'integrity %s seats_confirmed=%s seats_with_active_owner=%s violations=%s\n' "$label" \
		"$(json_number seats_confirmed <<<"$body")" "$(json_number seats_with_active_owner <<<"$body")" "$(json_number violations <<<"$body")"
	if ! grep -q '"ok":true' <<<"$body"; then
		printf '%s\n' "$body" >&2
		fail_burst "integrity check failed ($label)"
	fi
}

metrics_before="$temporary_directory/metrics-before"
if [[ $(request_status GET /metrics "${METRICS_BEARER_TOKEN:-}" "" "$metrics_before") != 200 ]]; then
	fail_burst "GET /metrics did not return 200; set METRICS_BEARER_TOKEN when the target protects /metrics"
fi

reserve_path="/shows/$show_id/reserve"
call_api 201 POST "$reserve_path" "${tokens[$taken_user]}" "$(reserve_payload "$run_id-taken" TAKEN)" "$run_id-taken" >/dev/null

idempotency_key="$run_id-idempotency"
idempotency_original=$(call_api 201 POST "$reserve_path" "${tokens[$owner_user]}" "$(reserve_payload "$idempotency_key" IDEMP)" "$idempotency_key")
idempotency_replay=$(call_api 200 POST "$reserve_path" "${tokens[$owner_user]}" "$(reserve_payload "$idempotency_key" IDEMP)" "$idempotency_key")
idempotency_conflict=$(call_api 409 POST "$reserve_path" "${tokens[$owner_user]}" "$(reserve_payload "$idempotency_key" IDEMP_ALT)" "$idempotency_key")
idempotency_original_id=$(json_string reservation_id <<<"$idempotency_original")
idempotency_show=$(call_api 200 GET "/shows/$show_id" "" "")
printf 'idempotency replay_same=%s conflict=%s alternate_seat=%s\n' \
	"$([[ $idempotency_replay == "$idempotency_original" ]] && printf true || printf false)" \
	"$(json_string error <<<"$idempotency_conflict")" "$(seat_state "$idempotency_show" IDEMP_ALT)"
if [[ -z $idempotency_original_id || $idempotency_replay != "$idempotency_original" ||
	$(json_string error <<<"$idempotency_conflict") != idempotency_conflict ||
	$(seat_state "$idempotency_show" IDEMP_ALT) != available ]]; then
	fail_burst "sequential idempotency"
fi

cancellation_key="$run_id-cancellation"
cancellation_original=$(call_api 201 POST "$reserve_path" "${tokens[$owner_user]}" "$(reserve_payload "$cancellation_key" CANCEL)" "$cancellation_key")
cancellation_id=$(json_string reservation_id <<<"$cancellation_original")
cancellation_first=$(call_api 200 POST "/reservations/$cancellation_id/cancel" "${tokens[$owner_user]}" "")
cancellation_repeat=$(call_api 200 POST "/reservations/$cancellation_id/cancel" "${tokens[$owner_user]}" "")
cancellation_replay=$(call_api 200 POST "$reserve_path" "${tokens[$owner_user]}" "$(reserve_payload "$cancellation_key" CANCEL)" "$cancellation_key")
cancellation_rebook=$(call_api 201 POST "$reserve_path" "${tokens[$owner_user]}" "$(reserve_payload "$cancellation_key-new" CANCEL)" "$cancellation_key-new")
printf 'cancellation first=%s repeat=%s replay_status=%s replay_same_id=%s rebooked_new_id=%s\n' \
	"$(json_string status <<<"$cancellation_first")" "$(json_string status <<<"$cancellation_repeat")" \
	"$(json_string status <<<"$cancellation_replay")" \
	"$([[ $(json_string reservation_id <<<"$cancellation_replay") == "$cancellation_id" ]] && printf true || printf false)" \
	"$([[ $(json_string reservation_id <<<"$cancellation_rebook") != "$cancellation_id" ]] && printf true || printf false)"
if [[ $(json_string status <<<"$cancellation_first") != cancelled ||
	$(json_string status <<<"$cancellation_repeat") != cancelled ||
	$(json_string status <<<"$cancellation_replay") != cancelled ||
	$(json_string reservation_id <<<"$cancellation_replay") != "$cancellation_id" ||
	-z $(json_string reservation_id <<<"$cancellation_rebook") ||
	$(json_string reservation_id <<<"$cancellation_rebook") == "$cancellation_id" ]]; then
	fail_burst "cancellation and replay of a cancelled reservation"
fi

no_token_status=$(request_status POST "$reserve_path" "" "$(reserve_payload "$run_id-no-token" SPOOF)" /dev/null)
bad_token_status=$(request_status POST "$reserve_path" not.a.valid.token "$(reserve_payload "$run_id-bad-token" SPOOF)" /dev/null)
spoof_status=$(request_status POST "$reserve_path" "${tokens[$attacker_user]}" \
	"$(printf '{"seats":["SPOOF"],"idempotency_key":"%s-spoof","user_id":"%s"}' "$run_id" "${user_ids[$owner_user]}")" /dev/null)
cross_cancel_status=$(request_status POST "/reservations/$idempotency_original_id/cancel" "${tokens[$attacker_user]}" "" /dev/null)
anonymous_cancel_status=$(request_status POST "/reservations/$idempotency_original_id/cancel" "" "" /dev/null)
identity_show=$(call_api 200 GET "/shows/$show_id" "" "")
printf 'identity no_token=%s bad_token=%s spoofed_user_id=%s spoof_seat=%s cross_user_cancel=%s anonymous_cancel=%s victim_seat=%s owner_matches_token=%s\n' \
	"$no_token_status" "$bad_token_status" "$spoof_status" "$(seat_state "$identity_show" SPOOF)" \
	"$cross_cancel_status" "$anonymous_cancel_status" "$(seat_state "$identity_show" IDEMP)" \
	"$([[ $(json_string user_id <<<"$idempotency_original") == "${user_ids[$owner_user]}" ]] && printf true || printf false)"
if [[ $no_token_status != 401 || $bad_token_status != 401 || $spoof_status != 400 ||
	$(seat_state "$identity_show" SPOOF) != available || $cross_cancel_status != 404 ||
	$anonymous_cancel_status != 401 || $(seat_state "$identity_show" IDEMP) != confirmed ||
	$(json_string user_id <<<"$idempotency_original") != "${user_ids[$owner_user]}" ]]; then
	fail_burst "identity checks"
fi

cancel_ids=()
cancel_keys=()
for (( index = 0; index < cancel_owners; index++ )); do
	owner=$((cancel_first + index))
	key="$run_id-cancel-owner-$index"
	response=$(call_api 201 POST "$reserve_path" "${tokens[$owner]}" "$(reserve_payload "$key" "CX$index")" "$key")
	cancel_ids+=("$(json_string reservation_id <<<"$response")")
	cancel_keys+=("$key")
done

huge_body="$temporary_directory/huge-body"
awk 'BEGIN { printf "{\"seats\":[\"R01\"],\"idempotency_key\":\""; for (i = 0; i < 2097152; i++) printf "x"; printf "\"}" }' >"$huge_body"

plan_unsorted="$temporary_directory/plan-unsorted"
sequence=0
add() {
	sequence=$((sequence + 1))
	printf '%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
		"$((RANDOM * 32768 + RANDOM))" "$1" "$2" "$3" "$4" "$5" "$6" "$7" "$8" "$9" "${10}" "${11}"
}

{
	for (( user = 0; user < spam_users; user++ )); do
		for (( index = 0; index < spam_requests; index++ )); do
			seat="SP${user}_$((index % spam_seats + 1))"
			key="$run_id-spam-$user-$index"
			add spam "$user" u POST "$reserve_path" "$(reserve_payload "$key" "$seat")" application/json 0 "$key" "$seat" "$user"
		done
	done
	for (( index = 0; index < same_users; index++ )); do
		user=$((same_first + index))
		key="$run_id-same-$index"
		for (( repeat = 0; repeat < storm_requests; repeat++ )); do
			add storm_same "$user" u POST "$reserve_path" "$(reserve_payload "$key" "SS$index")" application/json 0 "$key" "SS$index" "$index"
		done
	done
	for (( index = 0; index < diff_users; index++ )); do
		user=$((diff_first + index))
		key="$run_id-diff-$index"
		for (( repeat = 0; repeat < storm_requests; repeat++ )); do
			if (( repeat % 2 == 0 )); then seat="SDA$index"; else seat="SDB$index"; fi
			add storm_diff "$user" u POST "$reserve_path" "$(reserve_payload "$key" "$seat")" application/json 0 "$key" "$seat" "$index"
		done
	done
	for (( index = 0; index < cancel_owners; index++ )); do
		owner=$((cancel_first + index))
		cancel_path="/reservations/${cancel_ids[$index]}/cancel"
		for (( repeat = 0; repeat < cancel_owner_requests; repeat++ )); do
			add cancel_owner "$owner" u POST "$cancel_path" - - 0 - "CX$index" "$index"
		done
		add cancel_cross "$((pool_first + RANDOM % pool_size))" u POST "$cancel_path" - - 0 - "CX$index" "$index"
		add cancel_anonymous "$owner" none POST "$cancel_path" - - 0 - "CX$index" "$index"
		for (( repeat = 0; repeat < cancel_challengers; repeat++ )); do
			key="$run_id-regrab-$index-$repeat"
			add regrab "$((pool_first + RANDOM % pool_size))" u POST "$reserve_path" "$(reserve_payload "$key" "CX$index")" application/json 0 "$key" "CX$index" "$index"
		done
	done
	many_seats=$(for (( index = 0; index < 300; index++ )); do printf '"Q%03d",' "$index"; done)
	many_seats=${many_seats%,}
	for (( repeat = 0; repeat < 8; repeat++ )); do
		user=$((pool_first + RANDOM % pool_size))
		key="$run_id-hostile-$repeat"
		add hostile_no_token "$user" none POST "$reserve_path" "$(reserve_payload "$key-a" R01)" application/json 0 - - 401
		add hostile_bad_token "$user" bad POST "$reserve_path" "$(reserve_payload "$key-b" R01)" application/json 0 - - 401
		add hostile_spoof_user "$user" u POST "$reserve_path" "{\"seats\":[\"R02\"],\"idempotency_key\":\"$key-c\",\"user_id\":\"${user_ids[0]}\"}" application/json 0 - - 400
		add hostile_bad_json "$user" u POST "$reserve_path" '{"seats":[' application/json 0 - - 400
		add hostile_empty_seats "$user" u POST "$reserve_path" "{\"seats\":[],\"idempotency_key\":\"$key-d\"}" application/json 0 - - 400
		add hostile_duplicate_seats "$user" u POST "$reserve_path" "$(reserve_payload "$key-e" R03,R03)" application/json 0 - - 400
		add hostile_no_key "$user" u POST "$reserve_path" '{"seats":["R04"]}' application/json 0 - - 400
		add hostile_long_key "$user" u POST "$reserve_path" "$(reserve_payload "$(printf 'k%.0s' {1..300})" R05)" application/json 0 - - 400
		add hostile_control_seat "$user" u POST "$reserve_path" "{\"seats\":[\"S\\u0001\"],\"idempotency_key\":\"$key-f\"}" application/json 0 - - 400
		add hostile_wrong_content_type "$user" u POST "$reserve_path" "$(reserve_payload "$key-g" R06)" text/plain 0 - - 415
		add hostile_bad_show_id "$user" u POST /shows/not-a-uuid/reserve "$(reserve_payload "$key-h" R07)" application/json 0 - - 400
		add hostile_unknown_show "$user" u POST /shows/00000000-0000-0000-0000-000000000000/reserve "$(reserve_payload "$key-i" R08)" application/json 0 - - 404
		add hostile_unknown_seat "$user" u POST "$reserve_path" "$(reserve_payload "$key-j" NOPE-1)" application/json 0 "$key-j" NOPE-1 409
		add hostile_300_seats "$user" u POST "$reserve_path" "{\"seats\":[$many_seats],\"idempotency_key\":\"$key-k\"}" application/json 0 "$key-k" - 409
		add hostile_cancel_bad_id "$user" u POST /reservations/not-a-uuid/cancel - - 0 - - 400,404
		if (( repeat < 2 )); then
			add hostile_huge_body "$user" u POST "$reserve_path" "@$huge_body" application/json 0 - - 400,413
		fi
	done
	remaining=$((requests - sequence))
	hot_count=$((remaining * 40 / 100))
	multi_count=$((remaining * 15 / 100))
	probe_count=$((remaining * 5 / 100))
	soup_count=$((remaining - hot_count - multi_count - probe_count))
	for (( index = 0; index < hot_count; index++ )); do
		key="$run_id-hot-$index"
		add hot "$((pool_first + RANDOM % pool_size))" u POST "$reserve_path" "$(reserve_payload "$key" HOT)" application/json 0 "$key" HOT -
	done
	multi_patterns=(M1,M2 M2,M3 M4,M5,M6 M6,M7 M8,M9,M10)
	for (( index = 0; index < multi_count; index++ )); do
		pattern=$((index % 5))
		key="$run_id-multi-$index"
		add multi "$((multi_first + pattern))" u POST "$reserve_path" "$(reserve_payload "$key" "${multi_patterns[$pattern]}")" application/json 0 "$key" "${multi_patterns[$pattern]}" "$pattern"
	done
	for (( index = 0; index < probe_count; index++ )); do
		key="$run_id-probe-$index"
		add probe "$probe_user" u POST "$reserve_path" "$(reserve_payload "$key" PROBE,TAKEN)" application/json 0 "$key" PROBE,TAKEN -
	done
	for (( index = 0; index < soup_count; index++ )); do
		count=$((1 + RANDOM % 3))
		seats=
		for (( position = 0; position < count; position++ )); do
			printf -v seat 'R%02d' "$((1 + RANDOM % soup_seats))"
			if [[ ,$seats, != *",$seat,"* ]]; then
				seats=${seats:+$seats,}$seat
			fi
		done
		key="$run_id-soup-$index"
		abort=0
		if (( RANDOM % 100 < abort_percent )); then
			abort=1
		fi
		add soup "$((pool_first + RANDOM % pool_size))" u POST "$reserve_path" "$(reserve_payload "$key" "$seats")" application/json "$abort" "$key" "$seats" -
	done
} >"$plan_unsorted"

plan="$temporary_directory/plan"
sort -n -k1,1 "$plan_unsorted" | cut -f2- | awk -F '\t' 'BEGIN { OFS = "\t" } { print NR - 1, $0 }' >"$plan"
planned=$(wc -l <"$plan" | tr -d ' ')
mkdir "$temporary_directory/bodies"
curl_config="$temporary_directory/burst.curl"
awk -F '\t' \
	-v base="$base_url" -v run="$run_id" -v dir="$temporary_directory/bodies" \
	-v timeout="$timeout_seconds" -v concurrency="$concurrency" -v tokens="$tokens_file" '
	BEGIN {
		while ((getline line < tokens) > 0) token[count++] = line
		print "parallel"
		print "parallel-max = " concurrency
	}
	{
		index_value = $1; user = $3; mode = $4; method = $5; path = $6; payload = $7; ctype = $8; abort = $9
		if (NR > 1) print "next"
		printf "url = \"%s%s\"\n", base, path
		printf "request = \"%s\"\n", method
		if (mode == "u") printf "header = \"Authorization: Bearer %s\"\n", token[user]
		if (mode == "bad") print "header = \"Authorization: Bearer not.a.valid.token\""
		if (ctype != "-") printf "header = \"Content-Type: %s\"\n", ctype
		printf "header = \"X-Request-ID: burst-%s-%d\"\n", run, index_value
		if (payload != "-") {
			gsub(/\\/, "\\\\", payload)
			gsub(/"/, "\\\"", payload)
			printf "data-binary = \"%s\"\n", payload
		}
		printf "output = \"%s/%d\"\n", dir, index_value
		printf "write-out = \"%d %%{http_code}\\n\"\n", index_value
		print "silent"
		print "connect-timeout = 10"
		if (abort == 1) {
			printf "max-time = %.3f\n", 0.02 + (index_value % 50) / 100
		} else {
			printf "max-time = %d\n", timeout
		}
	}' "$plan" >"$curl_config"

poll_stop="$temporary_directory/poll-stop"
poll_counts="$temporary_directory/poll-counts"
poll_integrity="$temporary_directory/poll-integrity"
: >"$poll_counts"
: >"$poll_integrity"
(
	while [[ ! -e $poll_stop ]]; do
		poll_body=$(curl --silent --max-time 10 "$base_url/shows/$show_id" 2>/dev/null || true)
		printf '%s %s %s %s\n' "$(json_number available <<<"$poll_body")" "$(json_number held <<<"$poll_body")" \
			"$(json_number confirmed <<<"$poll_body")" "$(json_number total_seats <<<"$poll_body")" >>"$poll_counts"
		poll_body=$(curl --silent --max-time 10 --header "Authorization: Bearer $admin_token" \
			"$base_url/admin/shows/$show_id/integrity" 2>/dev/null || true)
		if grep -q '"ok":true' <<<"$poll_body"; then
			printf 'ok\n' >>"$poll_integrity"
		elif grep -q '"ok":false' <<<"$poll_body"; then
			printf 'violation\n' >>"$poll_integrity"
		else
			printf 'unavailable\n' >>"$poll_integrity"
		fi
		sleep 0.2
	done
) &
poll_pid=$!

statuses="$temporary_directory/statuses"
printf 'burst requests=%d concurrency=%d sample_request_id=burst-%s-0\n' "$planned" "$concurrency" "$run_id"
started_at=$SECONDS
curl --config "$curl_config" >"$statuses" 2>/dev/null || true
duration_seconds=$((SECONDS - started_at))
touch "$poll_stop"
wait "$poll_pid" || true

results="$temporary_directory/results"
awk -F '\t' -v dir="$temporary_directory/bodies" -v statuses="$statuses" '
	BEGIN {
		OFS = "\t"
		while ((getline line < statuses) > 0) {
			split(line, parts, " ")
			code[parts[1]] = parts[2]
		}
	}
	function field(body, name,    value) {
		if (match(body, "\"" name "\":\"[^\"]*\"")) {
			value = substr(body, RSTART, RLENGTH)
			sub("^\"" name "\":\"", "", value)
			sub("\"$", "", value)
			return value
		}
		return "-"
	}
	{
		status = ($1 in code) ? code[$1] : "000"
		body = ""
		file = dir "/" $1
		while ((getline line < file) > 0) body = body line
		close(file)
		seats = "-"
		if (match(body, "\"seats\":\\[[^]]*\\]")) {
			seats = substr(body, RSTART + 9, RLENGTH - 10)
			gsub(/"/, "", seats)
		}
		rid = field(body, "reservation_id"); rstatus = field(body, "status"); error = field(body, "error"); ruser = field(body, "user_id")
		print $1, $2, $3, $9, $10, $11, $12, status, rid, rstatus, error, ruser, seats, body
	}' "$plan" >"$results"

awk -F '\t' '{
	type = ($2 ~ /^cancel/ || $2 == "hostile_cancel_bad_id") ? "C" : "R"
	print type "\t" $8 "\t" $5 "\t" $14
}' "$results" >>"$receipts"

aborted_count=$(awk -F '\t' '$4 == 1' "$results" | wc -l | tr -d ' ')
retry_results="$temporary_directory/retry-results"
: >"$retry_results"
if (( aborted_count > 0 )); then
	mkdir "$temporary_directory/retry"
	retry_config="$temporary_directory/retry.curl"
	awk -F '\t' -v plan="$plan" -v base="$base_url" -v dir="$temporary_directory/retry" -v tokens="$tokens_file" -v timeout="$timeout_seconds" '
		BEGIN {
			while ((getline line < tokens) > 0) token[count++] = line
			while ((getline line < plan) > 0) { split(line, p, "\t"); payload[p[1]] = p[7]; path[p[1]] = p[6] }
			print "parallel"
			print "parallel-max = 100"
		}
		$4 == 1 {
			if (emitted++) print "next"
			body = payload[$1]
			gsub(/\\/, "\\\\", body)
			gsub(/"/, "\\\"", body)
			printf "url = \"%s%s\"\n", base, path[$1]
			print "request = \"POST\""
			printf "header = \"Authorization: Bearer %s\"\n", token[$3]
			print "header = \"Content-Type: application/json\""
			printf "data-binary = \"%s\"\n", body
			printf "output = \"%s/%d\"\n", dir, $1
			printf "write-out = \"%d %%{http_code}\\n\"\n", $1
			print "silent"
			printf "max-time = %d\n", timeout
		}' "$results" >"$retry_config"
	curl --config "$retry_config" >"$temporary_directory/retry-statuses" 2>/dev/null || true
	awk -v dir="$temporary_directory/retry" -v results="$results" '
		BEGIN { while ((getline line < results) > 0) { split(line, r, "\t"); key[r[1]] = r[5] } }
		{
			body = ""
			file = dir "/" $1
			while ((getline line < file) > 0) body = body line
			close(file)
			print $1 "\t" $2 "\t" key[$1] "\t" body
		}' "$temporary_directory/retry-statuses" >"$retry_results"
	awk -F '\t' '{ print "R\t" $2 "\t" $3 "\t" $4 }' "$retry_results" >>"$receipts"
fi

printf 'burst duration_seconds=%d aborted_by_client=%d retried=%d\n' "$duration_seconds" "$aborted_count" "$(wc -l <"$retry_results" | tr -d ' ')"
awk -F '\t' '{ counts[$2 " " $8]++ } END { for (k in counts) print k, counts[k] }' "$results" | sort |
	awk '{ if ($1 != kind) { if (line != "") print line; kind = $1; line = sprintf("    %-28s", $1) } line = line " " $2 ":" $3 } END { print line }'
awk -F '\t' '$8 == "409" { counts[$11]++ } END { for (k in counts) printf "declined reason=%s count=%d\n", k, counts[k] }' "$results" | sort

problems="$temporary_directory/problems"
: >"$problems"
problem() {
	printf '%s\n' "$1" >>"$problems"
}

awk -F '\t' -v retries="$retry_results" -v spam_users="$spam_users" -v limit="$per_user_limit" '
	BEGIN { while ((getline line < retries) > 0) { split(line, r, "\t"); retried[r[1]] = r[2] } }
	$8 ~ /^5/ { print "5xx: " $2 " request " $1 " returned " $8 }
	$8 == "000" && $4 != 1 { print "client error: " $2 " request " $1 " got no response" }
	$4 == 1 && !($1 in retried) { print "aborted request " $1 " was not retried" }
	$4 == 1 && ($1 in retried) && retried[$1] !~ /^(200|201|409)$/ { print "retry of aborted request " $1 " returned " retried[$1] }
	$2 ~ /^hostile/ {
		n = split($7, allowed, ",")
		ok = 0
		for (i = 1; i <= n; i++) if ($8 == allowed[i]) ok = 1
		if (!ok) print $2 " request " $1 " returned " $8 " expected " $7
	}
	$2 == "spam" { if ($8 == "201") spam_ok[$3]++; else if ($8 != "409") print "spam request " $1 " returned " $8 }
	$2 == "storm_same" {
		if ($8 == "201") same_created[$5]++
		else if ($8 == "200") same_replay[$5]++
		else print "storm_same request " $1 " returned " $8
		if ($9 != "-") { if (!(($5 SUBSEP $9) in same_id)) same_ids[$5]++; same_id[$5, $9] = 1 }
		same_keys[$5] = 1
	}
	$2 == "storm_diff" {
		if ($8 == "201") diff_created[$5]++
		else if ($8 != "200" && !($8 == "409" && $11 == "idempotency_conflict")) print "storm_diff request " $1 " returned " $8 " " $11
		if ($9 != "-") { if (!(($5 SUBSEP $9) in diff_id)) diff_ids[$5]++; diff_id[$5, $9] = 1 }
		diff_keys[$5] = 1
	}
	$2 == "cancel_owner" && !($8 == "200" && $10 == "cancelled") { print "owner cancel request " $1 " returned " $8 " " $10 }
	$2 == "cancel_cross" && $8 != "404" { print "cross-user cancel request " $1 " returned " $8 }
	$2 == "cancel_anonymous" && $8 != "401" { print "anonymous cancel request " $1 " returned " $8 }
	$2 == "regrab" { if ($8 == "201") regrab_won[$6]++; else if ($8 != "409") print "regrab request " $1 " returned " $8 }
	$2 == "hot" { if ($8 == "201") hot_won++; else if ($8 != "409") print "hot request " $1 " returned " $8 }
	$2 == "multi" { if ($8 == "201") multi_won++; else if ($8 != "409") print "multi request " $1 " returned " $8 }
	$2 == "probe" && !($8 == "409" && $11 == "seats_unavailable") { print "all-or-nothing request " $1 " returned " $8 " " $11 }
	$2 == "soup" && $4 != 1 && $8 !~ /^(201|409)$/ { print "soup request " $1 " returned " $8 }
	END {
		for (user = 0; user < spam_users; user++) if (spam_ok[user] != limit) print "spam user " user " got " spam_ok[user] + 0 " confirmations, expected exactly " limit
		for (key in same_keys) if (same_created[key] != 1 || same_ids[key] != 1) print "same-key storm " key " created " same_created[key] + 0 " with " same_ids[key] + 0 " distinct reservations"
		for (key in diff_keys) if (diff_created[key] != 1 || diff_ids[key] != 1) print "different-body storm " key " created " diff_created[key] + 0 " with " diff_ids[key] + 0 " distinct reservations"
		for (seat in regrab_won) if (regrab_won[seat] > 1) print "seat " seat " was re-grabbed " regrab_won[seat] " times"
		if (hot_won != 1) print "hot seat had " hot_won + 0 " winners, expected 1"
		if (multi_won != 3) print "overlapping multi-seat groups had " multi_won + 0 " winners, expected 3"
	}' "$results" >>"$problems"

regrab_winners=$(awk -F '\t' '$2 == "regrab" && $8 == "201" { print $6 }' "$results" | sort)

sweep_results="$temporary_directory/sweep"
: >"$sweep_results"
awk -F '\t' '$8 == "201" && $5 != "-" && $2 != "storm_diff" && picked < 300 { print $3 "\t" $5 "\t" $6 "\t" $9; picked++ }' "$results" >"$temporary_directory/sweep-keys"
for (( index = 0; index < cancel_owners; index++ )); do
	printf '%d\t%s\tCX%d\t%s\n' "$((cancel_first + index))" "${cancel_keys[$index]}" "$index" "${cancel_ids[$index]}" >>"$temporary_directory/sweep-keys"
done
while IFS=$'\t' read -r user key seats expected_id; do
	sweep_body="$temporary_directory/sweep-body"
	sweep_status=$(request_status POST "$reserve_path" "${tokens[$user]}" "$(reserve_payload "$key" "$seats")" "$sweep_body" "$key")
	printf '%s\t%s\t%s\t%s\n' "$sweep_status" "$expected_id" "$(json_string reservation_id <"$sweep_body")" "$(json_string status <"$sweep_body")" >>"$sweep_results"
done <"$temporary_directory/sweep-keys"
awk -F '\t' '$1 != "200" || $2 != $3 { print "replay sweep: status " $1 " reservation " $3 " expected " $2 }' "$sweep_results" >>"$problems"
sweep_cancelled=$(awk -F '\t' '$4 == "cancelled"' "$sweep_results" | wc -l | tr -d ' ')
printf 'replay_sweep keys=%d cancelled_replays=%d\n' "$(wc -l <"$sweep_results" | tr -d ' ')" "$sweep_cancelled"
if (( sweep_cancelled != cancel_owners )); then
	problem "replay sweep returned $sweep_cancelled cancelled reservations, expected $cancel_owners"
fi

cycle_reserve() {
	local expected=$1
	local key="$run_id-cycle-$2"
	local seats=$3
	local output=$4
	local token=${5:-${tokens[$cycle_user]}}
	local status
	status=$(request_status POST "$reserve_path" "$token" "$(reserve_payload "$key" "$seats")" "$output" "$key")
	if [[ $status != "$expected" ]]; then
		problem "limit cycle step $2 expected $expected got $status"
	fi
}
cycle_first="$temporary_directory/cycle-first"
cycle_reserve 201 first C1,C2 "$cycle_first"
cycle_first_id=$(json_string reservation_id <"$cycle_first")
cycle_reserve 201 second C3,C4 /dev/null
cycle_reserve 409 over-limit C5 /dev/null
cycle_cancel_status=$(request_status POST "/reservations/$cycle_first_id/cancel" "${tokens[$cycle_user]}" "" /dev/null)
cycle_reserve 201 after-cancel C5,C6 /dev/null
cycle_reserve 409 over-limit-again C7 /dev/null
cycle_reserve 201 other-user-takes-freed C1 /dev/null "${tokens[$cycle_other_user]}"
cycle_show=$(call_api 200 GET "/shows/$show_id" "" "")
cycle_states=$(for seat in C1 C2 C3 C4 C5 C6 C7 C8; do printf '%s=%s ' "$seat" "$(seat_state "$cycle_show" "$seat")"; done)
printf 'limit_cycle cancel=%s %s\n' "$cycle_cancel_status" "$cycle_states"
if [[ $cycle_cancel_status != 200 || $cycle_states != "C1=confirmed C2=available C3=confirmed C4=confirmed C5=confirmed C6=confirmed C7=available C8=available " ]]; then
	problem "booking, cancelling part of the limit, and booking again"
fi

final_show=$(call_api 200 GET "/shows/$show_id" "" "")
final_available=$(json_number available <<<"$final_show")
final_held=$(json_number held <<<"$final_show")
final_confirmed=$(json_number confirmed <<<"$final_show")
final_total=$(json_number total_seats <<<"$final_show")
api_confirmed_seats="$temporary_directory/api-confirmed"
grep -oE '"seat_number":"[^"]*","state":"confirmed"' <<<"$final_show" | sed -E 's/"seat_number":"([^"]*)".*/\1/' | sort >"$api_confirmed_seats"

model_report="$temporary_directory/model"
model_seats="$temporary_directory/model-seats"
awk -F '\t' -v limit="$per_user_limit" -v seats_out="$model_seats" '
	function field(body, name,    value) {
		if (match(body, "\"" name "\":\"[^\"]*\"")) {
			value = substr(body, RSTART, RLENGTH)
			sub("^\"" name "\":\"", "", value)
			sub("\"$", "", value)
			return value
		}
		return ""
	}
	{
		type = $1; status = $2; key = $3; body = $4
		rid = field(body, "reservation_id"); error = field(body, "error")
		if (type == "R") {
			if (status == "201" || status == "200") {
				user = field(body, "user_id")
				list = ""
				if (match(body, "\"seats\":\\[[^]]*\\]")) { list = substr(body, RSTART + 9, RLENGTH - 10); gsub(/"/, "", list) }
				if (rid in owner && (owner[rid] != user || seats[rid] != list)) print "PROBLEM reservation " rid " changed owner or seats between receipts"
				owner[rid] = user; seats[rid] = list
				if (field(body, "status") == "cancelled") cancelled[rid] = 1
				if (key != "-") {
					if ((user SUBSEP key) in key_id && key_id[user, key] != rid) print "PROBLEM idempotency key " key " returned two reservations"
					key_id[user, key] = rid
				}
				if (status == "201") { if (rid in created) print "PROBLEM reservation " rid " created twice"; created[rid] = 1 }
				if (status == "200") replays++
			}
			if (status == "409" && error == "seats_unavailable") seat_taken++
			if (status == "409" && error == "seat_limit_exceeded") limit_declines++
			if (status == "409" && error == "idempotency_conflict") conflicts++
		}
		if (type == "C" && status == "200") cancelled[rid] = 1
	}
	END {
		for (rid in owner) {
			distinct++
			if (rid in cancelled) continue
			active++
			n = split(seats[rid], parts, ",")
			held[owner[rid]] += n
			for (i = 1; i <= n; i++) {
				if (parts[i] in seat_owner) print "PROBLEM seat " parts[i] " is held by reservations " seat_owner[parts[i]] " and " rid
				seat_owner[parts[i]] = rid
				print parts[i] > seats_out
			}
		}
		for (user in held) if (held[user] > limit) print "PROBLEM user " user " holds " held[user] " seats over the limit of " limit
		for (rid in cancelled) cancelled_total++
		printf "COUNTS %d %d %d %d %d %d %d\n", distinct, active, cancelled_total, replays, seat_taken, limit_declines, conflicts
	}' "$receipts" >"$model_report"
touch "$model_seats"
sed -n 's/^PROBLEM //p' "$model_report" >>"$problems"
read -r _ model_created model_active model_cancelled model_replays model_seat_taken model_limit model_conflicts < <(grep '^COUNTS' "$model_report")
sort -o "$model_seats" "$model_seats"
if ! cmp -s "$model_seats" "$api_confirmed_seats"; then
	problem "confirmed seats from the API differ from the seats held by active reservations in the receipts: $(diff "$model_seats" "$api_confirmed_seats" | grep '^[<>]' | head -10 | tr '\n' ' ')"
fi
printf 'model reservations_created=%d active=%d cancelled=%d seats_held=%d api_confirmed_seats=%d\n' \
	"$model_created" "$model_active" "$model_cancelled" "$(wc -l <"$model_seats" | tr -d ' ')" "$(wc -l <"$api_confirmed_seats" | tr -d ' ')"

for (( index = 0; index < cancel_owners; index++ )); do
	won=$(grep -cx "CX$index" <<<"$regrab_winners" || true)
	state=$(seat_state "$final_show" "CX$index")
	if [[ ($won == 1 && $state != confirmed) || ($won == 0 && $state != available) ]]; then
		problem "cancelled seat CX$index is $state with $won re-grab winners"
	fi
done
if [[ $(seat_state "$final_show" HOT) != confirmed || $(seat_state "$final_show" PROBE) != available || $(seat_state "$final_show" TAKEN) != confirmed ]]; then
	problem "HOT must be confirmed, PROBE available and TAKEN confirmed"
fi
for (( index = 0; index < diff_users; index++ )); do
	pair="$(seat_state "$final_show" "SDA$index")/$(seat_state "$final_show" "SDB$index")"
	if [[ $pair != confirmed/available && $pair != available/confirmed ]]; then
		problem "different-body storm $index left seats $pair"
	fi
done

printf 'final available=%s held=%s confirmed=%s total=%s reconciled=%s\n' "$final_available" "$final_held" "$final_confirmed" "$final_total" \
	"$([[ $((final_available + final_held + final_confirmed)) -eq $final_total ]] && printf true || printf false)"
if (( final_held != 0 || final_total != seat_total || final_available + final_confirmed != final_total )); then
	problem "seat counts do not reconcile"
fi

poll_samples=$(awk 'NF == 4' "$poll_counts" | wc -l | tr -d ' ')
poll_violations=$(awk -v total="$seat_total" 'NF == 4 && ($1 + $2 + $3 != $4 || $4 != total)' "$poll_counts" | wc -l | tr -d ' ')
integrity_ok=$(grep -c '^ok$' "$poll_integrity" || true)
integrity_bad=$(grep -c '^violation$' "$poll_integrity" || true)
integrity_unavailable=$(grep -c '^unavailable$' "$poll_integrity" || true)
printf 'during_burst invariant_samples=%d invariant_violations=%d integrity_checks=%d integrity_violations=%d integrity_unavailable=%d\n' \
	"$poll_samples" "$poll_violations" "$integrity_ok" "$integrity_bad" "$integrity_unavailable"
if (( poll_samples < 1 || poll_violations != 0 || integrity_ok < 1 || integrity_bad != 0 )); then
	problem "invariant or integrity failed while the burst was running"
fi
check_integrity final

metrics_cache_seconds=5
sleep "$((metrics_cache_seconds + 1))"
metrics_file="$temporary_directory/metrics"
call_api 200 GET /metrics "${METRICS_BEARER_TOKEN:-}" "" >"$metrics_file"
gauges=""
for gauge in available held confirmed total; do
	gauges="$gauges $(metric_number "seat_reservation_seats_$gauge" "$show_id" "$metrics_file" || printf missing)"
done
printf 'metrics seat_gauges=%s api=%s %s %s %s\n' "$gauges" "$final_available" "$final_held" "$final_confirmed" "$final_total"
if [[ $gauges != " $final_available $final_held $final_confirmed $final_total" ]]; then
	problem "seat gauges in /metrics do not match the API"
fi
metric_delta() {
	local before after
	before=$(counter_value "$1" "$metrics_before" 2>/dev/null || printf '0')
	after=$(counter_value "$1" "$metrics_file" 2>/dev/null || printf '0')
	printf '%d' "$((after - before))"
}
check_counter() {
	local name=$1
	local delta=$2
	local observed=$3
	local slack=$4
	printf 'metric_counter %s=%d observed=%d' "$name" "$delta" "$observed"
	if (( slack > 0 )); then printf ' allowed_extra_from_aborts=%d' "$slack"; fi
	printf '\n'
	if (( delta < observed || delta > observed + slack )); then
		problem "metric counter $name is $delta but the script observed $observed"
	fi
}
check_counter confirmed "$(metric_delta seat_reservation_reservations_confirmed_total)" "$model_created" 0
check_counter seat_taken "$(metric_delta 'seat_reservation_reservations_declined_total{reason="seat_taken"}')" "$model_seat_taken" "$aborted_count"
check_counter per_user_limit "$(metric_delta 'seat_reservation_reservations_declined_total{reason="per_user_limit"}')" "$model_limit" "$aborted_count"
check_counter idempotency_conflict "$(metric_delta 'seat_reservation_reservations_declined_total{reason="idempotency_conflict"}')" "$model_conflicts" "$aborted_count"
check_counter idempotent_replay "$(metric_delta 'seat_reservation_reservations_declined_total{reason="idempotent_replay"}')" "$model_replays" "$aborted_count"
check_counter idempotent_replays_total "$(metric_delta seat_reservation_idempotent_replays_total)" "$model_replays" "$aborted_count"

if [[ -s $problems ]]; then
	printf 'result=fail: %d problems\n' "$(wc -l <"$problems" | tr -d ' ')" >&2
	head -40 "$problems" | sed 's/^/  - /' >&2
	exit 1
fi
printf 'result=pass\n'

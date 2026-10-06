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
price_paise=25000

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

edge_rejected() {
	local status=$1
	local body_file=$2
	case $status in
		429 | 502 | 503 | 504) ;;
		*) return 1 ;;
	esac
	[[ $(head -c 1 "$body_file" 2>/dev/null) != "{" ]]
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
	local attempt=0
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
	while :; do
		attempt=$((attempt + 1))
		status=$(curl "${arguments[@]}" "$base_url$path" 2>/dev/null) || status=000
		if (( attempt >= 60 )) || ! edge_rejected "$status" "$body_file"; then
			break
		fi
		sleep 2
	done
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

parallel_requests() {
	local input=$1
	local output=$2
	local parallel=$3
	local directory
	local round=0
	directory=$(mktemp -d "$temporary_directory/parallel.XXXXXX")
	cp "$input" "$directory/pending"
	: >"$directory/settled"
	while [[ -s $directory/pending ]]; do
		round=$((round + 1))
		awk -F '\t' -v base="$base_url" -v dir="$directory" -v parallel="$parallel" -v timeout="$timeout_seconds" '
			BEGIN { print "parallel"; print "parallel-max = " parallel }
			{
				if (NR > 1) print "next"
				printf "url = \"%s%s\"\n", base, $3
				printf "request = \"%s\"\n", $2
				if ($4 != "-") printf "header = \"Authorization: Bearer %s\"\n", $4
				printf "header = \"Content-Type: %s\"\n", ($7 == "" ? "application/json" : $7)
				if ($5 != "-") {
					body = $5
					gsub(/\\/, "\\\\", body)
					gsub(/"/, "\\\"", body)
					printf "data-binary = \"%s\"\n", body
				}
				printf "output = \"%s/%s\"\n", dir, $1
				printf "write-out = \"%s %%{http_code}\\n\"\n", $1
				print "silent"
				print "connect-timeout = 10"
				printf "max-time = %d\n", timeout
			}' "$directory/pending" >"$directory/config"
		curl --config "$directory/config" >"$directory/statuses" 2>/dev/null || true
		awk -F '\t' -v dir="$directory" -v statuses="$directory/statuses" -v last="$round" -v settled="$directory/settled" -v pending="$directory/pending.next" '
			BEGIN { while ((getline line < statuses) > 0) { split(line, parts, " "); code[parts[1]] = parts[2] } }
			{
				body = ""
				file = dir "/" $1
				while ((getline line < file) > 0) body = body line
				close(file)
				status = ($1 in code) ? code[$1] : "000"
				if ((status == "000" || (status ~ /^(429|502|503|504)$/ && substr(body, 1, 1) != "{")) && last < 30) {
					print > pending
					next
				}
				type = "-"
				if ($3 ~ /\/reserve$/) type = "R"
				if ($3 ~ /\/cancel$/) type = "C"
				print $1 "\t" status "\t" $6 "\t" type "\t" body > settled
			}' "$directory/pending"
		touch "$directory/pending.next"
		mv "$directory/pending.next" "$directory/pending"
		if [[ -s $directory/pending ]]; then
			sleep 2
		fi
	done
	cp "$directory/settled" "$output"
	awk -F '\t' '$4 != "-" { print $4 "\t" $2 "\t" $3 "\t" $5 }' "$output" >>"$receipts"
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
registration_pending="$temporary_directory/registration-pending"
registration_done="$temporary_directory/registration-done"
: >"$registration_done"
for (( index = 0; index < users; index++ )); do
	printf '%d\tPOST\t/auth/register\t-\t{"email":"burst-%s-%d@example.com","password":"password123"}\t-\n' "$index" "$run_id" "$index"
done >"$registration_pending"
registration_rounds=0
while [[ -s $registration_pending ]]; do
	registration_rounds=$((registration_rounds + 1))
	if (( registration_rounds > 300 )); then
		fail_burst "could not register all users"
	fi
	parallel_requests "$registration_pending" "$temporary_directory/registration-round" 20
	awk -F '\t' '$2 == "201" { print $1 "\t" $5 }' "$temporary_directory/registration-round" >>"$registration_done"
	unexpected_registration=$(awk -F '\t' '$2 != "201" && $2 != "429" && $2 != "000" { print "user " $1 " got " $2; exit }' "$temporary_directory/registration-round")
	if [[ -n $unexpected_registration ]]; then
		fail_burst "registration failed: $unexpected_registration"
	fi
	awk -F '\t' -v round="$temporary_directory/registration-round" '
		BEGIN { while ((getline line < round) > 0) { split(line, parts, "\t"); if (parts[2] != "201") retry[parts[1]] = 1 } }
		$1 in retry' "$registration_pending" >"$registration_pending.next"
	mv "$registration_pending.next" "$registration_pending"
	if [[ -s $registration_pending ]]; then
		sleep 1
	fi
done
while IFS=$'\t' read -r _ registration_body; do
	user_token=$(json_string access_token <<<"$registration_body")
	tokens+=("$user_token")
	user_ids+=("$(json_string user_id <<<"$registration_body")")
	printf '%s\n' "$user_token" >>"$tokens_file"
done < <(sort -n -k1,1 "$registration_done")
if (( ${#tokens[@]} != users )); then
	fail_burst "registered ${#tokens[@]} of $users users"
fi
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
	"$(printf '{"name":"burst-%s","seats":[%s],"price_paise":%d,"per_user_limit":%d}' "$run_id" "$seats_json" "$price_paise" "$per_user_limit")")
show_id=$(json_string show_id <<<"$show_response")
if [[ -z $show_id ]]; then
	fail_burst "show response did not contain show_id"
fi
created_available=$(grep -o '"state":"available"' <<<"$show_response" | wc -l | tr -d ' ')
if (( created_available != seat_total )); then
	fail_burst "new show has $created_available of $seat_total seats available"
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
add_reserve() {
	local payload
	printf -v payload '{"seats":["%s"],"idempotency_key":"%s"}' "${4//,/\",\"}" "$3"
	add "$1" "$2" u POST "$reserve_path" "$payload" application/json "$5" "$3" "$4" "$6"
}

{
	for (( user = 0; user < spam_users; user++ )); do
		for (( index = 0; index < spam_requests; index++ )); do
			pair=$((index / 2))
			add_reserve spam "$user" "$run_id-spam-$user-$pair" "SP${user}_$((pair % spam_seats + 1))" 0 "$user"
		done
	done
	for (( index = 0; index < same_users; index++ )); do
		for (( repeat = 0; repeat < storm_requests; repeat++ )); do
			add_reserve storm_same "$((same_first + index))" "$run_id-same-$index" "SS$index" 0 "$index"
		done
	done
	for (( index = 0; index < diff_users; index++ )); do
		for (( repeat = 0; repeat < storm_requests; repeat++ )); do
			if (( repeat % 2 == 0 )); then seat="SDA$index"; else seat="SDB$index"; fi
			add_reserve storm_diff "$((diff_first + index))" "$run_id-diff-$index" "$seat" 0 "$index"
		done
	done
	for (( index = 0; index < cancel_owners; index++ )); do
		owner=$((cancel_first + index))
		cancel_path="/reservations/${cancel_ids[$index]}/cancel"
		for (( repeat = 0; repeat < cancel_owner_requests; repeat++ )); do
			add cancel_owner "$owner" u POST "$cancel_path" - - 0 - "CX$index" "$index"
			add_reserve replay_vs_cancel "$owner" "${cancel_keys[$index]}" "CX$index" 0 "${cancel_ids[$index]}"
		done
		add cancel_cross "$((pool_first + RANDOM % pool_size))" u POST "$cancel_path" - - 0 - "CX$index" "$index"
		add cancel_anonymous "$owner" none POST "$cancel_path" - - 0 - "CX$index" "$index"
		add_reserve regrab "$owner" "$run_id-rebook-own-$index" "CX$index" 0 "$index"
		for (( repeat = 0; repeat < cancel_challengers; repeat++ )); do
			add_reserve regrab "$((pool_first + RANDOM % pool_size))" "$run_id-regrab-$index-$repeat" "CX$index" 0 "$index"
		done
	done
	many_seats=$(for (( index = 0; index < 300; index++ )); do printf '"Q%03d",' "$index"; done)
	many_seats=${many_seats%,}
	long_key=$(printf 'k%.0s' {1..300})
	for (( repeat = 0; repeat < 8; repeat++ )); do
		user=$((pool_first + RANDOM % pool_size))
		key="$run_id-hostile-$repeat"
		add hostile_no_token "$user" none POST "$reserve_path" "{\"seats\":[\"HOT\"],\"idempotency_key\":\"$key-a\"}" application/json 0 - - 401
		add hostile_bad_token "$user" bad POST "$reserve_path" "{\"seats\":[\"HOT\"],\"idempotency_key\":\"$key-b\"}" application/json 0 - - 401
		add hostile_spoof_user "$user" u POST "$reserve_path" "{\"seats\":[\"HOT\"],\"idempotency_key\":\"$key-c\",\"user_id\":\"${user_ids[0]}\"}" application/json 0 - - 400
		add hostile_bad_json "$user" u POST "$reserve_path" '{"seats":[' application/json 0 - - 400
		add hostile_empty_seats "$user" u POST "$reserve_path" "{\"seats\":[],\"idempotency_key\":\"$key-d\"}" application/json 0 - - 400
		add hostile_duplicate_seats "$user" u POST "$reserve_path" "{\"seats\":[\"HOT\",\"HOT\"],\"idempotency_key\":\"$key-e\"}" application/json 0 - - 400
		add hostile_no_key "$user" u POST "$reserve_path" '{"seats":["HOT"]}' application/json 0 - - 400
		add hostile_long_key "$user" u POST "$reserve_path" "{\"seats\":[\"HOT\"],\"idempotency_key\":\"$long_key\"}" application/json 0 - - 400
		add hostile_control_seat "$user" u POST "$reserve_path" "{\"seats\":[\"S\\u0001\"],\"idempotency_key\":\"$key-f\"}" application/json 0 - - 400
		add hostile_wrong_content_type "$user" u POST "$reserve_path" "{\"seats\":[\"HOT\"],\"idempotency_key\":\"$key-g\"}" text/plain 0 - - 415
		add hostile_bad_show_id "$user" u POST /shows/not-a-uuid/reserve "{\"seats\":[\"HOT\"],\"idempotency_key\":\"$key-h\"}" application/json 0 - - 400
		add hostile_unknown_show "$user" u POST /shows/00000000-0000-0000-0000-000000000000/reserve "{\"seats\":[\"HOT\"],\"idempotency_key\":\"$key-i\"}" application/json 0 - - 404
		add hostile_unknown_seat "$user" u POST "$reserve_path" "{\"seats\":[\"NOPE-1\"],\"idempotency_key\":\"$key-j\"}" application/json 0 "$key-j" NOPE-1 409
		add hostile_unknown_plus_hot "$user" u POST "$reserve_path" "{\"seats\":[\"HOT\",\"NOPE-2\"],\"idempotency_key\":\"$key-l\"}" application/json 0 "$key-l" HOT,NOPE-2 409
		add hostile_300_seats "$user" u POST "$reserve_path" "{\"seats\":[$many_seats],\"idempotency_key\":\"$key-k\"}" application/json 0 "$key-k" - 409
		add hostile_cancel_bad_id "$user" u POST /reservations/not-a-uuid/cancel - - 0 - - 400,404
		add hostile_cancel_unknown "$user" u POST /reservations/00000000-0000-0000-0000-000000000000/cancel - - 0 - - 404
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
		add_reserve hot "$((pool_first + RANDOM % pool_size))" "$run_id-hot-$index" HOT 0 -
	done
	multi_patterns=(M1,M2 M2,M3 M4,M5,M6 M6,M7 M8,M9,M10)
	for (( index = 0; index < multi_count; index++ )); do
		pattern=$((index % 5))
		add_reserve multi "$((multi_first + pattern))" "$run_id-multi-$index" "${multi_patterns[$pattern]}" 0 "$pattern"
	done
	for (( index = 0; index < probe_count; index++ )); do
		add_reserve probe "$probe_user" "$run_id-probe-$index" PROBE,TAKEN 0 -
	done
	for (( index = 0; index < soup_count; index++ )); do
		count=$((1 + RANDOM % 3))
		seats=
		for (( position = 0; position < count; position++ )); do
			pick=$((RANDOM % 100))
			if (( pick < 8 )); then
				seat=HOT
			elif (( pick < 16 )); then
				seat="CX$((RANDOM % cancel_owners))"
			else
				printf -v seat 'R%02d' "$((1 + RANDOM % soup_seats))"
			fi
			if [[ ,$seats, != *",$seat,"* ]]; then
				seats=${seats:+$seats,}$seat
			fi
		done
		abort=0
		if (( RANDOM % 100 < abort_percent )); then
			abort=1
		fi
		add_reserve soup "$((pool_first + RANDOM % pool_size))" "$run_id-soup-$index" "$seats" "$abort" -
	done
} >"$plan_unsorted"

plan="$temporary_directory/plan"
sort -n -k1,1 "$plan_unsorted" | cut -f2- | awk -F '\t' 'BEGIN { OFS = "\t" } { print NR - 1, $0 }' >"$plan"
planned=$(wc -l <"$plan" | tr -d ' ')
mkdir "$temporary_directory/bodies"
batch_size=250
processes=$(( (concurrency + batch_size - 1) / batch_size ))
per_process=$(( (concurrency + processes - 1) / processes ))
config_directory="$temporary_directory/burst-configs"
mkdir "$config_directory"
awk -F '\t' \
	-v base="$base_url" -v run="$run_id" -v dir="$temporary_directory/bodies" \
	-v timeout="$timeout_seconds" -v processes="$processes" -v per_process="$per_process" \
	-v tokens="$tokens_file" -v configs="$config_directory" '
	BEGIN {
		while ((getline line < tokens) > 0) token[count++] = line
		for (process = 0; process < processes; process++) {
			file = configs "/" process ".curl"
			print "parallel" > file
			print "parallel-max = " per_process > file
		}
	}
	{
		index_value = $1; user = $3; mode = $4; method = $5; path = $6; payload = $7; ctype = $8; abort = $9
		file = configs "/" (NR - 1) % processes ".curl"
		if (written[file]++) print "next" > file
		printf "url = \"%s%s\"\n", base, path > file
		printf "request = \"%s\"\n", method > file
		if (mode == "u") printf "header = \"Authorization: Bearer %s\"\n", token[user] > file
		if (mode == "bad") print "header = \"Authorization: Bearer not.a.valid.token\"" > file
		if (ctype != "-") printf "header = \"Content-Type: %s\"\n", ctype > file
		printf "header = \"X-Request-ID: burst-%s-%d\"\n", run, index_value > file
		if (payload != "-") {
			gsub(/\\/, "\\\\", payload)
			gsub(/"/, "\\\"", payload)
			printf "data-binary = \"%s\"\n", payload > file
		}
		printf "output = \"%s/%d\"\n", dir, index_value > file
		printf "write-out = \"%d %%{http_code} %%{time_total}\\n\"\n", index_value > file
		print "silent" > file
		print "connect-timeout = 10" > file
		if (abort == 1) {
			printf "max-time = %.3f\n", 0.02 + (index_value % 50) / 100 > file
		} else {
			printf "max-time = %d\n", timeout > file
		}
	}' "$plan"

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
printf 'burst requests=%d in_flight_at_once=%d curl_processes=%d sample_request_id=burst-%s-0\n' "$planned" "$concurrency" "$processes" "$run_id"
burst_gate="$temporary_directory/burst-gate"
burst_pids=()
for (( process = 0; process < processes; process++ )); do
	(
		while [[ ! -e $burst_gate ]]; do
			sleep 0.01
		done
		exec curl --config "$config_directory/$process.curl" >"$config_directory/$process.statuses" 2>/dev/null
	) &
	burst_pids+=($!)
done
sleep 1
started_at=$(perl -MTime::HiRes=time -e 'printf "%.3f", time')
touch "$burst_gate"
for pid in "${burst_pids[@]}"; do
	wait "$pid" || true
done
finished_at=$(perl -MTime::HiRes=time -e 'printf "%.3f", time')
cat "$config_directory"/*.statuses >"$statuses"
duration_seconds=$(awk -v start="$started_at" -v end="$finished_at" 'BEGIN { printf "%.1f", end - start }')
slowest_request=$(awk '$3 > slowest { slowest = $3 } END { printf "%.1f", slowest }' "$statuses")
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

retry_results="$temporary_directory/retry-results"
retry_input="$temporary_directory/retry-input"
awk -F '\t' -v plan="$plan" -v tokens="$tokens_file" '
	BEGIN {
		while ((getline line < tokens) > 0) token[count++] = line
		while ((getline line < plan) > 0) {
			split(line, p, "\t")
			mode[p[1]] = p[4]; method[p[1]] = p[5]; path[p[1]] = p[6]; payload[p[1]] = p[7]; ctype[p[1]] = p[8]
		}
	}
	$4 == 1 || $8 == "000" || ($8 ~ /^(429|502|503|504)$/ && substr($14, 1, 1) != "{") {
		auth = "-"
		if (mode[$1] == "u") auth = token[$3]
		if (mode[$1] == "bad") auth = "not.a.valid.token"
		print $1 "\t" method[$1] "\t" path[$1] "\t" auth "\t" payload[$1] "\t" $5 "\t" (ctype[$1] == "-" ? "application/json" : ctype[$1])
	}' "$results" >"$retry_input"
: >"$retry_results"
if [[ -s $retry_input ]]; then
	parallel_requests "$retry_input" "$retry_results" 100
fi
aborted_count=$(awk -F '\t' '$4 == 1' "$results" | wc -l | tr -d ' ')
edge_count=$(awk -F '\t' '$4 != 1 && $8 ~ /^(429|502|503|504)$/ && substr($14, 1, 1) != "{"' "$results" | wc -l | tr -d ' ')
no_response_count=$(awk -F '\t' '$4 != 1 && $8 == "000"' "$results" | wc -l | tr -d ' ')
unsettled_count=$((aborted_count + edge_count + no_response_count))

settled="$temporary_directory/settled"
awk -F '\t' -v retries="$retry_results" '
	function field(body, name,    value) {
		if (match(body, "\"" name "\":\"[^\"]*\"")) {
			value = substr(body, RSTART, RLENGTH)
			sub("^\"" name "\":\"", "", value)
			sub("\"$", "", value)
			return value
		}
		return "-"
	}
	BEGIN {
		OFS = "\t"
		while ((getline line < retries) > 0) {
			split(line, r, "\t")
			retry_status[r[1]] = r[2]
			retry_body[r[1]] = r[5]
		}
	}
	{
		again = "-"
		again_body = "-"
		if ($1 in retry_status) {
			again = retry_status[$1]
			again_body = retry_body[$1]
			if ($8 == "000" || ($8 ~ /^(429|502|503|504)$/ && substr($14, 1, 1) != "{")) {
				$8 = again
				$14 = again_body
				$9 = field(again_body, "reservation_id")
				$10 = field(again_body, "status")
				$11 = field(again_body, "error")
				$12 = field(again_body, "user_id")
				$13 = "-"
				if (match(again_body, "\"seats\":\\[[^]]*\\]")) {
					$13 = substr(again_body, RSTART + 9, RLENGTH - 10)
					gsub(/"/, "", $13)
				}
			}
		}
		print $0, again, again_body
	}' "$results" >"$settled"

printf 'burst duration_seconds=%s slowest_request_seconds=%s\n' "$duration_seconds" "$slowest_request"
printf 'burst_retries aborted_by_client=%d rejected_by_railway_edge=%d no_response=%d retried_with_same_key=%d\n' \
	"$aborted_count" "$edge_count" "$no_response_count" "$(wc -l <"$retry_results" | tr -d ' ')"
awk -F '\t' '{ counts[$2 " " $8]++ } END { for (k in counts) print k, counts[k] }' "$settled" | sort |
	awk '{ if ($1 != kind) { if (line != "") print line; kind = $1; line = sprintf("    %-28s", $1) } line = line " " $2 ":" $3 } END { print line }'
awk -F '\t' '
	$8 == "201" { confirmed++ }
	$8 == "200" { replays++ }
	$8 ~ /^4/ { declined++ }
	$8 ~ /^5/ { server++ }
	$8 == "000" { client++ }
	END { printf "burst_summary confirmed=%d idempotent_replays=%d declined_4xx=%d server_5xx=%d no_response=%d (after retries)\n", confirmed, replays, declined, server, client }' "$settled"
awk -F '\t' '$8 == "409" { counts[$11]++ } END { for (k in counts) printf "declined reason=%s count=%d\n", k, counts[k] }' "$settled" | sort

problems="$temporary_directory/problems"
: >"$problems"
problem() {
	printf '%s\n' "$1" >>"$problems"
}

burst_won="$temporary_directory/burst-won"
awk -F '\t' -v spam_users="$spam_users" -v limit="$per_user_limit" -v won_out="$burst_won" '
	function field(body, name,    value) {
		if (match(body, "\"" name "\":\"[^\"]*\"")) {
			value = substr(body, RSTART, RLENGTH)
			sub("^\"" name "\":\"", "", value)
			sub("\"$", "", value)
			return value
		}
		return "-"
	}
	function claim(body,    rid, list, n, i, parts) {
		rid = field(body, "reservation_id")
		if (rid == "-" || rid in claimed) return
		claimed[rid] = 1
		if (match(body, "\"seats\":\\[[^]]*\\]")) {
			list = substr(body, RSTART + 9, RLENGTH - 10)
			gsub(/"/, "", list)
			n = split(list, parts, ",")
			for (i = 1; i <= n; i++) won[parts[i]]++
		}
	}
	function reservation(status, body) {
		if (status == "200" || status == "201") return field(body, "reservation_id")
		return ""
	}
	{
		retried = ($15 != "-")
		rid = reservation($8, $14)
		again = reservation($15, $16)
	}
	$2 !~ /cancel/ {
		if (rid != "") claim($14)
		if (again != "") claim($16)
	}
	$8 ~ /^5/ && substr($14, 1, 1) == "{" { print "5xx from the service: " $2 " request " $1 " returned " $8 }
	$8 ~ /^5/ && substr($14, 1, 1) != "{" { print "request " $1 " was still rejected by the edge with " $8 " after retries" }
	$8 == "000" { print "request " $1 " (" $2 ") never got a response, even after retries" }
	$4 == 1 && !retried { print "aborted request " $1 " was not retried" }
	$4 == 1 && retried && $15 !~ /^(200|201|409)$/ { print "retry of aborted request " $1 " returned " $15 }
	$2 ~ /^hostile/ {
		n = split($7, allowed, ",")
		ok = 0
		for (i = 1; i <= n; i++) if ($8 == allowed[i]) ok = 1
		if (!ok) print $2 " request " $1 " returned " $8 " expected " $7
	}
	$2 == "spam" {
		if ($8 !~ /^(200|201|409)$/) print "spam request " $1 " returned " $8
		if (rid != "") spam_rid[$3, rid] = 1
		if (again != "") spam_rid[$3, again] = 1
	}
	$2 == "storm_same" {
		if ($8 == "201") same_created[$5]++
		else if ($8 != "200") print "storm_same request " $1 " returned " $8
		if (rid != "") same_rid[$5, rid] = 1
		same_keys[$5] = 1
	}
	$2 == "storm_diff" {
		if ($8 == "201") diff_created[$5]++
		else if ($8 != "200" && !($8 == "409" && $11 == "idempotency_conflict")) print "storm_diff request " $1 " returned " $8 " " $11
		if (rid != "") diff_rid[$5, rid] = 1
		diff_keys[$5] = 1
	}
	$2 == "cancel_owner" && !($8 == "200" && $10 == "cancelled") { print "owner cancel request " $1 " returned " $8 " " $10 }
	$2 == "cancel_cross" && $8 != "404" { print "cross-user cancel request " $1 " returned " $8 }
	$2 == "cancel_anonymous" && $8 != "401" { print "anonymous cancel request " $1 " returned " $8 }
	$2 == "replay_vs_cancel" && !($8 == "200" && $9 == $7 && ($10 == "confirmed" || $10 == "cancelled")) { print "replay racing cancel request " $1 " returned " $8 " " $9 " " $10 }
	($2 == "regrab" || $2 == "hot" || $2 == "multi" || $2 == "soup") && !($8 ~ /^(201|409)$/ || ($8 == "200" && retried)) { print $2 " request " $1 " returned " $8 }
	$2 == "multi" && rid != "" { multi_rid[rid] = 1 }
	$2 == "probe" && !($8 == "409" && $11 == "seats_unavailable") { print "all-or-nothing request " $1 " returned " $8 " " $11 }
	END {
		for (pair in spam_rid) { split(pair, parts, SUBSEP); spam_count[parts[1]]++ }
		for (user = 0; user < spam_users; user++) if (spam_count[user] != limit) print "spam user " user " got " spam_count[user] + 0 " reservations, expected exactly " limit
		for (pair in same_rid) { split(pair, parts, SUBSEP); same_ids[parts[1]]++ }
		for (key in same_keys) if (same_created[key] > 1 || same_ids[key] != 1) print "same-key storm " key " created " same_created[key] + 0 " with " same_ids[key] + 0 " distinct reservations"
		for (pair in diff_rid) { split(pair, parts, SUBSEP); diff_ids[parts[1]]++ }
		for (key in diff_keys) if (diff_created[key] > 1 || diff_ids[key] != 1) print "different-body storm " key " created " diff_created[key] + 0 " with " diff_ids[key] + 0 " distinct reservations"
		for (rid in multi_rid) multi_won++
		for (seat in won) {
			if (won[seat] > 1) print "seat " seat " was won " won[seat] " times during the burst"
			print seat > won_out
		}
		if (won["HOT"] != 1) print "hot seat had " won["HOT"] + 0 " winners, expected 1"
		if (multi_won != 3) print "overlapping multi-seat groups had " multi_won + 0 " winners, expected 3"
	}' "$settled" >>"$problems"

touch "$burst_won"

sweep_input="$temporary_directory/sweep-input"
sweep_expected="$temporary_directory/sweep-expected"
awk -F '\t' -v tokens="$tokens_file" -v path="$reserve_path" -v expected_out="$sweep_expected" '
	BEGIN { while ((getline line < tokens) > 0) token[count++] = line }
	$8 == "201" && $5 != "-" && $2 != "storm_diff" && $15 == "-" && picked < 300 {
		seats = $6
		gsub(/,/, "\",\"", seats)
		printf "replay%d\tPOST\t%s\t%s\t{\"seats\":[\"%s\"],\"idempotency_key\":\"%s\"}\t%s\n", NR, path, token[$3], seats, $5, $5
		printf "replay%d\t%s\tconfirmed\n", NR, $9 > expected_out
		picked++
	}' "$settled" >"$sweep_input"
for (( index = 0; index < cancel_owners; index++ )); do
	owner=$((cancel_first + index))
	printf 'cancelled%d\tPOST\t%s\t%s\t%s\t%s\n' "$index" "$reserve_path" "${tokens[$owner]}" "$(reserve_payload "${cancel_keys[$index]}" "CX$index")" "${cancel_keys[$index]}" >>"$sweep_input"
	printf 'cancelled%d\t%s\tcancelled\n' "$index" "${cancel_ids[$index]}" >>"$sweep_expected"
	printf 'stale%d\tPOST\t/reservations/%s/cancel\t%s\t-\t-\n' "$index" "${cancel_ids[$index]}" "${tokens[$owner]}" >>"$sweep_input"
	printf 'stale%d\t%s\tcancelled\n' "$index" "${cancel_ids[$index]}" >>"$sweep_expected"
done
parallel_requests "$sweep_input" "$temporary_directory/sweep-output" 50
awk -F '\t' -v expected="$sweep_expected" '
	function field(body, name,    value) {
		if (match(body, "\"" name "\":\"[^\"]*\"")) {
			value = substr(body, RSTART, RLENGTH)
			sub("^\"" name "\":\"", "", value)
			sub("\"$", "", value)
			return value
		}
		return "-"
	}
	BEGIN { while ((getline line < expected) > 0) { split(line, parts, "\t"); want_id[parts[1]] = parts[2]; want_status[parts[1]] = parts[3] } }
	{
		if ($2 != "200" || field($5, "reservation_id") != want_id[$1] || field($5, "status") != want_status[$1])
			print "after the burst, " $1 " returned " $2 " " field($5, "reservation_id") " " field($5, "status") ", expected 200 " want_id[$1] " " want_status[$1]
	}' "$temporary_directory/sweep-output" >>"$problems"
printf 'after_burst replays=%d replays_of_cancelled=%d stale_cancels=%d\n' \
	"$(grep -c '^replay' "$sweep_input" || true)" "$cancel_owners" "$cancel_owners"

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
awk -F '\t' -v limit="$per_user_limit" -v seats_out="$model_seats" -v price="$price_paise" '
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
				amount = ""
				if (match(body, "\"amount_paise\":[0-9]+")) amount = substr(body, RSTART + 15, RLENGTH - 15)
				if (amount != price * split(list, counted, ",")) print "PROBLEM reservation " rid " charged " amount " for " list
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
	won=$(grep -cx "CX$index" "$burst_won" || true)
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
check_counter seat_taken "$(metric_delta 'seat_reservation_reservations_declined_total{reason="seat_taken"}')" "$model_seat_taken" "$unsettled_count"
check_counter per_user_limit "$(metric_delta 'seat_reservation_reservations_declined_total{reason="per_user_limit"}')" "$model_limit" "$unsettled_count"
check_counter idempotency_conflict "$(metric_delta 'seat_reservation_reservations_declined_total{reason="idempotency_conflict"}')" "$model_conflicts" "$unsettled_count"
check_counter idempotent_replay "$(metric_delta 'seat_reservation_reservations_declined_total{reason="idempotent_replay"}')" "$model_replays" "$unsettled_count"
check_counter idempotent_replays_total "$(metric_delta seat_reservation_idempotent_replays_total)" "$model_replays" "$unsettled_count"

if [[ -s $problems ]]; then
	printf 'result=fail: %d problems\n' "$(wc -l <"$problems" | tr -d ' ')" >&2
	head -40 "$problems" | sed 's/^/  - /' >&2
	exit 1
fi
printf 'result=pass\n'

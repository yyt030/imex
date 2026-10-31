#!/usr/bin/env bash
#
# Export every base table of one database (or of every user database) with
# imex. Table names come from `imex list tbl`, so the script follows the same
# filtering the CLI exposes: base tables only, `--pattern` for a LIKE filter,
# and `-D` as the schema scope.
#
# Output layout: <out-dir>/<database>/<table>.<format>
#
# Usage:
#   ./scripts/export-all-tables.sh -D shop -u root -p pass
#   IMEX_PASSWORD=pass ./scripts/export-all-tables.sh --all-databases -u root
#   ./scripts/export-all-tables.sh -D shop -u root -p pass -f sql --create \
#       --threads 4 -o /backup/shop
#
# Exit status: 0 when every export succeeded, 1 when any table failed (the
# failing tables are listed at the end), 2 for usage errors.

set -euo pipefail

PROG=${0##*/}
IMEX_BIN=${IMEX_BIN:-imex}
PASSWORD=${IMEX_PASSWORD:-}

DATABASE=""
ALL_DATABASES=0
INCLUDE_SYSTEM=0
HOST=""
PORT=""
USER=""
SOCKET=""
OUT_DIR="./export"
FORMAT="del"
THREADS=""
PATTERN=""
CREATE=0
NO_OVERWRITE=0
LOG_LEVEL=""
DRY_RUN=0
FAIL_FAST=0
EXTRA=()

usage() {
	cat <<EOF
Usage: $PROG -D DATABASE [options]
       $PROG --all-databases [options]

Export every base table of a database (or of every non-system database) with
\`imex export\`. One file per table is written to <out-dir>/<database>/.

Scope:
  -D, --database NAME      Database to export (required unless --all-databases)
      --all-databases      Export every non-system database
      --include-system     With --all-databases, keep information_schema, mysql,
                           performance_schema and sys
      --pattern LIKE       Only tables whose name matches the LIKE pattern
                           (% and _ are wildcards)

Connection (same meaning as imex):
  -h, --host HOST          Database host (imex default 127.0.0.1)
  -P, --port PORT          TCP port (imex default 3306)
  -u, --user USER          Database user (imex default root)
  -p, --password PASS      Password; defaults to IMEX_PASSWORD. ENC: blobs work
                           when IMEX_KEY is set
  -S, --socket PATH        UNIX socket instead of host/port

Output:
  -o, --out-dir DIR        Output root (default: ./export)
  -f, --format FORMAT      del, sql or parquet (default: del)
      --create             With -f sql, add CREATE TABLE to each file
      --threads N          Pass --threads N to every imex export
      --no-overwrite       Pass --no-overwrite to every imex export
      --log-level LEVEL    Pass --log-level to every imex export
      --imex PATH          imex binary (default: \$IMEX_BIN or "imex")

Control:
      --dry-run            Print the commands without running them
      --fail-fast          Stop after the first failed table
  -?, --help               Show this help

Extra arguments after -- are appended to every imex export command.
EOF
}

log() { printf '[%s] %s\n' "$PROG" "$*" >&2; }
die() { log "error: $*"; exit 2; }

need_value() {
	[[ $# -ge 2 && -n $2 ]] || die "option $1 needs a value"
}

while [[ $# -gt 0 ]]; do
	case $1 in
	-D | --database)
		need_value "$1" "${2:-}"
		DATABASE=$2
		shift 2
		;;
	--all-databases)
		ALL_DATABASES=1
		shift
		;;
	--include-system)
		INCLUDE_SYSTEM=1
		shift
		;;
	-h | --host)
		need_value "$1" "${2:-}"
		HOST=$2
		shift 2
		;;
	-P | --port)
		need_value "$1" "${2:-}"
		PORT=$2
		shift 2
		;;
	-u | --user)
		need_value "$1" "${2:-}"
		USER=$2
		shift 2
		;;
	-p | --password)
		need_value "$1" "${2:-}"
		PASSWORD=$2
		shift 2
		;;
	-S | --socket)
		need_value "$1" "${2:-}"
		SOCKET=$2
		shift 2
		;;
	-o | --out-dir)
		need_value "$1" "${2:-}"
		OUT_DIR=$2
		shift 2
		;;
	-f | --format)
		need_value "$1" "${2:-}"
		FORMAT=$2
		shift 2
		;;
	--threads)
		need_value "$1" "${2:-}"
		THREADS=$2
		shift 2
		;;
	--pattern)
		need_value "$1" "${2:-}"
		PATTERN=$2
		shift 2
		;;
	--create)
		CREATE=1
		shift
		;;
	--no-overwrite)
		NO_OVERWRITE=1
		shift
		;;
	--log-level)
		need_value "$1" "${2:-}"
		LOG_LEVEL=$2
		shift 2
		;;
	--imex)
		need_value "$1" "${2:-}"
		IMEX_BIN=$2
		shift 2
		;;
	--dry-run)
		DRY_RUN=1
		shift
		;;
	--fail-fast)
		FAIL_FAST=1
		shift
		;;
	-\? | --help)
		usage
		exit 0
		;;
	--)
		shift
		EXTRA=("$@")
		break
		;;
	*)
		die "unknown option: $1 (try --help)"
		;;
	esac
done

[[ -n $DATABASE || $ALL_DATABASES -eq 1 ]] || die "specify -D DATABASE or --all-databases"
[[ -z $DATABASE || $ALL_DATABASES -eq 0 ]] || die "-D and --all-databases are mutually exclusive"
[[ -n $PASSWORD ]] || die "password is required: use -p/--password or IMEX_PASSWORD"
case $FORMAT in
del | sql | parquet) ;;
*) die "invalid format: $FORMAT (expected del, sql or parquet)" ;;
esac
[[ $CREATE -eq 0 || $FORMAT == sql ]] || die "--create is only valid with -f sql"

command -v "$IMEX_BIN" >/dev/null 2>&1 || die "imex binary not found: $IMEX_BIN"

CONN=()
[[ -n $USER ]] && CONN+=(-u "$USER")
[[ -n $HOST ]] && CONN+=(-h "$HOST")
[[ -n $PORT ]] && CONN+=(-P "$PORT")
[[ -n $SOCKET ]] && CONN+=(-S "$SOCKET")
CONN+=(-p "$PASSWORD")

# list_raw runs one `imex list` query and prints stdout. The version banner and
# other logs go to stderr; on failure they are replayed so the real error is
# visible, on success they are dropped to keep the loop readable. The result is
# the aligned table format, so callers split it on two or more spaces.
list_raw() {
	local err out status
	err=$(mktemp)
	if out=$("$IMEX_BIN" list "$@" "${CONN[@]}" --no-col-names 2>"$err"); then
		status=0
	else
		status=$?
		cat "$err" >&2
	fi
	rm -f "$err"
	[[ $status -eq 0 ]] || return "$status"
	printf '%s\n' "$out"
}

list_databases() {
	list_raw dbs | awk -F'  +' 'NF >= 1 { print $1 }'
}

list_tables() {
	local db=$1 args=(tbl -D "$1")
	[[ -n $PATTERN ]] && args+=(--pattern "$PATTERN")
	list_raw "${args[@]}" | awk -F'  +' 'NF >= 2 { print $2 }'
}

# export_table writes one file and returns non-zero on failure.
export_table() {
	local db=$1 table=$2
	local dir="$OUT_DIR/$db"
	local safe=${table//\//_}
	local file="$dir/$safe.$FORMAT"
	local quoted
	quoted=$(printf '%s' "$table" | sed 's/`/``/g')

	local args=(export "${CONN[@]}" -D "$db" -q "SELECT * FROM \`$quoted\`" -o "$file" -f "$FORMAT")
	[[ -n $THREADS ]] && args+=(--threads "$THREADS")
	[[ $CREATE -eq 1 ]] && args+=(--create)
	[[ $NO_OVERWRITE -eq 1 ]] && args+=(--no-overwrite)
	[[ -n $LOG_LEVEL ]] && args+=(--log-level "$LOG_LEVEL")
	args+=("${EXTRA[@]}")

	if [[ $DRY_RUN -eq 1 ]]; then
		printf 'mkdir -p %q\n' "$dir"
		printf '%q ' "$IMEX_BIN" "${args[@]}"
		printf '\n'
		return 0
	fi

	mkdir -p "$dir"
	"$IMEX_BIN" "${args[@]}"
}

DATABASES=()
if [[ $ALL_DATABASES -eq 1 ]]; then
	if ! names=$(list_databases); then
		die "failed to list databases"
	fi
	while IFS= read -r db; do
		[[ -n $db ]] || continue
		case $db in
		information_schema | mysql | performance_schema | sys)
			[[ $INCLUDE_SYSTEM -eq 1 ]] && DATABASES+=("$db")
			;;
		*) DATABASES+=("$db") ;;
		esac
	done <<<"$names"
else
	DATABASES=("$DATABASE")
fi

[[ ${#DATABASES[@]} -gt 0 ]] || die "no databases to export"

TOTAL=0
FAILED=()
for db in "${DATABASES[@]}"; do
	if ! names=$(list_tables "$db"); then
		log "failed to list tables of $db"
		FAILED+=("$db.*")
		[[ $FAIL_FAST -eq 1 ]] && break
		continue
	fi
	tables=()
	while IFS= read -r table; do
		[[ -n $table ]] && tables+=("$table")
	done <<<"$names"

	if [[ ${#tables[@]} -eq 0 ]]; then
		log "$db: no tables matched"
		continue
	fi

	log "$db: exporting ${#tables[@]} table(s) to $OUT_DIR/$db"
	for i in "${!tables[@]}"; do
		table=${tables[$i]}
		log "$db.${table} ($((i + 1))/${#tables[@]})"
		if export_table "$db" "$table"; then
			TOTAL=$((TOTAL + 1))
		else
			log "failed: $db.$table"
			FAILED+=("$db.$table")
			if [[ $FAIL_FAST -eq 1 ]]; then
				break 2
			fi
		fi
	done
done

if [[ ${#FAILED[@]} -gt 0 ]]; then
	log "done with ${#FAILED[@]} failure(s); exported $TOTAL table(s)"
	printf 'failed: %s\n' "${FAILED[*]}" >&2
	exit 1
fi

log "done: exported $TOTAL table(s)"

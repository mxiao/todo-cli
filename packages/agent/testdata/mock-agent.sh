#!/bin/sh
# Mock agent for the agent package tests. The first argument selects the
# behaviour; the JSON input arrives on stdin (or in $TODO_AGENT_INPUT_FILE).
mode="$1"
cat > input.json
case "$mode" in
success)
	printf '{"type":"progress","stage":"prepare","percent":10,"message":"reading input"}\n'
	echo "plain output line"
	printf '{"type":"command","argv":["git","commit","-m","report"],"exit_code":0}\n'
	echo "# Report" > report.md
	printf '{"type":"result","result_type":"text","text":"Report written.\\nAll checks passed.","summary":"report done"}\n'
	printf '{"type":"result","result_type":"file","path":"report.md","summary":"weekly report"}\n'
	printf '{"type":"result","result_type":"commit","repo":"/tmp/repo","branch":"main","commit":"0123456789abcdef0123456789abcdef01234567","message":"add report"}\n'
	printf '{"type":"result","result_type":"command_output","argv":["make","test"],"exit_code":0,"stdout":"ok"}\n'
	printf '{"type":"progress","stage":"done","percent":100}\n'
	printf '{"type":"status","status":"succeeded","message":"all done"}\n'
	exit 0
	;;
fail)
	echo "starting" 
	echo "boom: missing dependency libfoo" >&2
	exit 3
	;;
partial)
	printf '{"type":"result","result_type":"text","text":"first half done"}\n'
	printf '{"type":"error","message":"second half failed: no network"}\n'
	exit 2
	;;
declared-partial)
	printf '{"type":"result","result_type":"text","text":"one of two files written"}\n'
	printf '{"type":"status","status":"partial","message":"1/2 files"}\n'
	exit 0
	;;
slow)
	i=0
	while [ $i -lt 300 ]; do
		echo "tick $i"
		i=$((i + 1))
		sleep 0.05
	done
	printf '{"type":"result","result_type":"text","text":"slow done"}\n'
	exit 0
	;;
trap)
	trap 'echo "got TERM" >&2; exit 143' TERM
	printf '{"type":"result","result_type":"text","text":"before cancel"}\n'
	while :; do sleep 0.05; done
	;;
secret)
	echo "api_key=sk-abcdefghijklmnop1234567890"
	echo "Authorization: Bearer abcdefghijklmnopqrstuvwxyz0123"
	echo "password=hunter2hunter2" >&2
	echo "custom token value: $MY_SERVICE_TOKEN"
	printf '{"type":"result","result_type":"text","text":"used key sk-abcdefghijklmnop1234567890"}\n'
	exit 0
	;;
flaky)
	n=$(cat count 2>/dev/null || echo 0)
	n=$((n + 1))
	echo "$n" > count
	if [ "$n" -lt 2 ]; then
		echo "transient failure $n" >&2
		exit 1
	fi
	printf '{"type":"result","result_type":"commit","repo":"r","branch":"b","commit":"abcdef1234567"}\n'
	exit 0
	;;
dup)
	printf '{"type":"result","result_type":"commit","repo":"r","branch":"main","commit":"abcdef1234567"}\n'
	printf '{"type":"result","result_type":"commit","repo":"r","branch":"main","commit":"abcdef1234567"}\n'
	printf '{"type":"result","result_type":"text","key":"a","text":"attempt %s"}\n' "$TODO_AGENT_ATTEMPT"
	printf '{"type":"result","result_type":"text","key":"b","text":"stable"}\n'
	exit "${DUP_EXIT:-0}"
	;;
env)
	env | cut -d= -f1 | sort | tr '\n' ' '
	echo
	exit 0
	;;
args)
	echo "args: $*"
	exit 0
	;;
status)
	printf '{"type":"result","result_type":"task_status","system":"jira","status":"Resolved","url":"https://jira.example/T-1","local_status":"done"}\n'
	exit 0
	;;
invalid)
	printf '{"type":"result","result_type":"commit","repo":"r"}\n'
	printf '{"type":"result","result_type":"text","text":"ok part"}\n'
	exit 0
	;;
text)
	echo "Line one of the answer"
	echo "Line two"
	exit 0
	;;
*)
	echo "unknown mode $mode" >&2
	exit 64
	;;
esac

#!/usr/bin/env bash
# Copyright 2025-2026 Patrick J. Scruggs
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -uo pipefail

if [[ $# -lt 4 || "$3" != "--" ]]; then
    echo "Usage: $0 <report-path> <report-uri> -- <generator-command> [args...]" >&2
    exit 2
fi

REPORT_PATH="$1"
REPORT_URI="$2"
shift 3
PYTHON_BINARY="${PYTHON_BINARY:-python3}"

validate_report() {
    "$PYTHON_BINARY" - "$REPORT_PATH" "$GENERATOR_RC" <<'PY'
import json
from pathlib import Path
import sys

report_path = Path(sys.argv[1])
generator_rc = int(sys.argv[2])
try:
    report = json.loads(report_path.read_text(encoding="utf-8"))
except (OSError, UnicodeError, json.JSONDecodeError) as exc:
    print(f"Dependency report is missing or invalid: {exc}", file=sys.stderr)
    raise SystemExit(1)

expected_status = "success" if generator_rc == 0 else "failure"
if not isinstance(report, dict):
    print("Dependency report must be a JSON object", file=sys.stderr)
    raise SystemExit(1)
if report.get("schema_version") != 1:
    print("Dependency report schema_version must be 1", file=sys.stderr)
    raise SystemExit(1)
if report.get("status") != expected_status:
    print(
        "Dependency report status does not match the generator exit status: "
        f"expected {expected_status!r}, got {report.get('status')!r}",
        file=sys.stderr,
    )
    raise SystemExit(1)
PY
}

write_fallback_report() {
    "$PYTHON_BINARY" - "$REPORT_PATH" "$GENERATOR_RC" <<'PY'
import json
import os
from pathlib import Path
import sys

report_path = Path(sys.argv[1])
generator_rc = int(sys.argv[2])
report_path.parent.mkdir(parents=True, exist_ok=True)
temporary_path = report_path.with_name(
    f".{report_path.name}.{os.getpid()}.fallback.tmp"
)
report = {
    "schema_version": 1,
    "status": "failure",
    "failed_stage": "generator_process",
    "error": {
        "type": "GeneratorProcessFailure",
        "message": "generator exited without a valid dependency report",
        "exit_code": generator_rc,
    },
}
try:
    temporary_path.write_text(
        json.dumps(report, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    os.replace(temporary_path, report_path)
finally:
    if temporary_path.exists():
        temporary_path.unlink()
PY
}

if ! rm -f -- "$REPORT_PATH"; then
    echo "Unable to remove stale dependency report at $REPORT_PATH" >&2
    exit 73
fi

set +e
"$@"
GENERATOR_RC=$?

REPORT_RC=0
if ! validate_report; then
    if (( GENERATOR_RC != 0 )); then
        write_fallback_report || REPORT_RC=$?
    else
        echo "Generator succeeded without a valid dependency report" >&2
        REPORT_RC=66
    fi
fi

UPLOAD_RC=0
if (( REPORT_RC == 0 )) && [[ -s "$REPORT_PATH" ]]; then
    gsutil -h "x-goog-if-generation-match:0" cp "$REPORT_PATH" "$REPORT_URI"
    UPLOAD_RC=$?
else
    echo "Dependency report was not produced at $REPORT_PATH" >&2
fi
set -e

if (( GENERATOR_RC != 0 )); then
    if (( REPORT_RC != 0 )); then
        echo "Fallback dependency report creation failed with status $REPORT_RC" >&2
    fi
    if (( UPLOAD_RC != 0 )); then
        echo "Dependency report upload also failed with status $UPLOAD_RC" >&2
    fi
    exit "$GENERATOR_RC"
fi

if (( REPORT_RC != 0 )); then
    exit "$REPORT_RC"
fi

exit "$UPLOAD_RC"

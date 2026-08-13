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

set +e
"$@"
GENERATOR_RC=$?

UPLOAD_RC=0
if [[ -s "$REPORT_PATH" ]]; then
    gsutil cp "$REPORT_PATH" "$REPORT_URI"
    UPLOAD_RC=$?
else
    echo "Dependency report was not produced at $REPORT_PATH" >&2
fi
set -e

if (( GENERATOR_RC != 0 )); then
    if (( UPLOAD_RC != 0 )); then
        echo "Dependency report upload also failed with status $UPLOAD_RC" >&2
    fi
    exit "$GENERATOR_RC"
fi

exit "$UPLOAD_RC"

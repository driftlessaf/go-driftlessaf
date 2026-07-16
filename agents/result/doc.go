/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

/*
Package result extracts a JSON result from free-form model text.

It exports two functions:

  - ExtractJSON pulls the JSON content out of a text response, stripping a
    surrounding markdown code fence when present and returning the trimmed
    input otherwise.
  - Extract combines ExtractJSON with json.Unmarshal into a caller-supplied
    type.

The executors use it as a text fallback: when a model replies with plain text
instead of calling the submit_result tool, the text is parsed as a
submit-shaped result:

	resp, err := result.Extract[Response](textContent)
	if err != nil {
		return response, true, fmt.Errorf("failed to parse response: %w", err)
	}
*/
package result

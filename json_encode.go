package slogcp

import (
	"encoding/json"
	"io"
)

type jsonEncoderOption func(*json.Encoder)

var jsonEncoderOptions = []jsonEncoderOption{
	// disableHTMLEscaping keeps JSON payloads readable by avoiding HTML entity
	// escaping.
	func(enc *json.Encoder) {
		enc.SetEscapeHTML(false)
	},
}

// encodeJSON writes payload as JSON using shared encoder configuration.
func encodeJSON(w io.Writer, payload any) error {
	enc := json.NewEncoder(w)
	for _, opt := range jsonEncoderOptions {
		opt(enc)
	}
	// Encode appends a newline and streams directly to the writer.
	return enc.Encode(payload)
}

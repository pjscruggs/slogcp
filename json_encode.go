package slogcp

import (
	"encoding/json"
	"io"
)

type jsonEncoderOption func(*json.Encoder)

var jsonEncoderOptions = []jsonEncoderOption{
	func(enc *json.Encoder) {
		enc.SetEscapeHTML(false)
	},
}

func encodeJSON(w io.Writer, payload any) error {
	enc := json.NewEncoder(w)
	for _, opt := range jsonEncoderOptions {
		opt(enc)
	}
	// Encode appends a newline and streams directly to the writer.
	return enc.Encode(payload)
}

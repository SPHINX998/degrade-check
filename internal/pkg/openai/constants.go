// Package openai holds the request shape of the fingerprint challenge on the Responses API.
package openai

import _ "embed"

// DefaultInstructions are the instructions the fingerprint bank's GPT answers were collected under.
//
//go:embed instructions.txt
var DefaultInstructions string

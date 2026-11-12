// Package claude holds the request shape of the fingerprint challenge on the Messages API.
// The fingerprint bank was fitted on answers to requests of this shape, and many relays
// only let Claude Code traffic through, so a test has to look like one.
package claude

// APIKeyBetaHeader is the anthropic-beta header Claude Code sends with an API key.
const APIKeyBetaHeader = "claude-code-20250219,interleaved-thinking-2025-05-14,fine-grained-tool-streaming-2025-05-14"

// DefaultHeaders are the headers Claude Code sends.
func DefaultHeaders() map[string]string {
	return map[string]string{
		"Anthropic-Dangerous-Direct-Browser-Access": "true",
		"User-Agent": "claude-cli/2.1.321 (external, cli)",
		"X-App": "cli",
		"X-Stainless-Arch": "arm64",
		"X-Stainless-Lang": "js",
		"X-Stainless-OS": "Linux",
		"X-Stainless-Package-Version": "0.94.0",
		"X-Stainless-Retry-Count": "0",
		"X-Stainless-Runtime": "node",
		"X-Stainless-Runtime-Version": "v24.3.0",
		"X-Stainless-Timeout": "600",
	}
}

// Package tmpl formats messages. It is internal: its exported function is
// reached through the API, but is not part of it.
package tmpl

// Render formats a message for a recipient.
func Render(to, body string) string { return "To: " + to + "\n\n" + body }

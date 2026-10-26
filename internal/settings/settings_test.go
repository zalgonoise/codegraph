package settings

import "testing"

func TestSplitWords(t *testing.T) {
	for name, want := range map[string]string{
		"LogLevel":    "Log_Level",
		"HTTPPort":    "HTTP_Port",
		"DatabaseURL": "Database_URL",
		"Port":        "Port",
		"MaxIdle2":    "Max_Idle2",
	} {
		if got := splitWords(name); got != want {
			t.Errorf("splitWords(%q) = %q, want %q", name, got, want)
		}
	}
}

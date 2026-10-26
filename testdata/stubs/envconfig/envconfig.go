// Package envconfig is a minimal offline stand-in for
// github.com/kelseyhightower/envconfig: the call the settings reader looks at.
// The real library fills the spec's fields from the environment by reflection.
package envconfig

func Process(prefix string, spec interface{}) error { return nil }

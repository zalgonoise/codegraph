// Package config loads settings with inline defaults and env overrides.
package config

import "os"

type Config struct {
	Addr         string
	DSN          string
	InventoryURL string
}

func Load() Config {
	c := Config{
		Addr:         ":8080",
		DSN:          "postgres://localhost/orders",
		InventoryURL: "http://inventory:8081",
	}
	if v := os.Getenv("ADDR"); v != "" {
		c.Addr = v
	}
	if v := os.Getenv("DATABASE_URL"); v != "" {
		c.DSN = v
	}
	if v := os.Getenv("INVENTORY_URL"); v != "" {
		c.InventoryURL = v
	}
	return c
}

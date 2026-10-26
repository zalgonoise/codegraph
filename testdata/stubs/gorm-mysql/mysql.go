// Package mysql is a minimal offline stand-in for gorm.io/driver/mysql.
package mysql

import "gorm.io/gorm"

var DefaultDriverName = "mysql"

type Config struct{ DSN string }

type Dialector struct{ *Config }

func Open(dsn string) gorm.Dialector { return &Dialector{Config: &Config{DSN: dsn}} }

func (dialector Dialector) Name() string { return DefaultDriverName }

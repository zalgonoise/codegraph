// Package gorm is a minimal offline stand-in for gorm.io/gorm: the builder
// and finisher methods codegraph's gorm rules look at.
package gorm

import "context"

type Dialector interface {
	Name() string
}

type Option interface{}

type DB struct {
	Error     error
	Statement struct{ Table string }
}

func Open(dialector Dialector, _ ...Option) (*DB, error) { return &DB{}, nil }

func (db *DB) WithContext(context.Context) *DB { return db }

func (db *DB) Table(name string, _ ...interface{}) *DB {
	tx := *db
	tx.Statement.Table = name
	return &tx
}

func (db *DB) Model(interface{}) *DB { return db }

func (db *DB) Where(interface{}, ...interface{}) *DB { return db }

func (db *DB) Create(interface{}) *DB { return db }

func (db *DB) Save(interface{}) *DB { return db }

func (db *DB) First(interface{}, ...interface{}) *DB { return db }

func (db *DB) Find(interface{}, ...interface{}) *DB { return db }

func (db *DB) Updates(interface{}) *DB { return db }

func (db *DB) Raw(string, ...interface{}) *DB { return db }

func (db *DB) Exec(string, ...interface{}) *DB { return db }

func (db *DB) Scan(interface{}) *DB { return db }

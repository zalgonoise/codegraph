// Package kvcache is a minimal offline stand-in for an in-house Redis
// abstraction: the engine embeds the cache repository, so calls are interface
// calls on ports.CacheRepository.
package kvcache

import (
	"context"
	"errors"

	"example.com/inhouse/kvcache/internal/ports"
)

var ErrKeyDoesntExists = errors.New("key doesn't exist")

type Engine struct {
	ports.CacheRepository
}

func NewEngine() (*Engine, error) { return &Engine{CacheRepository: memory{}}, nil }

type memory map[string][]byte

func (m memory) Del(_ context.Context, key string, _ bool) error { delete(m, key); return nil }

func (m memory) Get(_ context.Context, key string) ([]byte, error) {
	if v, ok := m[key]; ok {
		return v, nil
	}
	return nil, ErrKeyDoesntExists
}

func (m memory) Set(_ context.Context, key string, value []byte, _ int, _ bool) error {
	m[key] = value
	return nil
}

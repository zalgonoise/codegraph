package ports

import "context"

type CacheRepository interface {
	Del(ctx context.Context, key string, shadowKey bool) error
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, value []byte, ttl int, shadowKey bool) error
}

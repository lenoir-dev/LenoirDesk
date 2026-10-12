package execution

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// WriteLimiter is a fail-closed, provider-connection outbound write guard.
// It is deliberately separate from the durable retry schedule in PostgreSQL.
type WriteLimiter interface {
	Allow(context.Context, string, string) (time.Duration, error)
}

type RedisWriteLimiter struct {
	Client *redis.Client
	Limit  int64
	Window time.Duration
}

var writeLimitScript = redis.NewScript(`
local current=redis.call('INCR',KEYS[1])
if current==1 then redis.call('PEXPIRE',KEYS[1],ARGV[1]) end
if current>tonumber(ARGV[2]) then return redis.call('PTTL',KEYS[1]) end
return 0`)

var writeLimitPeekScript = redis.NewScript(`
local current=tonumber(redis.call('GET',KEYS[1]) or '0')
if current<tonumber(ARGV[1]) then return 0 end
local remaining=redis.call('PTTL',KEYS[1])
if remaining<1 then return tonumber(ARGV[2]) end
return remaining`)

// Peek avoids doing a fresh provider snapshot when a new Contact cannot yet
// be written. It does not consume a token; Allow still fences the write.
func (l *RedisWriteLimiter) Peek(ctx context.Context, tenantID, connectionID string) (time.Duration, error) {
	if l == nil || l.Client == nil {
		return 0, errors.New("redis write limiter unavailable")
	}
	limit, window := l.Limit, l.Window
	if limit <= 0 {
		limit = 5
	}
	if window <= 0 {
		window = time.Second
	}
	key := "relaydesk:write:" + tenantID + ":" + connectionID
	n, err := writeLimitPeekScript.Run(ctx, l.Client, []string{key}, limit, window.Milliseconds()).Int64()
	if err != nil {
		return 0, err
	}
	return time.Duration(n) * time.Millisecond, nil
}

func (l *RedisWriteLimiter) Allow(ctx context.Context, tenantID, connectionID string) (time.Duration, error) {
	if l == nil || l.Client == nil {
		return 0, errors.New("redis write limiter unavailable")
	}
	limit, window := l.Limit, l.Window
	if limit <= 0 {
		limit = 5
	}
	if window <= 0 {
		window = time.Second
	}
	key := "relaydesk:write:" + tenantID + ":" + connectionID
	n, err := writeLimitScript.Run(ctx, l.Client, []string{key}, window.Milliseconds(), limit).Int64()
	if err != nil {
		return 0, err
	}
	if n > 0 {
		return time.Duration(n) * time.Millisecond, nil
	}
	return 0, nil
}

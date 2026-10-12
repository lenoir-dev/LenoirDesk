package execution

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// ProviderBreaker isolates transient failures by provider account. Its Redis
// state is advisory; a denied probe returns a delay to the PostgreSQL action
// scheduler. Redis errors are fail-closed.
type ProviderBreaker struct{ Client *redis.Client }

var breakerBeforeScript = redis.NewScript(`
local t=redis.call('TIME')
local now=tonumber(t[1])*1000+math.floor(tonumber(t[2])/1000)
local opened=tonumber(redis.call('HGET',KEYS[1],'open_until') or '0')
if opened>now then return {opened-now,0} end
if opened>0 then
 if redis.call('SET',KEYS[2],ARGV[1],'PX',10000,'NX') then return {0,1} end
 return {10000,0}
end
return {0,0}`)

var breakerAfterScript = redis.NewScript(`
local t=redis.call('TIME')
local now=tonumber(t[1])*1000+math.floor(tonumber(t[2])/1000)
local outcome=ARGV[1]
local probe=redis.call('GET',KEYS[2])
local hadprobe=probe and probe==ARGV[2]
if probe and not hadprobe then return 'stale' end
if tonumber(redis.call('HGET',KEYS[1],'open_until') or '0')>0 and not hadprobe then return 'stale' end
if outcome=='success' then
 redis.call('DEL',KEYS[1],KEYS[2])
 return 'closed'
end
if outcome=='neutral' then
 if hadprobe then
  redis.call('HSET',KEYS[1],'open_until',now+1000)
  redis.call('PEXPIRE',KEYS[1],61000)
  redis.call('DEL',KEYS[2])
 end
 return 'neutral'
end
if hadprobe then
 redis.call('HSET',KEYS[1],'failures',3,'open_until',now+30000)
 redis.call('PEXPIRE',KEYS[1],61000)
 redis.call('DEL',KEYS[2])
 return 'open'
end
local failures=redis.call('HINCRBY',KEYS[1],'failures',1)
redis.call('PEXPIRE',KEYS[1],61000)
if failures>=3 then
 redis.call('HSET',KEYS[1],'open_until',now+30000)
 return 'open'
end
return 'closed'`)

func breakerKeys(provider, account string) (string, string) {
	key := quotaKey("breaker", provider, account)
	return key, key + ":probe"
}

func (b *ProviderBreaker) Before(ctx context.Context, provider, account string) (time.Duration, string, error) {
	if b == nil || b.Client == nil || provider == "" || account == "" {
		return 0, "", errors.New("provider breaker unavailable")
	}
	key, probe := breakerKeys(provider, account)
	token := uuid.NewString()
	raw, err := breakerBeforeScript.Run(ctx, b.Client, []string{key, probe}, token).Int64Slice()
	if err != nil || len(raw) != 2 {
		return 0, "", errors.New("provider breaker unavailable")
	}
	if raw[1] != 1 {
		token = ""
	}
	return time.Duration(raw[0]) * time.Millisecond, token, nil
}

func (b *ProviderBreaker) After(ctx context.Context, provider, account, probeToken string, status int, transportError bool) error {
	if b == nil || b.Client == nil || provider == "" || account == "" {
		return errors.New("provider breaker unavailable")
	}
	outcome := "neutral"
	if transportError || status >= 500 {
		outcome = "transient"
	} else if status >= 200 && status < 400 {
		outcome = "success"
	}
	key, probe := breakerKeys(provider, account)
	return breakerAfterScript.Run(ctx, b.Client, []string{key, probe}, outcome, probeToken).Err()
}

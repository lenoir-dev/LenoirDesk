package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

type Budget struct {
	Key    string
	Limit  int64
	Window time.Duration
}

// ProviderQuota is a fail-closed multi-budget admission guard. A cold Redis
// database waits a full budget window before admitting requests, including
// after a flush/restart. Counters are advisory; PostgreSQL owns retry timing.
type ProviderQuota struct{ Client *redis.Client }

func quotaKey(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return "relaydesk:quota:{rdq}:" + hex.EncodeToString(h.Sum(nil))
}

var quotaScript = redis.NewScript(`
local nowparts=redis.call('TIME')
local now=tonumber(nowparts[1])*1000+math.floor(tonumber(nowparts[2])/1000)
local n=tonumber(ARGV[1])
local wait=0
for i=1,n do
 local key=KEYS[i]
 local limit=tonumber(ARGV[2*i])
 local window=tonumber(ARGV[2*i+1])
 local marker=key..':warm'
 local values=redis.call('HMGET',key,'count','reset')
 if not values[1] then
  local ready=redis.call('GET',marker)
  if not ready then
   ready=now+window
   redis.call('SET',marker,ready,'PX',172800000)
  end
  wait=math.max(wait,tonumber(ready)-now)
 else
  local reset=tonumber(values[2])
  if reset>now and tonumber(values[1])>=limit then
   wait=math.max(wait,reset-now)
  end
 end
 local cooldown=redis.call('PTTL',key..':cooldown')
 if cooldown>wait then wait=cooldown end
end
if wait>0 or tonumber(ARGV[2*n+2])==0 then return wait end
for i=1,n do
 local key=KEYS[i]
 local window=tonumber(ARGV[2*i+1])
 local values=redis.call('HMGET',key,'count','reset')
 local count=0
 local reset=now+window
 if values[1] and tonumber(values[2])>now then
  count=tonumber(values[1])
  reset=tonumber(values[2])
 end
 redis.call('HSET',key,'count',count+1,'reset',reset)
 redis.call('PEXPIRE',key,math.max(1000,reset-now+1000))
end
return 0`)

var cooldownScript = redis.NewScript(`
local current=redis.call('PTTL',KEYS[1])
local requested=tonumber(ARGV[1])
if requested>current then redis.call('SET',KEYS[1],'1','PX',requested) end
return 1`)

func (q *ProviderQuota) Allow(ctx context.Context, budgets []Budget) (time.Duration, error) {
	return q.check(ctx, budgets, true)
}

// Peek reports the current admission delay without reserving provider
// capacity. Allow remains the authoritative gate immediately before HTTP.
func (q *ProviderQuota) Peek(ctx context.Context, budgets []Budget) (time.Duration, error) {
	return q.check(ctx, budgets, false)
}

func (q *ProviderQuota) check(ctx context.Context, budgets []Budget, reserve bool) (time.Duration, error) {
	if q == nil || q.Client == nil {
		return 0, errors.New("redis quota unavailable")
	}
	if len(budgets) == 0 || len(budgets) > 8 {
		return 0, errors.New("invalid provider quota budget count")
	}
	keys := make([]string, 0, len(budgets))
	args := make([]any, 0, 1+2*len(budgets))
	args = append(args, len(budgets))
	for _, b := range budgets {
		if b.Key == "" || b.Limit < 1 || b.Window < time.Second || b.Window > 24*time.Hour {
			return 0, errors.New("invalid provider quota budget")
		}
		keys = append(keys, b.Key)
		args = append(args, b.Limit, b.Window.Milliseconds())
	}
	if reserve {
		args = append(args, 1)
	} else {
		args = append(args, 0)
	}
	ms, err := quotaScript.Run(ctx, q.Client, keys, args...).Int64()
	if err != nil {
		return 0, err
	}
	return time.Duration(ms) * time.Millisecond, nil
}

func (q *ProviderQuota) Cooldown(ctx context.Context, key string, wait time.Duration) error {
	if q == nil || q.Client == nil || key == "" || wait < time.Millisecond || wait > time.Hour {
		return errors.New("invalid provider cooldown")
	}
	return cooldownScript.Run(ctx, q.Client, []string{key + ":cooldown"}, wait.Milliseconds()).Err()
}

type QuotaScope struct {
	Provider, App, Account, Method, Category, Channel string
}

func (s QuotaScope) Budgets() []Budget {
	switch s.Provider {
	case "hubspot":
		return []Budget{
			{quotaKey("hubspot", s.App, "app"), 8, 10 * time.Second},
			{quotaKey("hubspot", s.App, s.Account, "portal"), 8, 10 * time.Second},
		}
	case "stripe":
		return []Budget{
			{quotaKey("stripe", s.Account, "account"), 10, time.Second},
			{quotaKey("stripe", s.Account, s.Category), 5, time.Second},
		}
	case "slack":
		budgets := []Budget{
			{quotaKey("slack", s.App, s.Account, "workspace"), 20, time.Minute},
			{quotaKey("slack", s.App, s.Account, s.Method), 1, time.Second},
		}
		if s.Method == "chat.postMessage" && s.Channel != "" {
			budgets = append(budgets, Budget{quotaKey("slack", s.App, s.Account, "channel", s.Channel), 1, time.Second})
		}
		return budgets
	}
	return nil
}

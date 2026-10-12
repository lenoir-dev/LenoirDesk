package execution

import (
	"crypto/rand"
	"math"
	"math/big"
	"time"
)

type RetryPolicy struct {
	Base, Cap, MaxAge time.Duration
	MaxProviderCalls  int
	// Random returns a value in [0,1). Tests inject a deterministic sequence.
	Random func() float64
}

func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{Base: 5 * time.Second, Cap: 15 * time.Minute,
		// Each CRM attempt re-reads the Stripe customer, subscriptions and
		// invoice before contacting HubSpot. Twelve total calls can exhaust
		// after only three failed HubSpot attempts during a short outage.
		MaxAge: 24 * time.Hour, MaxProviderCalls: 120, Random: secureFraction}
}

func secureFraction() float64 {
	n, err := rand.Int(rand.Reader, big.NewInt(1<<53))
	if err != nil {
		return 0.5
	}
	return float64(n.Int64()) / float64(1<<53)
}

func (p RetryPolicy) next(failures int, minimum time.Duration) time.Duration {
	if p.Base <= 0 {
		p.Base = 5 * time.Second
	}
	if p.Cap <= 0 {
		p.Cap = 15 * time.Minute
	}
	if failures < 1 {
		failures = 1
	}
	ceiling := p.Base
	for i := 1; i < failures && ceiling < p.Cap; i++ {
		if ceiling > p.Cap/2 {
			ceiling = p.Cap
			break
		}
		ceiling *= 2
	}
	if ceiling > p.Cap {
		ceiling = p.Cap
	}
	random := p.Random
	if random == nil {
		random = secureFraction
	}
	f := random()
	if math.IsNaN(f) || f < 0 {
		f = 0
	}
	if f >= 1 {
		f = math.Nextafter(1, 0)
	}
	d := time.Duration(float64(ceiling) * f)
	if d < 100*time.Millisecond {
		d = 100 * time.Millisecond
	}
	if d < minimum {
		d = minimum
	}
	return d
}

func (p RetryPolicy) exhausted(now, first time.Time, providerCalls int) bool {
	if p.MaxAge <= 0 {
		p.MaxAge = 24 * time.Hour
	}
	if p.MaxProviderCalls <= 0 {
		p.MaxProviderCalls = 120
	}
	return providerCalls >= p.MaxProviderCalls || !now.Before(first.Add(p.MaxAge))
}

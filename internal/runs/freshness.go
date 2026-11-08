package runs

import "time"

// CanObserve rejects reverse arrivals and obsolete baselines, including an ABA
// force-push. Equality at the freshness deadline is expired.
func CanObserve(instance, current string, started, now time.Duration, baseline, factsRevision, ticket, accepted int64) bool {
	return instance == current && now >= started && now-started < 30*time.Second && baseline == factsRevision && ticket > accepted
}

package graph

import (
	"time"

	"github.com/RidgetopAi/backstory/internal/store"
)

// expiredDraft reports whether rec is a Draft (Tier inferred) whose own
// expiry has passed at now: the store's notExpiredDraft rule, which Search
// already applies. Walk leaves such a Record out when an edge reaches it,
// and does not go through it (#32: "Expired Drafts are left out"); a seed
// is kept, since its caller named it.
func expiredDraft(rec store.Record, now time.Time) bool {
	return rec.Tier == store.TierInferred && rec.ExpiresAt != nil && !rec.ExpiresAt.After(now)
}

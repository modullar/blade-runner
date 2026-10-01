// Package admit decides whether a commit may run on this machine. It is the join between the
// provider (which supplies a commit's signed bytes) and internal/trust (which verifies them
// against keys the owner chose): the machine, not GitHub, makes the decision.
package admit

import (
	"context"
	"strings"
	"sync"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/provider"
	"github.com/modullar/blade-runner/internal/trust"
)

// Subject is what would run: a repository at a commit. For a push it is the pushed commit; for a
// pull request it is the pull request's HEAD commit (never GitHub's synthetic merge commit,
// which nobody signed).
type Subject struct {
	Repository string
	SHA        string
}

// Admitter checks commits against the trust store.
type Admitter struct {
	Provider provider.Provider
	Verifier *trust.Verifier
	// Cache, when set, remembers the commits already fetched so that judging the same commit
	// again (the supervisor judges every waiting job at every poll) costs no API call. Only the
	// FETCH is remembered: a commit object never changes under its id, but who is trusted does,
	// so every Admit still verifies the signature against the trust store as it is now, and a
	// revocation takes effect at the next poll.
	Cache *CommitCache
}

// DefaultCacheBytes bounds the memory a CommitCache holds whatever its commit count: 16 MiB. A
// commit object is small, but a signed commit message is not bounded by anyone but its author.
const DefaultCacheBytes = 16 << 20

// CommitCache is a bounded, concurrency-safe memory of fetched commits, oldest evicted first
// (FIFO), bounded both by how many commits it holds and by the bytes they take.
type CommitCache struct {
	mu       sync.Mutex
	max      int
	maxBytes int
	bytes    int
	byKey    map[string]provider.Commit
	order    []string
}

// NewCommitCache returns a cache holding at most limit commits (at least 1) and at most
// DefaultCacheBytes bytes.
func NewCommitCache(limit int) *CommitCache { return NewCommitCacheBytes(limit, DefaultCacheBytes) }

// NewCommitCacheBytes returns a cache holding at most limit commits (at least 1) and at most
// maxBytes bytes of them (at least 1). A commit that alone exceeds the byte bound is not kept.
func NewCommitCacheBytes(limit, maxBytes int) *CommitCache {
	return &CommitCache{max: max(limit, 1), maxBytes: max(maxBytes, 1), byKey: map[string]provider.Commit{}}
}

// The key names the repository: the same id served by two repositories is two fetches, and a
// commit cached for one must never be served for the other.
func cacheKey(s Subject) string { return strings.ToLower(s.Repository) + "@" + s.SHA }

func sizeOf(key string, cm provider.Commit) int {
	n := len(key) + len(cm.SHA) + len(cm.Payload) + len(cm.Signature)
	for _, p := range cm.Parents {
		n += len(p)
	}
	return n
}

func (c *CommitCache) get(s Subject) (provider.Commit, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cm, ok := c.byKey[cacheKey(s)]
	return cm, ok
}

func (c *CommitCache) evictOldest() {
	k := c.order[0]
	c.bytes -= sizeOf(k, c.byKey[k])
	delete(c.byKey, k)
	c.order = c.order[1:]
}

func (c *CommitCache) put(s Subject, cm provider.Commit) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := cacheKey(s)
	if old, ok := c.byKey[k]; ok {
		c.bytes += sizeOf(k, cm) - sizeOf(k, old)
		c.byKey[k] = cm
	} else {
		if sizeOf(k, cm) > c.maxBytes {
			return
		}
		c.order = append(c.order, k)
		c.byKey[k] = cm
		c.bytes += sizeOf(k, cm)
	}
	for len(c.order) > c.max || c.bytes > c.maxBytes {
		c.evictOldest()
	}
}

// Len is how many commits are held.
func (c *CommitCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.byKey)
}

// Bytes is how many bytes the held commits take.
func (c *CommitCache) Bytes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}

// Admit returns who vouched for the commit, or a BR-E067 error saying why it is not admitted.
func (a *Admitter) Admit(ctx context.Context, s Subject) (trust.Verdict, error) {
	c, cached := provider.Commit{}, false
	if a.Cache != nil {
		c, cached = a.Cache.get(s)
	}
	var err error
	if !cached {
		c, err = a.Provider.Commit(ctx, s.Repository, s.SHA)
	}
	if err != nil {
		if diag.CodeOf(err) == "" {
			// Not a verdict on the commit: GitHub could not be asked. It must not carry
			// BR-E067, which callers read as "this commit is not allowed to run".
			return trust.Verdict{}, diag.Wrap(err, diag.CodeGitHubUnavailable, "cannot fetch commit "+s.SHA, "GitHub could not be reached", "re-run")
		}
		return trust.Verdict{}, err
	}
	if a.Cache != nil && !cached && c.Signature != "" {
		// Remember only bytes that really are this commit: a wrong answer from a misbehaving
		// server must not stick, and an unsigned commit is cheap to refuse again.
		if id, err := trust.ObjectID([]byte(c.Payload), c.Signature); err == nil && id == s.SHA {
			a.Cache.put(s, c)
		}
	}
	// The id is what was asked for, never what the server claims: a server that answers with a
	// different commit must not be able to choose what is verified.
	return a.Verifier.Verify(trust.Commit{SHA: s.SHA, Payload: c.Payload, Signature: c.Signature})
}

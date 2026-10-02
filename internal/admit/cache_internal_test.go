package admit

import (
	"strings"
	"testing"

	"github.com/modullar/blade-runner/internal/provider"
)

func TestACommitBiggerThanTheWholeBudgetFlushesNothingAndIsNotKept(t *testing.T) {
	small := provider.Commit{SHA: "s", Payload: "p"}
	c := NewCommitCacheBytes(10, 200)
	c.put(Subject{Repository: "o/r", SHA: "s"}, small)
	c.put(Subject{Repository: "o/r", SHA: "huge"}, provider.Commit{SHA: "huge", Payload: strings.Repeat("x", 1000)})
	if _, ok := c.get(Subject{Repository: "o/r", SHA: "s"}); !ok || c.Len() != 1 {
		t.Errorf("the small commit was flushed by one that could never fit: %d held", c.Len())
	}
}

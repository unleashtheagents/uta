package engine

import (
	"context"
	"testing"

	"github.com/unleashtheagents/uta/internal/provider"
)

func TestRoleContextDefaultsToWorker(t *testing.T) {
	if got := roleFromContext(context.Background()); got != provider.RoleWorker {
		t.Fatalf("default role = %q, want worker", got)
	}
	if got := roleFromContext(nil); got != provider.RoleWorker { //nolint:staticcheck // nil ctx is the defensive path
		t.Fatalf("nil ctx role = %q, want worker", got)
	}
	ctx := withRole(context.Background(), provider.RolePlanner)
	if got := roleFromContext(ctx); got != provider.RolePlanner {
		t.Fatalf("role = %q, want planner", got)
	}
	// A derived context keeps the role.
	sub, cancel := context.WithCancel(ctx)
	defer cancel()
	if got := roleFromContext(sub); got != provider.RolePlanner {
		t.Fatalf("derived role = %q, want planner", got)
	}
}

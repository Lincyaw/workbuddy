package cmd

import (
	"context"
	"log"
	"os/exec"
	"time"

	"github.com/Lincyaw/workbuddy/internal/ghutil"
)

// runRateLimitBudgetCheck performs an optional startup check for remaining REST
// rate-limit budget. It is best-effort by design and logs only warnings.
func runRateLimitBudgetCheck(parentCtx context.Context, component, repo string) {
	if _, err := exec.LookPath("gh"); err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(parentCtx, 5*time.Second)
	defer cancel()

	budget, low, err := ghutil.WarnIfLowBudgetDefault(ctx)
	if err != nil {
		log.Printf("[%s] rate limit budget check for %s failed: %v", component, repo, ghutil.RedactTokens(err.Error()))
		return
	}
	if low {
		log.Printf("[%s] warning: remaining rate limit for %s is low (%d/%d), check token scope and usage",
			component, repo, budget.Remaining, budget.Limit)
	}
}

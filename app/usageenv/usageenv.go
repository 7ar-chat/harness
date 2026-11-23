// Package usageenv builds what the usage ledger and the scheduled tasks need from the environment, for the processes that
// share them: harness-web (which serves the pages, refuses runs over a cap and fires the schedules) and harness-worker
// (which executes the runs, so it books them when they end).
package usageenv

import (
	"context"
	"sync"

	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/modelinfo"
)

var (
	priceOnce sync.Once
	table     *modelinfo.Table
)

// Price returns the model price lookup, or nil when price loading is switched off. The table loads in the background,
// once per process; until it arrives every model is unpriced (tokens are still counted).
func Price(cfg *config.Config) func(model string) (modelinfo.Info, bool) {
	mo, ok := config.ModelPriceOptions()
	if !ok {
		return nil
	}
	priceOnce.Do(func() { table = modelinfo.LoadAsync(context.Background(), mo) })
	return func(model string) (modelinfo.Info, bool) {
		if i, ok := table.Lookup(model, cfg.LLMProviderName); ok {
			return i, true
		}
		return table.Lookup(model, cfg.LLMProvider)
	}
}

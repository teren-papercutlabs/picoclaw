// PicoClaw PCL downstream — cost tracking config validation.
// All code in this file is PcL-only and is not part of upstream picoclaw.
// Downstream: permanent

package telemetry

import (
	"fmt"
	"strings"
)

// ValidateModelPrices checks that every model name referenced in agents or model_list
// has a corresponding entry in the cost tracking price table.
// Returns an error listing all unpriced models, or nil if all models are covered.
// If cost tracking is disabled (prices is nil or empty), returns nil (no enforcement).
func ValidateModelPrices(modelNames []string, prices map[string]ModelPrice) error {
	if len(prices) == 0 {
		return nil // cost tracking not configured — skip validation
	}

	var missing []string
	seen := make(map[string]bool)

	for _, name := range modelNames {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if _, ok := prices[name]; !ok {
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		return fmt.Errorf("cost_tracking.prices missing entries for models: %s — add pricing before deploying", strings.Join(missing, ", "))
	}
	return nil
}

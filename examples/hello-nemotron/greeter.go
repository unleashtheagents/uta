package hellonemotron

import "fmt"

// TierGreeting returns a formatted greeting from a specific NVIDIA Nemotron
// model tier hosted on Nebius Token Factory.
func TierGreeting(tier, modelID, role string) string {
	return fmt.Sprintf("[Nebius Token Factory] tier=%s model=%s role=%s", tier, modelID, role)
}

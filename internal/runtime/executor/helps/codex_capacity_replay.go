package helps

import "github.com/tidwall/gjson"

// CodexCapacityReplayGuard is deliberately conservative. Any generated item,
// tool activity or unknown event makes replay unsafe, even if no client bytes
// were delivered. Only admission/lifecycle events can precede a safe rejection.
type CodexCapacityReplayGuard struct{ unsafe bool }

func (g *CodexCapacityReplayGuard) Observe(payload []byte) {
	if g.unsafe {
		return
	}
	if output := gjson.GetBytes(payload, "response.output"); output.IsArray() && len(output.Array()) > 0 {
		g.unsafe = true
		return
	}
	switch gjson.GetBytes(payload, "type").String() {
	case "response.created", "response.in_progress", "response.queued", "rate_limits.updated", "error", "response.failed":
	default:
		g.unsafe = true
	}
}
func (g CodexCapacityReplayGuard) Safe() bool { return !g.unsafe }

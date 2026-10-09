package purview

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// maxContentChars is the purview-dlp-integration limit. A longer turn is rejected
// before Graph so a prefix is never treated as the whole message.
const maxContentChars = 100_000

// PurviewGuard is the Go form of the purview-dlp-integration skill.
// The delegated token is evaluated as /me. A blueprint app-only token is not used:
// Graph strips Content.Process.* from it.
type PurviewGuard struct {
	Enabled     bool
	CheckOutput bool
	FailClosed  bool
	Timeout     time.Duration
	loaded      bool
}

// LoadPurviewGuard reads the skill environment contract.
// PURVIEW_DLP_ENABLED defaults to true because this agent already has a published
// DLP policy. Set it to false to skip the gate. PURVIEW_TIMEOUT_MS defaults to
// 15000; the skill template uses 2000, which is too short for processContent.
func LoadPurviewGuard() PurviewGuard {
	timeout := 15 * time.Second
	if raw := strings.TrimSpace(os.Getenv("PURVIEW_TIMEOUT_MS")); raw != "" {
		if ms, err := strconv.Atoi(raw); err == nil && ms > 0 {
			timeout = time.Duration(ms) * time.Millisecond
		}
	}
	return PurviewGuard{
		Enabled:     envBool("PURVIEW_DLP_ENABLED", true),
		CheckOutput: envBool("PURVIEW_CHECK_OUTPUT", true),
		FailClosed:  !strings.EqualFold(strings.TrimSpace(os.Getenv("PURVIEW_FAIL_MODE")), "open"),
		Timeout:     timeout,
		loaded:      true,
	}
}

func envBool(name string, fallback bool) bool {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func (c *Client) guard() PurviewGuard {
	if c != nil && c.Guard.loaded {
		return c.Guard
	}
	return LoadPurviewGuard()
}

func (c *Client) Enabled() bool {
	return c.guard().Enabled
}

func (c *Client) OutputEnabled() bool {
	return c.guard().Enabled && c.guard().CheckOutput
}

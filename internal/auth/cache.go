package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// HostedCache keeps delegated test-user tokens and drops registration credentials.
func HostedCache(raw string) (string, error) {
	var file cacheFile
	if err := json.Unmarshal([]byte(raw), &file); err != nil || file.Entries == nil {
		return "", fmt.Errorf("token cache is not valid json")
	}
	kept := map[string]Token{}
	for key, tok := range file.Entries {
		if !hostedKey(key) || tok.RefreshToken == "" {
			continue
		}
		// Keep only the refresh token. Access tokens make the hosted env exceed
		// the 32 KiB limit, and the runtime renews them without a device prompt.
		tok.AccessToken = ""
		tok.ExpiresAt = time.Time{}
		kept[key] = tok
	}
	if len(kept) == 0 {
		return "", fmt.Errorf("token cache has no hosted login entries")
	}
	out, err := json.Marshal(cacheFile{Entries: kept})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// InstallCache writes a hosted token cache without logging its contents.
func InstallCache(path, raw string) error {
	filtered, err := HostedCache(raw)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(filtered), 0o600)
}

func hostedKey(key string) bool {
	return strings.HasPrefix(key, "runtime:") || strings.HasPrefix(key, "workiq:") || strings.HasPrefix(key, "mcp:")
}

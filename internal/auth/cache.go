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

// GraphCacheKey stores one Graph token per test user. The runtime client is shared.
func GraphCacheKey(clientID, user string) string {
	return "runtime:" + strings.TrimSpace(clientID) + ":" + strings.ToLower(strings.TrimSpace(user))
}

// LegacyGraphCacheKey is the old single Graph slot. It is moved to a user slot and then removed.
func LegacyGraphCacheKey(clientID string) string {
	return "runtime:" + strings.TrimSpace(clientID)
}

func ReadEntry(path, key string) (Token, bool, error) {
	file, err := loadCache(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Token{}, false, nil
		}
		return Token{}, false, err
	}
	tok, ok := file.Entries[key]
	return tok, ok, nil
}

func WriteEntry(path, key string, tok Token) error {
	file, err := loadCache(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if file.Entries == nil {
		file.Entries = map[string]Token{}
	}
	file.Entries[key] = tok
	return saveCache(path, file)
}

func DeleteEntry(path, key string) error {
	file, err := loadCache(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	delete(file.Entries, key)
	return saveCache(path, file)
}

func loadCache(path string) (cacheFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return cacheFile{}, err
	}
	var file cacheFile
	if err := json.Unmarshal(b, &file); err != nil {
		return cacheFile{}, err
	}
	return file, nil
}

func saveCache(path string, file cacheFile) error {
	if file.Entries == nil {
		file.Entries = map[string]Token{}
	}
	b, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

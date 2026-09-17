package kit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTruncate(t *testing.T) {
	if got := Truncate("short", 10); got != "short" {
		t.Fatalf("short string must be untouched, got %q", got)
	}
	if got := Truncate("abcdef", 3); got != "abc..." {
		t.Fatalf("got %q, want %q", got, "abc...")
	}
	if got := Truncate("", 0); got != "" {
		t.Fatalf("empty input must stay empty, got %q", got)
	}
}

func TestPickProxy(t *testing.T) {
	if got := pickProxy(nil, "round_robin"); got != "" {
		t.Fatalf("empty list must pick nothing, got %q", got)
	}

	list := []string{"http://a:1", "http://b:2", "http://c:3"}
	if got := pickProxy(list, "fill"); got != list[0] {
		t.Fatalf("fill must always pick the first, got %q", got)
	}
	if got := pickProxy(list, "random"); !contains(list, got) {
		t.Fatalf("random picked %q, not in list", got)
	}

	// round_robin 必须遍历整张表（游标是包级状态，所以看"用满一轮"）
	seen := map[string]bool{}
	for i := 0; i < len(list); i++ {
		seen[pickProxy(list, "round_robin")] = true
	}
	if len(seen) != len(list) {
		t.Fatalf("round_robin did not cycle through every proxy: %v", seen)
	}
	// 未知策略按 round_robin 处理
	if got := pickProxy(list, "unknown"); !contains(list, got) {
		t.Fatalf("unknown strategy should fall back to round_robin, got %q", got)
	}
}

func TestWithRetryJitterIsBounded(t *testing.T) {
	if got := WithRetryJitter(0); got != 0 {
		t.Fatalf("zero delay must stay zero, got %v", got)
	}
	if got := WithRetryJitter(-time.Second); got != -time.Second {
		t.Fatalf("negative delay must be returned as-is, got %v", got)
	}
	base := 100 * time.Millisecond
	for i := 0; i < 50; i++ {
		got := WithRetryJitter(base)
		if got < base || got > base+base/4 {
			t.Fatalf("jitter out of range: %v (base %v)", got, base)
		}
	}
}

func TestIdentityHelpers(t *testing.T) {
	if got := RandHex(8); len(got) != 16 || strings.Trim(got, "0123456789abcdef") != "" {
		t.Fatalf("RandHex(8) = %q, want 16 hex chars", got)
	}
	if got := RandIntn(0); got != 0 {
		t.Fatalf("RandIntn(0) = %d, want 0", got)
	}
	for i := 0; i < 50; i++ {
		if got := RandIntn(3); got < 0 || got >= 3 {
			t.Fatalf("RandIntn(3) = %d out of range", got)
		}
	}

	sessionA, requestA, uaA := FreshZenIdentity()
	sessionB, _, _ := FreshZenIdentity()
	if !strings.HasPrefix(sessionA, "sess_") || !strings.HasPrefix(requestA, "user_") {
		t.Fatalf("identity prefixes wrong: %q %q", sessionA, requestA)
	}
	if sessionA == sessionB {
		t.Fatal("two identities must not share a session id")
	}
	if !contains(ZenUserAgents, uaA) {
		t.Fatalf("user agent %q not from the pool", uaA)
	}
}

func TestResolveDataPathPrefersExistingDataDir(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	target := filepath.Join(dir, "data", "stats.db")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if got := ResolveDataPath("stats.db"); got != target {
		t.Fatalf("ResolveDataPath = %q, want the existing data/ file %q", got, target)
	}
	if !FileExists(target) {
		t.Fatal("FileExists should report the file we just wrote")
	}
	if FileExists(filepath.Join(dir, "data", "missing.db")) {
		t.Fatal("FileExists must be false for a missing file")
	}
}

func TestNewProxiedClientHasTimeout(t *testing.T) {
	c := NewProxiedClient(3 * time.Second)
	if c == nil || c.Timeout != 3*time.Second {
		t.Fatalf("client = %+v, want timeout 3s", c)
	}
	if c.Transport == nil {
		t.Fatal("client must carry a transport")
	}
	// 直连（无代理）时依旧可用
	RebuildHTTPClient(nil, "")
	if HTTPClient() == nil {
		t.Fatal("HTTPClient must never be nil")
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
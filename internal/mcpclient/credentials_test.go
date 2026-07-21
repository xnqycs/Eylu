package mcpclient

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestCredentialKeyNormalizesResourceURLAndAuth(t *testing.T) {
	config := OAuthOptions{
		Issuer:       "https://LOGIN.Example.com:443/issuer/",
		ClientID:     "eylu",
		ClientSecret: "secret",
		Scopes:       []string{"tools", "profile", "tools"},
	}
	hashOne, err := config.AuthHash()
	if err != nil {
		t.Fatal(err)
	}
	config.Scopes = []string{"profile", "tools"}
	hashTwo, err := config.AuthHash()
	if err != nil {
		t.Fatal(err)
	}
	if hashOne != hashTwo {
		t.Fatalf("scope order changed auth hash: %q != %q", hashOne, hashTwo)
	}
	first, err := CredentialKey("fixture", "HTTPS://MCP.Example.com:443/api?b=2&a=1", hashOne)
	if err != nil {
		t.Fatal(err)
	}
	second, err := CredentialKey("fixture", "https://mcp.example.com/api?a=1&b=2", hashTwo)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("equivalent resource URLs produced different keys: %q != %q", first, second)
	}
	config.ClientSecret = "rotated"
	rotated, err := config.AuthHash()
	if err != nil {
		t.Fatal(err)
	}
	if rotated == hashOne {
		t.Fatal("client secret rotation did not change auth hash")
	}
}

func TestCredentialStoreRoundTripAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "mcp_credentials.json")
	store := NewCredentialStore(path)
	want := OAuthCredential{
		AccessToken: "access", RefreshToken: "refresh", TokenType: "Bearer",
		Expiry: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC), Scope: []string{"tools"},
		DynamicClient: &DynamicClientCredentials{ClientID: "dynamic", ClientSecret: "registered-secret"},
	}
	if err := store.Put(context.Background(), "key", want); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(context.Background(), "key")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken || got.DynamicClient == nil || got.DynamicClient.ClientID != "dynamic" || !got.Expiry.Equal(want.Expiry) {
		t.Fatalf("credential = %#v", got)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("credential mode = %o", info.Mode().Perm())
		}
	}
	if err := store.Delete(context.Background(), "key"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), "key"); !errors.Is(err, ErrCredentialsNotFound) {
		t.Fatalf("Get after Delete error = %v", err)
	}
}

func TestCredentialStoreConcurrentWritersKeepAllEntries(t *testing.T) {
	store := NewCredentialStore(filepath.Join(t.TempDir(), "mcp_credentials.json"))
	const writers = 24
	var wait sync.WaitGroup
	errorsSeen := make(chan error, writers)
	for index := 0; index < writers; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			key := fmt.Sprintf("key-%02d", index)
			if err := store.Put(context.Background(), key, OAuthCredential{AccessToken: key}); err != nil {
				errorsSeen <- err
			}
		}(index)
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Error(err)
	}
	for index := 0; index < writers; index++ {
		key := fmt.Sprintf("key-%02d", index)
		credential, err := store.Get(context.Background(), key)
		if err != nil || credential.AccessToken != key {
			t.Fatalf("Get(%q) = %#v, %v", key, credential, err)
		}
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(store.Path()), ".mcp_credentials.json.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary credential files remain: %v", matches)
	}
}

func TestCredentialStoreTransactionCommitsAndRollsBack(t *testing.T) {
	store := NewCredentialStore(filepath.Join(t.TempDir(), "mcp_credentials.json"))
	rollbackErr := errors.New("rollback")
	err := store.Transaction(context.Background(), func(transaction *CredentialTransaction) error {
		if err := transaction.Put("rolled-back", OAuthCredential{AccessToken: "secret"}); err != nil {
			return err
		}
		return rollbackErr
	})
	if !errors.Is(err, rollbackErr) {
		t.Fatalf("transaction error = %v", err)
	}
	if _, err := store.Get(context.Background(), "rolled-back"); !errors.Is(err, ErrCredentialsNotFound) {
		t.Fatalf("rolled-back credential error = %v", err)
	}
	if err := store.Transaction(context.Background(), func(transaction *CredentialTransaction) error {
		if err := transaction.Put("one", OAuthCredential{AccessToken: "first"}); err != nil {
			return err
		}
		return transaction.Put("two", OAuthCredential{AccessToken: "second"})
	}); err != nil {
		t.Fatal(err)
	}
	if credential, err := store.Get(context.Background(), "two"); err != nil || credential.AccessToken != "second" {
		t.Fatalf("committed credential = %#v, %v", credential, err)
	}
}

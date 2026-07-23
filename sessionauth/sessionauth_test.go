package sessionauth

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLoadOrCreateTokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client-auth-token")
	createdToken, created, err := LoadOrCreateTokenFile(path)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if !created {
		t.Fatal("new token file was not reported as created")
	}
	loadedToken, createdAgain, err := LoadOrCreateTokenFile(path)
	if err != nil {
		t.Fatalf("load token: %v", err)
	}
	if createdAgain {
		t.Fatal("existing token file was recreated")
	}
	if createdToken != loadedToken {
		t.Fatal("loaded token differs from created token")
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatalf("stat token: %v", err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("token permissions = %04o, want 0600", info.Mode().Perm())
	}
}

func TestLoadOrCreateTokenFileIsAtomicAcrossConcurrentCreators(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client-auth-token")
	const workers = 16
	type result struct {
		token   Token
		created bool
		err     error
	}
	results := make(chan result, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, created, err := LoadOrCreateTokenFile(path)
			results <- result{token: token, created: created, err: err}
		}()
	}
	wg.Wait()
	close(results)

	var (
		expected  Token
		haveToken bool
		creators  int
	)
	for got := range results {
		if got.err != nil {
			t.Fatalf("concurrent LoadOrCreateTokenFile: %v", got.err)
		}
		if !haveToken {
			expected = got.token
			haveToken = true
		} else if got.token != expected {
			t.Fatal("concurrent creators observed different tokens")
		}
		if got.created {
			creators++
		}
	}
	if creators != 1 {
		t.Fatalf("reported creators = %d, want exactly 1", creators)
	}
}

func TestLoadTokenFileRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real-token")
	token, _, err := LoadOrCreateTokenFile(target)
	if err != nil || token == (Token{}) {
		t.Fatalf("create token target: %v", err)
	}
	link := filepath.Join(dir, "linked-token")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	if _, err := LoadTokenFile(link); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("LoadTokenFile symlink error = %v", err)
	}
}

func TestAuthenticateAndVerify(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	token := Token{1, 2, 3}
	serverResult := make(chan error, 1)
	go func() {
		serverResult <- Verify(context.Background(), server, token)
	}()
	if err := Authenticate(context.Background(), client, token); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if err := <-serverResult; err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestAuthenticateRejectsWrongToken(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	serverResult := make(chan error, 1)
	go func() {
		serverResult <- Verify(context.Background(), server, Token{1})
	}()
	clientErr := Authenticate(context.Background(), client, Token{2})
	if clientErr == nil || !strings.Contains(clientErr.Error(), "rejected") {
		t.Fatalf("client error = %v, want rejection", clientErr)
	}
	if !errors.Is(clientErr, ErrRejected) {
		t.Fatalf("client error = %v, want ErrRejected", clientErr)
	}
	if serverErr := <-serverResult; serverErr == nil {
		t.Fatal("server accepted the wrong token")
	}
}

func TestAuthenticateRejectsOversizedAcknowledgement(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	serverResult := make(chan error, 1)
	go func() {
		request := make([]byte, len(authPrefix)+len(Token{}))
		if _, err := io.ReadFull(server, request); err != nil {
			serverResult <- err
			return
		}
		_, err := server.Write(append(append([]byte(nil), authOK...), 'X'))
		serverResult <- err
	}()
	err := Authenticate(context.Background(), client, Token{1})
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("oversized acknowledgement error = %v, want ErrRejected", err)
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
}

func TestAuthenticateStopsOnContextCancellation(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := Authenticate(ctx, client, Token{1})
	if err == nil {
		t.Fatal("authentication unexpectedly succeeded")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("canceled authentication took too long: %s", time.Since(start))
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("context error = %v", ctx.Err())
	}
}

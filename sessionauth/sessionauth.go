// Package sessionauth authenticates a client inside the server-authenticated
// DTLS channel before any proxy payload is accepted.
package sessionauth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	tokenBytes       = 32
	encodedTokenSize = tokenBytes * 2
	authTimeout      = 5 * time.Second
)

var (
	authPrefix = []byte{'V', 'K', 'T', 'A', 1}
	authOK     = []byte{'V', 'K', 'T', 'A', 1, 'O', 'K'}
	authDenied = []byte{'V', 'K', 'T', 'A', 1, 'N', 'O'}

	// ErrRejected is safe for callers to classify and log: it contains no
	// credential material and means the server explicitly denied the token.
	ErrRejected = errors.New("server rejected client authentication")
)

// Token is a 256-bit bearer secret. It is sent only after the client has
// authenticated the server's pinned DTLS certificate.
type Token [tokenBytes]byte

// LoadTokenFile loads a 64-hex-character token from a private file.
func LoadTokenFile(path string) (Token, error) {
	var token Token
	path = strings.TrimSpace(path)
	if path == "" {
		return token, fmt.Errorf("client authentication token file is required")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return token, fmt.Errorf("stat client authentication token file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return token, fmt.Errorf("client authentication token path must not be a symbolic link")
	}
	if !info.Mode().IsRegular() {
		return token, fmt.Errorf("client authentication token path is not a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return token, fmt.Errorf("client authentication token file permissions %04o are too broad; use 0600", info.Mode().Perm())
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return token, fmt.Errorf("read client authentication token file: %w", err)
	}
	normalized := strings.TrimSpace(string(encoded))
	if len(normalized) != encodedTokenSize {
		return token, fmt.Errorf("client authentication token must contain exactly %d hexadecimal characters", encodedTokenSize)
	}
	decoded, err := hex.DecodeString(normalized)
	if err != nil {
		return token, fmt.Errorf("decode client authentication token: %w", err)
	}
	copy(token[:], decoded)
	var zero Token
	if subtle.ConstantTimeCompare(token[:], zero[:]) == 1 {
		return Token{}, fmt.Errorf("client authentication token must not be all zeroes")
	}
	return token, nil
}

// LoadOrCreateTokenFile loads a token, or creates a new private token file when
// the path does not exist. It never logs or returns a printable token.
func LoadOrCreateTokenFile(path string) (Token, bool, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return Token{}, false, fmt.Errorf("client authentication token file is required")
	}
	token, err := LoadTokenFile(path)
	if err == nil {
		return token, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Token{}, false, err
	}

	if _, err := io.ReadFull(rand.Reader, token[:]); err != nil {
		return Token{}, false, fmt.Errorf("generate client authentication token: %w", err)
	}
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return Token{}, false, fmt.Errorf("create temporary client authentication token file: %w", err)
	}
	tempPath := file.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return Token{}, false, fmt.Errorf("set client authentication token file permissions: %w", err)
	}
	writeErr := func() error {
		if _, err := fmt.Fprintf(file, "%s\n", hex.EncodeToString(token[:])); err != nil {
			return err
		}
		return file.Sync()
	}()
	closeErr := file.Close()
	if writeErr != nil {
		return Token{}, false, fmt.Errorf("write client authentication token file: %w", writeErr)
	}
	if closeErr != nil {
		return Token{}, false, fmt.Errorf("close client authentication token file: %w", closeErr)
	}
	// A hard link publishes the fully written, fsynced inode atomically without
	// replacing a token created concurrently by another server process.
	if err := os.Link(tempPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			loaded, loadErr := LoadTokenFile(path)
			return loaded, false, loadErr
		}
		return Token{}, false, fmt.Errorf("publish client authentication token file atomically: %w", err)
	}
	return token, true, nil
}

// Authenticate sends the client token and requires an acknowledgement before
// any application payload is written.
func Authenticate(ctx context.Context, conn net.Conn, token Token) error {
	stop, err := setAuthenticationDeadline(ctx, conn)
	if err != nil {
		return err
	}
	defer stop()

	frame := make([]byte, len(authPrefix)+len(token))
	defer clear(frame)
	copy(frame, authPrefix)
	copy(frame[len(authPrefix):], token[:])
	if err := writeFull(conn, frame); err != nil {
		return fmt.Errorf("send client authentication: %w", err)
	}
	response := make([]byte, len(authOK)+1)
	n, err := conn.Read(response)
	if err != nil {
		return fmt.Errorf("read client authentication acknowledgement: %w", err)
	}
	if n != len(authOK) || subtle.ConstantTimeCompare(response[:n], authOK) != 1 {
		return ErrRejected
	}
	return nil
}

// Verify reads and validates the first DTLS application datagram.
func Verify(ctx context.Context, conn net.Conn, token Token) error {
	stop, err := setAuthenticationDeadline(ctx, conn)
	if err != nil {
		return err
	}
	defer stop()

	expected := make([]byte, len(authPrefix)+len(token))
	defer clear(expected)
	copy(expected, authPrefix)
	copy(expected[len(authPrefix):], token[:])
	frame := make([]byte, len(expected)+1)
	defer clear(frame)
	n, readErr := conn.Read(frame)
	valid := readErr == nil && n == len(expected) && subtle.ConstantTimeCompare(frame[:n], expected) == 1
	if !valid {
		_ = writeFull(conn, authDenied)
		if readErr != nil {
			return fmt.Errorf("read client authentication: %w", readErr)
		}
		return fmt.Errorf("client authentication failed")
	}
	if err := writeFull(conn, authOK); err != nil {
		return fmt.Errorf("acknowledge client authentication: %w", err)
	}
	return nil
}

func writeFull(conn net.Conn, payload []byte) error {
	n, err := conn.Write(payload)
	if err != nil {
		return err
	}
	if n != len(payload) {
		return io.ErrShortWrite
	}
	return nil
}

func setAuthenticationDeadline(ctx context.Context, conn net.Conn) (func(), error) {
	deadline := time.Now().Add(authTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("set client authentication deadline: %w", err)
	}
	deadlineDone := make(chan struct{})
	stopContextDeadline := context.AfterFunc(ctx, func() {
		defer close(deadlineDone)
		_ = conn.SetDeadline(time.Now())
	})
	return func() {
		if !stopContextDeadline() {
			<-deadlineDone
		}
		_ = conn.SetDeadline(time.Time{})
	}, nil
}

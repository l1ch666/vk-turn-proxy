package diagnostics

import (
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
)

const (
	// TokenEnvironmentVariable is the non-command-line token source used when
	// -diagnostics-token-file is not set.
	TokenEnvironmentVariable  = "VK_TURN_DIAGNOSTICS_TOKEN"
	diagnosticsTokenHexLength = 64
	maxTokenFileBytes         = 4096
)

// Options contains the command-line diagnostics settings shared by both
// binaries. RegisterFlags binds these fields to a FlagSet.
type Options struct {
	ListenAddress string
	TokenFile     string
	EnablePprof   bool
}

// RegisterFlags installs diagnostics flags on fs. Diagnostics remain disabled
// unless -diagnostics-listen is explicitly set.
func RegisterFlags(fs *flag.FlagSet) *Options {
	if fs == nil {
		panic("diagnostics: nil FlagSet")
	}
	options := &Options{}
	fs.StringVar(&options.ListenAddress, "diagnostics-listen", "", "loopback diagnostics listen address; empty disables diagnostics")
	fs.StringVar(&options.TokenFile, "diagnostics-token-file", "", "file containing the 64-hex diagnostics Bearer token")
	fs.BoolVar(&options.EnablePprof, "diagnostics-pprof", false, "enable authenticated runtime pprof handlers")
	return options
}

// Config loads and validates the diagnostics token without putting it in the
// process command line. An explicit token file takes precedence over the
// VK_TURN_DIAGNOSTICS_TOKEN environment variable.
func (o Options) Config() (Config, error) {
	listenAddress := strings.TrimSpace(o.ListenAddress)
	tokenFile := strings.TrimSpace(o.TokenFile)
	if listenAddress == "" {
		if o.EnablePprof {
			return Config{}, fmt.Errorf("-diagnostics-pprof requires -diagnostics-listen")
		}
		if tokenFile != "" {
			return Config{}, fmt.Errorf("-diagnostics-token-file requires -diagnostics-listen")
		}
		return Config{}, nil
	}

	var (
		token string
		err   error
	)
	if tokenFile != "" {
		token, err = readTokenFile(tokenFile)
		if err != nil {
			return Config{}, err
		}
	} else {
		token = strings.TrimSpace(os.Getenv(TokenEnvironmentVariable))
	}

	config := Config{
		ListenAddress: listenAddress,
		BearerToken:   token,
		EnablePprof:   o.EnablePprof,
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func readTokenFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open diagnostics token file: %w", err)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("stat diagnostics token file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("diagnostics token file must be a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("diagnostics token file permissions must not allow group or other access")
	}

	data, err := io.ReadAll(io.LimitReader(file, maxTokenFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("read diagnostics token file: %w", err)
	}
	if len(data) > maxTokenFileBytes {
		return "", fmt.Errorf("diagnostics token file is too large")
	}
	token := strings.TrimSpace(string(data))
	if _, err := decodeToken(token); err != nil {
		return "", err
	}
	return token, nil
}

func decodeToken(token string) ([32]byte, error) {
	var decoded [32]byte
	if len(token) != diagnosticsTokenHexLength {
		return decoded, fmt.Errorf("diagnostics Bearer token must contain exactly %d hexadecimal characters", diagnosticsTokenHexLength)
	}
	n, err := hex.Decode(decoded[:], []byte(token))
	if err != nil || n != len(decoded) {
		return [32]byte{}, fmt.Errorf("diagnostics Bearer token must contain exactly %d hexadecimal characters", diagnosticsTokenHexLength)
	}
	return decoded, nil
}

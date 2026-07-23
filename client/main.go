// SPDX-FileCopyrightText: 2023 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"crypto/md5"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"

	"github.com/bschaatsbergen/dnsdialer"
	"github.com/cbeuw/connutil"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/l1ch666/vk-turn-proxy/v2/diagnostics"
	"github.com/l1ch666/vk-turn-proxy/v2/dtlsauth"
	"github.com/l1ch666/vk-turn-proxy/v2/metrics"
	"github.com/l1ch666/vk-turn-proxy/v2/sessionauth"
	"github.com/l1ch666/vk-turn-proxy/v2/tcputil"
	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
	"github.com/pion/logging"
	"github.com/pion/transport/v4"
	"github.com/pion/turn/v5"
	"github.com/xtaci/smux"
)

type getCredsFunc func(ctx context.Context, link string, streamID int) (string, string, string, error)

type directNet struct{}

type directDialer struct {
	*net.Dialer
}

type directListenConfig struct {
	*net.ListenConfig
}

// Global state trackers
var (
	globalCaptchaLockout atomic.Int64
	connectedStreams     atomic.Int32
	globalAppCancel      context.CancelFunc
	handshakeSem         = make(chan struct{}, 3)
	isDebug              bool
	manualCaptcha        bool
	autoCaptchaSliderPOC bool
)

type captchaSolveMode int

const (
	captchaSolveModeAuto captchaSolveMode = iota
	captchaSolveModeSliderPOC
	captchaSolveModeManual
)

func captchaSolveModeForAttempt(attempt int, manualOnly bool, enableSliderPOC bool) (captchaSolveMode, bool) {
	if manualOnly {
		return captchaSolveModeManual, attempt == 0
	}

	switch attempt {
	case 0:
		return captchaSolveModeAuto, true
	case 1:
		if enableSliderPOC {
			return captchaSolveModeSliderPOC, true
		}
		return captchaSolveModeManual, true
	case 2:
		if enableSliderPOC {
			return captchaSolveModeManual, true
		}
	}

	return 0, false
}

func captchaSolveModeLabel(mode captchaSolveMode) string {
	switch mode {
	case captchaSolveModeAuto:
		return "auto captcha"
	case captchaSolveModeSliderPOC:
		return "auto captcha slider POC"
	case captchaSolveModeManual:
		return "manual captcha"
	default:
		return "captcha"
	}
}

type UDPPacket struct {
	Data []byte
	N    int
}

var packetPool = sync.Pool{
	New: func() any { return &UDPPacket{Data: make([]byte, udpPacketBufferSize)} },
}

const udpPacketBufferSize = 2048

func newDirectNet() transport.Net {
	return directNet{}
}

func (directNet) ListenPacket(network string, address string) (net.PacketConn, error) {
	return net.ListenPacket(network, address)
}

func (directNet) ListenUDP(network string, locAddr *net.UDPAddr) (transport.UDPConn, error) {
	return net.ListenUDP(network, locAddr)
}

func (directNet) ListenTCP(network string, laddr *net.TCPAddr) (transport.TCPListener, error) {
	listener, err := net.ListenTCP(network, laddr)
	if err != nil {
		return nil, err
	}

	return directTCPListener{listener}, nil
}

func (directNet) Dial(network, address string) (net.Conn, error) {
	return net.Dial(network, address)
}

func (directNet) DialUDP(network string, laddr, raddr *net.UDPAddr) (transport.UDPConn, error) {
	return net.DialUDP(network, laddr, raddr)
}

func (directNet) DialTCP(network string, laddr, raddr *net.TCPAddr) (transport.TCPConn, error) {
	return net.DialTCP(network, laddr, raddr)
}

func (directNet) ResolveIPAddr(network, address string) (*net.IPAddr, error) {
	return net.ResolveIPAddr(network, address)
}

func (directNet) ResolveUDPAddr(network, address string) (*net.UDPAddr, error) {
	return net.ResolveUDPAddr(network, address)
}

func (directNet) ResolveTCPAddr(network, address string) (*net.TCPAddr, error) {
	return net.ResolveTCPAddr(network, address)
}

func (directNet) Interfaces() ([]*transport.Interface, error) {
	return nil, transport.ErrNotSupported
}

func (directNet) InterfaceByIndex(index int) (*transport.Interface, error) {
	return nil, fmt.Errorf("%w: index=%d", transport.ErrInterfaceNotFound, index)
}

func (directNet) InterfaceByName(name string) (*transport.Interface, error) {
	return nil, fmt.Errorf("%w: %s", transport.ErrInterfaceNotFound, name)
}

func (directNet) CreateDialer(dialer *net.Dialer) transport.Dialer {
	return directDialer{Dialer: dialer}
}

func (directNet) CreateListenConfig(listenerConfig *net.ListenConfig) transport.ListenConfig {
	return directListenConfig{ListenConfig: listenerConfig}
}

func (d directDialer) Dial(network, address string) (net.Conn, error) {
	return d.Dialer.Dial(network, address)
}

func (d directListenConfig) Listen(ctx context.Context, network, address string) (net.Listener, error) {
	return d.ListenConfig.Listen(ctx, network, address)
}

func (d directListenConfig) ListenPacket(ctx context.Context, network, address string) (net.PacketConn, error) {
	return d.ListenConfig.ListenPacket(ctx, network, address)
}

type directTCPListener struct {
	*net.TCPListener
}

func (l directTCPListener) AcceptTCP() (transport.TCPConn, error) {
	return l.TCPListener.AcceptTCP()
}

// region Helper: HTTP Headers Injection

// applyBrowserProfile applies consistent User-Agent and Client Hints to bypass WAFs
func applyBrowserProfile(req *http.Request, profile Profile) {
	req.Header.Set("User-Agent", profile.UserAgent)
	req.Header.Set("sec-ch-ua", profile.SecChUa)
	req.Header.Set("sec-ch-ua-mobile", profile.SecChUaMobile)
	req.Header.Set("sec-ch-ua-platform", profile.SecChUaPlatform)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("DNT", "1")
}

func applyBrowserProfileFhttp(req *fhttp.Request, profile Profile) {
	req.Header.Set("User-Agent", profile.UserAgent)
	req.Header.Set("sec-ch-ua", profile.SecChUa)
	req.Header.Set("sec-ch-ua-mobile", profile.SecChUaMobile)
	req.Header.Set("sec-ch-ua-platform", profile.SecChUaPlatform)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("DNT", "1")
}

func generateBrowserFp(profile Profile) string {
	data := profile.UserAgent + profile.SecChUa + "1920x1080x24" + strconv.FormatInt(time.Now().UnixNano(), 10)
	h := md5.Sum([]byte(data))
	return hex.EncodeToString(h[:])
}

// endregion

// region Automatic Captcha Solver & Authentication

type VkCaptchaError struct {
	ErrorCode               int
	ErrorMsg                string
	CaptchaSid              string
	CaptchaImg              string
	RedirectURI             string
	IsSoundCaptchaAvailable bool
	SessionToken            string
	CaptchaTs               string
	CaptchaAttempt          string
}

func ParseVkCaptchaError(errData map[string]interface{}) *VkCaptchaError {
	// Extract error_code
	codeFloat, ok := errData["error_code"].(float64)
	if !ok {
		log.Printf("missing error_code in captcha error data")
		return nil
	}
	code := int(codeFloat)

	// Modern VK captcha payloads carry redirect_uri/session_token for
	// captchaNotRobot. Legacy payloads can be image-only and still need to flow
	// into the manual fallback instead of being treated as a generic API error.
	redirectURI, _ := errData["redirect_uri"].(string)

	// captcha_sid is present on legacy image captcha payloads, but newer
	// redirect/session-token payloads may rely on success_token instead.
	var captchaSid string
	if sidText, ok := errData["captcha_sid"].(string); ok {
		captchaSid = sidText
	} else if sidNum, ok := errData["captcha_sid"].(float64); ok {
		captchaSid = fmt.Sprintf("%.0f", sidNum)
	}

	// New VK captcha payloads can be redirect/session-token only. captcha_img is
	// still useful for the legacy manual image fallback, but it is not required
	// for automatic captchaNotRobot solving.
	captchaImg, _ := errData["captcha_img"].(string)

	errorMsg, _ := errData["error_msg"].(string)

	// Extract session token if redirect_uri present
	var sessionToken string
	if redirectURI != "" {
		if parsed, err := neturl.Parse(redirectURI); err == nil {
			sessionToken = parsed.Query().Get("session_token")
		} else {
			log.Printf("failed to parse redirect_uri: %v", err)
			return nil
		}
	}

	// Extract is_sound_captcha_available
	isSound, ok := errData["is_sound_captcha_available"].(bool)
	if !ok {
		isSound = false
	}

	// Extract captcha_ts
	var captchaTs string
	if tsFloat, ok := errData["captcha_ts"].(float64); ok {
		captchaTs = fmt.Sprintf("%.0f", tsFloat)
	} else if tsStr, ok := errData["captcha_ts"].(string); ok {
		captchaTs = tsStr
	}

	// Extract captcha_attempt
	var captchaAttempt string
	if attFloat, ok := errData["captcha_attempt"].(float64); ok {
		captchaAttempt = fmt.Sprintf("%.0f", attFloat)
	} else if attStr, ok := errData["captcha_attempt"].(string); ok {
		captchaAttempt = attStr
	}

	if redirectURI == "" && captchaImg != "" && captchaSid == "" {
		log.Printf("missing captcha_sid in image captcha error data")
		return nil
	}

	// Build VkCaptchaError
	return &VkCaptchaError{
		ErrorCode:               code,
		ErrorMsg:                errorMsg,
		CaptchaSid:              captchaSid,
		CaptchaImg:              captchaImg,
		RedirectURI:             redirectURI,
		IsSoundCaptchaAvailable: isSound,
		SessionToken:            sessionToken,
		CaptchaTs:               captchaTs,
		CaptchaAttempt:          captchaAttempt,
	}
}

func (e *VkCaptchaError) IsCaptchaError() bool {
	return e.ErrorCode == 14 && ((e.RedirectURI != "" && e.SessionToken != "") || e.CaptchaImg != "")
}

func solveVkCaptcha(ctx context.Context, captchaErr *VkCaptchaError, streamID int, client tlsclient.HttpClient, profile Profile, useSliderPOC bool) (string, error) {
	if useSliderPOC {
		log.Printf("[STREAM %d] [Captcha] Solving captcha with slider POC...", streamID)
	} else {
		log.Printf("[STREAM %d] [Captcha] Solving captcha...", streamID)
	}

	if captchaErr.SessionToken == "" {
		return "", fmt.Errorf("no session_token in redirect_uri for auto-solve")
	}
	if captchaErr.RedirectURI == "" {
		return "", fmt.Errorf("no redirect_uri for auto-solve")
	}

	bootstrap, err := fetchCaptchaBootstrap(ctx, captchaErr.RedirectURI, client, profile)
	if err != nil {
		return "", fmt.Errorf("failed to fetch captcha bootstrap: %w", err)
	}

	log.Printf("[STREAM %d] [Captcha] PoW input: %s, difficulty: %d", streamID, bootstrap.PowInput, bootstrap.Difficulty)

	hash := solvePoW(bootstrap.PowInput, bootstrap.Difficulty)
	log.Printf("[STREAM %d] [Captcha] PoW solved: hash=%s", streamID, hash)

	var successToken string
	if useSliderPOC {
		successToken, err = callCaptchaNotRobotWithSliderPOC(
			ctx,
			captchaErr.SessionToken,
			hash,
			streamID,
			client,
			profile,
			bootstrap.Settings,
		)
	} else {
		successToken, err = callCaptchaNotRobot(ctx, captchaErr.SessionToken, hash, streamID, client, profile)
	}
	if err != nil {
		return "", fmt.Errorf("captchaNotRobot API failed: %w", err)
	}

	log.Printf("[STREAM %d] [Captcha] Success! Got success_token", streamID)
	return successToken, nil
}

func fetchCaptchaBootstrap(ctx context.Context, redirectURI string, client tlsclient.HttpClient, profile Profile) (*captchaBootstrap, error) {
	parsedURL, err := neturl.Parse(redirectURI)
	if err != nil {
		return nil, err
	}
	domain := parsedURL.Hostname()

	req, err := fhttp.NewRequestWithContext(ctx, "GET", redirectURI, nil)
	if err != nil {
		return nil, err
	}

	req.Host = domain
	applyBrowserProfileFhttp(req, profile)
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func(Body io.ReadCloser) {
		_ = Body.Close()
	}(resp.Body)

	body, err := readResponseBodyLimited(
		resp.Body,
		maxCaptchaBootstrapResponseBytes,
		"captcha bootstrap response",
	)
	if err != nil {
		return nil, err
	}
	return parseCaptchaBootstrapHTML(string(body))
}

func solvePoW(powInput string, difficulty int) string {
	target := strings.Repeat("0", difficulty)
	for nonce := 1; nonce <= 10000000; nonce++ {
		data := powInput + strconv.Itoa(nonce)
		hash := sha256.Sum256([]byte(data))
		hexHash := hex.EncodeToString(hash[:])
		if strings.HasPrefix(hexHash, target) {
			return hexHash
		}
	}
	return ""
}

// tlsClientProfileName selects the tls-client profile for VK auth/captcha.
// Override via -tls-profile flag or VK_TURN_TLS_PROFILE env (env wins).
var tlsClientProfileName string

// defaultTLSProfile is the compile-time default profile. It can be overridden at
// build time via -ldflags "-X github.com/l1ch666/vk-turn-proxy/v2/client.defaultTLSProfile=mesh_android"
// to produce per-profile builds without code edits.
var defaultTLSProfile = "confirmed_android_2"

func selectedTLSProfileName() string {
	name := strings.ToLower(strings.TrimSpace(os.Getenv("VK_TURN_TLS_PROFILE")))
	if name == "" {
		name = strings.ToLower(strings.TrimSpace(tlsClientProfileName))
	}
	if name == "" {
		return strings.ToLower(strings.TrimSpace(defaultTLSProfile))
	}
	return name
}

// selectTLSProfile maps the configured name to a tls-client ClientProfile.
// Default is an Android (Chromium) fingerprint, matching the Android WebView
// that VK accepts — desktop Chrome JA3 is the likely BOT discriminator.
func selectTLSProfile() profiles.ClientProfile {
	switch selectedTLSProfileName() {
	case "chrome_146", "chrome146", "chrome":
		return profiles.Chrome_146
	case "chrome_133":
		return profiles.Chrome_133
	case "confirmed_android", "android":
		return profiles.ConfirmedAndroid
	case "confirmed_android_2", "confirmed_android2", "android2":
		return profiles.ConfirmedAndroid2
	case "mesh_android":
		return profiles.MeshAndroid
	case "mesh_android_2", "mesh_android2":
		return profiles.MeshAndroid2
	case "okhttp4_android_13", "okhttp":
		return profiles.Okhttp4Android13
	default:
		return profiles.ConfirmedAndroid2
	}
}

// generateCaptchaDebugInfo derives a per-session debug_info value. A real
// VK-accepted request carries a 64-hex (32-byte) value, and the old static
// tool-shared constant is now rejected — so emit a fresh 64-hex value per
// session (matching the captured length/format).
func generateCaptchaDebugInfo(profile Profile) string {
	var raw [32]byte
	if _, err := cryptorand.Read(raw[:]); err != nil {
		sum := sha256.Sum256([]byte(profile.UserAgent + strconv.FormatInt(time.Now().UnixNano(), 10)))
		return hex.EncodeToString(sum[:])
	}
	return hex.EncodeToString(raw[:])
}

func callCaptchaNotRobot(ctx context.Context, sessionToken, hash string, streamID int, client tlsclient.HttpClient, profile Profile) (string, error) {
	vkReq := func(method string, postData string) (map[string]interface{}, error) {
		reqURL := captchaAPIBaseURL + method + "?v=5.131"
		parsedURL, err := neturl.Parse(reqURL)
		if err != nil {
			return nil, fmt.Errorf("parse request URL: %w", err)
		}
		domain := parsedURL.Hostname()

		dumpCaptchaRequest(streamID, method, postData)
		req, err := fhttp.NewRequestWithContext(ctx, "POST", reqURL, strings.NewReader(postData))
		if err != nil {
			return nil, err
		}

		req.Host = domain
		applyCaptchaAPIHeaders(req, profile)

		httpResp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer func(Body io.ReadCloser) {
			_ = Body.Close()
		}(httpResp.Body)

		body, err := readResponseBodyLimited(
			httpResp.Body,
			maxCaptchaAPIResponseBytes,
			"captcha API response",
		)
		if err != nil {
			return nil, err
		}
		var resp map[string]interface{}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, err
		}
		dumpCaptchaResponse(streamID, method, resp)
		return resp, nil
	}

	baseParams := fmt.Sprintf("session_token=%s&domain=vk.com&adFp=%s&access_token=", neturl.QueryEscape(sessionToken), neturl.QueryEscape(generateAdFp()))

	log.Printf("[STREAM %d] [Captcha] Step 1/4: settings", streamID)
	if _, err := vkReq("captchaNotRobot.settings", baseParams); err != nil {
		return "", fmt.Errorf("settings failed: %w", err)
	}

	time.Sleep(200 * time.Millisecond)

	log.Printf("[STREAM %d] [Captcha] Step 2/4: componentDone", streamID)
	browserFp := generateBrowserFp(profile)
	deviceJSON := buildCaptchaDeviceJSON(profile)
	// Real WebView sends an empty browser_fp at componentDone (populated only at check).
	componentDoneData := baseParams + fmt.Sprintf("&browser_fp=&device=%s", neturl.QueryEscape(deviceJSON))

	if _, err := vkReq("captchaNotRobot.componentDone", componentDoneData); err != nil {
		return "", fmt.Errorf("componentDone failed: %w", err)
	}

	time.Sleep(200 * time.Millisecond)

	log.Printf("[STREAM %d] [Captcha] Step 3/4: check", streamID)
	answer := base64.StdEncoding.EncodeToString([]byte("{}"))

	debugInfo := generateCaptchaDebugInfo(profile)

	connectionRtt := captchaConnectionRtt
	connectionDownlink := captchaConnectionDownlink

	// Captured VK-accepted check sends all motion sensors, cursor and taps empty;
	// only connectionDownlink is populated. Replicate exactly.
	checkData := baseParams + fmt.Sprintf(
		"&accelerometer=%s&gyroscope=%s&motion=%s&cursor=%s&taps=%s&connectionRtt=%s&connectionDownlink=%s&browser_fp=%s&hash=%s&answer=%s&debug_info=%s",
		neturl.QueryEscape("[]"), neturl.QueryEscape("[]"), neturl.QueryEscape("[]"),
		neturl.QueryEscape("[]"), neturl.QueryEscape("[]"), neturl.QueryEscape(connectionRtt),
		neturl.QueryEscape(connectionDownlink),
		browserFp, hash, answer, debugInfo,
	)

	checkResp, err := vkReq("captchaNotRobot.check", checkData)
	if err != nil {
		return "", fmt.Errorf("check failed: %w", err)
	}

	respObj, ok := checkResp["response"].(map[string]interface{})
	if !ok {
		return "", responseShapeError("invalid check response", checkResp)
	}
	status, ok := respObj["status"].(string)
	if !ok || status != "OK" {
		return "", fmt.Errorf("check status: %s", status)
	}
	successToken, ok := respObj["success_token"].(string)
	if !ok || successToken == "" {
		return "", fmt.Errorf("success_token not found")
	}

	time.Sleep(200 * time.Millisecond)

	log.Printf("[STREAM %d] [Captcha] Step 4/4: endSession", streamID)
	_, err = vkReq("captchaNotRobot.endSession", baseParams)
	if err != nil {
		log.Printf("[STREAM %d] [Captcha] Warning: endSession failed: %v", streamID, err)
	}

	return successToken, nil
}

// endregion

// region VK Credentials Layer

type VKCredentials struct {
	ClientID     string
	ClientSecret string
}

var vkCredentialsList = []VKCredentials{
	{ClientID: "6287487", ClientSecret: "QbYic1K3lEV5kTGiqlq2"},  // VK_WEB_APP_ID
	{ClientID: "7879029", ClientSecret: "aR5NKGmm03GYrCiNKsaw"},  // VK_MVK_APP_ID
	{ClientID: "52461373", ClientSecret: "o557NLIkAErNhakXrQ7A"}, // VK_WEB_VKVIDEO_APP_ID
	{ClientID: "52649896", ClientSecret: "WStp4ihWG4l3nmXZgIbC"}, // VK_MVK_VKVIDEO_APP_ID
	{ClientID: "51781872", ClientSecret: "IjjCNl4L4Tf5QZEXIHKK"}, // VK_ID_AUTH_APP
}

type TurnCredentials struct {
	Username   string
	Password   string
	ServerAddr string
	ExpiresAt  time.Time
	Link       string
}

type StreamCredentialsCache struct {
	creds         TurnCredentials
	mutex         sync.RWMutex
	errorCount    atomic.Int32
	lastErrorTime atomic.Int64
}

const (
	credentialLifetime     = 10 * time.Minute
	cacheSafetyMargin      = 60 * time.Second
	maxCacheErrors         = 3
	errorWindow            = 10 * time.Second
	defaultStreamsPerCache = 10
)

var streamsPerCache = defaultStreamsPerCache

func getCacheID(streamID int) int {
	return streamID / streamsPerCache
}

func vkDelayRandom(minMs, maxMs int) {
	ms := minMs + rand.Intn(maxMs-minMs+1)
	time.Sleep(time.Duration(ms) * time.Millisecond)
}

var credentialsStore = struct {
	mu     sync.RWMutex
	caches map[int]*StreamCredentialsCache
}{
	caches: make(map[int]*StreamCredentialsCache),
}

func getStreamCache(streamID int) *StreamCredentialsCache {
	cacheID := getCacheID(streamID)

	credentialsStore.mu.RLock()
	cache, exists := credentialsStore.caches[cacheID]
	credentialsStore.mu.RUnlock()

	if exists {
		return cache
	}

	credentialsStore.mu.Lock()
	defer credentialsStore.mu.Unlock()

	if cache, exists = credentialsStore.caches[cacheID]; exists {
		return cache
	}

	cache = &StreamCredentialsCache{}
	credentialsStore.caches[cacheID] = cache
	return cache
}

func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "401") ||
		strings.Contains(errStr, "unauthorized") ||
		strings.Contains(errStr, "authentication") ||
		strings.Contains(errStr, "invalid credential") ||
		strings.Contains(errStr, "stale nonce")
}

func isFatalCaptchaError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "FATAL_CAPTCHA")
}

func recordTURNAllocationResult(streamID int, err error) {
	if err != nil {
		if isAuthError(err) {
			metrics.Process.AuthFailed()
			handleAuthError(streamID)
		}
		return
	}

	cache := getStreamCache(streamID)
	cache.errorCount.Store(0)
	cache.lastErrorTime.Store(0)
}

func handleAuthError(streamID int) bool {
	cache := getStreamCache(streamID)
	cacheID := getCacheID(streamID)

	now := time.Now().Unix()

	if now-cache.lastErrorTime.Load() > int64(errorWindow.Seconds()) {
		cache.errorCount.Store(0)
	}

	count := cache.errorCount.Add(1)
	cache.lastErrorTime.Store(now)

	log.Printf("[STREAM %d] Auth error (cache=%d, count=%d/%d)", streamID, cacheID, count, maxCacheErrors)

	if count >= maxCacheErrors {
		log.Printf("[VK Auth] Multiple auth errors detected (%d), invalidating cache %d for stream %d...", count, cacheID, streamID)
		cache.invalidate(streamID)
		return true
	}
	return false
}

func (c *StreamCredentialsCache) invalidate(streamID int) {
	c.mutex.Lock()
	c.creds = TurnCredentials{}
	c.mutex.Unlock()

	c.errorCount.Store(0)
	c.lastErrorTime.Store(0)

	log.Printf("[STREAM %d] [VK Auth] Credentials cache invalidated", streamID)
}

func getVkCredsCached(ctx context.Context, link string, streamID int, dialer *dnsdialer.Dialer) (string, string, string, error) {
	cache := getStreamCache(streamID)
	cacheID := getCacheID(streamID)

	cache.mutex.RLock()
	if cache.creds.Link == link && time.Now().Before(cache.creds.ExpiresAt) {
		expires := time.Until(cache.creds.ExpiresAt)
		u, p, a := cache.creds.Username, cache.creds.Password, cache.creds.ServerAddr
		cache.mutex.RUnlock()
		if isDebug {
			log.Printf("[STREAM %d] [VK Auth] Using cached credentials (cache=%d, expires in %v)", streamID, cacheID, expires)
		}
		return u, p, a, nil
	}
	cache.mutex.RUnlock()

	cache.mutex.Lock()
	defer cache.mutex.Unlock()

	// Double-check inside lock
	if cache.creds.Link == link && time.Now().Before(cache.creds.ExpiresAt) {
		return cache.creds.Username, cache.creds.Password, cache.creds.ServerAddr, nil
	}

	user, pass, addr, err := fetchVkCredsSerialized(ctx, link, streamID, dialer)
	if err != nil {
		return "", "", "", err
	}

	cache.creds = TurnCredentials{Username: user, Password: pass, ServerAddr: addr, ExpiresAt: time.Now().Add(credentialLifetime - cacheSafetyMargin), Link: link}
	return user, pass, addr, nil
}

var (
	vkRequestMu           sync.Mutex
	globalLastVkFetchTime time.Time
)

func fetchVkCredsSerialized(ctx context.Context, link string, streamID int, dialer *dnsdialer.Dialer) (string, string, string, error) {
	vkRequestMu.Lock()
	defer vkRequestMu.Unlock()

	// Ensure a minimum cooldown between credential requests to avoid VK rate limits
	minInterval := 3*time.Second + time.Duration(rand.Intn(3000))*time.Millisecond
	elapsed := time.Since(globalLastVkFetchTime)

	if !globalLastVkFetchTime.IsZero() && elapsed < minInterval {
		wait := minInterval - elapsed
		log.Printf("[STREAM %d] [VK Auth] Throttling: waiting %v to prevent rate limit...", streamID, wait.Truncate(time.Millisecond))
		select {
		case <-ctx.Done():
			return "", "", "", ctx.Err()
		case <-time.After(wait):
		}
	}

	defer func() {
		globalLastVkFetchTime = time.Now()
	}()

	return fetchVkCreds(ctx, link, streamID, dialer)
}

func fetchVkCreds(ctx context.Context, link string, streamID int, dialer *dnsdialer.Dialer) (string, string, string, error) {
	// Check Global Lockout to prevent API bans
	if time.Now().Unix() < globalCaptchaLockout.Load() {
		return "", "", "", fmt.Errorf("CAPTCHA_WAIT_REQUIRED: global lockout active")
	}

	var lastErr error
	jar := tlsclient.NewCookieJar()

	for _, creds := range vkCredentialsList {
		log.Printf("[STREAM %d] [VK Auth] Trying credentials: client_id=%s", streamID, creds.ClientID)

		user, pass, addr, err := getTokenChain(ctx, link, streamID, creds, dialer, jar)

		if err == nil {
			log.Printf("[STREAM %d] [VK Auth] Success with client_id=%s", streamID, creds.ClientID)
			return user, pass, addr, nil
		}

		lastErr = err
		log.Printf("[STREAM %d] [VK Auth] Failed with client_id=%s: %v", streamID, creds.ClientID, err)

		// Hard abort on captcha/fatal conditions instead of trying next creds
		if strings.Contains(err.Error(), "CAPTCHA_WAIT_REQUIRED") || isFatalCaptchaError(err) {
			return "", "", "", err
		}

		if strings.Contains(err.Error(), "error_code:29") || strings.Contains(err.Error(), "error_code: 29") || strings.Contains(err.Error(), "Rate limit") {
			log.Printf("[STREAM %d] [VK Auth] Rate limit detected, trying next credentials...", streamID)
		}
	}

	return "", "", "", fmt.Errorf("all VK credentials failed: %w", lastErr)
}

func getTokenChain(ctx context.Context, link string, streamID int, creds VKCredentials, dialer *dnsdialer.Dialer, jar tlsclient.CookieJar) (string, string, string, error) {
	// Present a coherent Android Chrome identity: the captcha that VK accepts is
	// solved in an Android WebView (Chromium), so a desktop Windows UA + desktop
	// Chrome TLS fingerprint is an inconsistency the anti-bot can flag. UA,
	// client hints and TLS profile (below) are all mobile/Android now.
	profile := Profile{
		UserAgent:       "Mozilla/5.0 (Linux; Android 14; Pixel 7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Mobile Safari/537.36",
		SecChUa:         `"Not(A:Brand";v="99", "Google Chrome";v="146", "Chromium";v="146"`,
		SecChUaMobile:   "?1",
		SecChUaPlatform: `"Android"`,
	}

	tlsProfile := selectTLSProfile()
	client, err := tlsclient.NewHttpClient(tlsclient.NewNoopLogger(),
		tlsclient.WithTimeoutSeconds(20),
		tlsclient.WithClientProfile(tlsProfile),
		tlsclient.WithCookieJar(jar),
		tlsclient.WithDialContext(dialer.DialContext),
	)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to initialize tls_client: %w", err)
	}
	log.Printf("[STREAM %d] [VK Auth] TLS profile: %s", streamID, selectedTLSProfileName())

	name := generateName()
	escapedName := neturl.QueryEscape(name)

	log.Printf("[STREAM %d] [VK Auth] Connecting Identity - Name: %s | User-Agent: %s", streamID, name, profile.UserAgent)

	doRequest := func(data string, url string) (resp map[string]interface{}, err error) {
		parsedURL, err := neturl.Parse(url)
		if err != nil {
			return nil, fmt.Errorf("parse request URL: %w", err)
		}
		domain := parsedURL.Hostname()

		req, err := fhttp.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer([]byte(data)))
		if err != nil {
			return nil, err
		}

		req.Host = domain
		applyBrowserProfileFhttp(req, profile)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "*/*")
		req.Header.Set("Origin", "https://vk.ru")
		req.Header.Set("Referer", "https://vk.ru/")
		req.Header.Set("Sec-Fetch-Site", "same-site")
		req.Header.Set("Sec-Fetch-Mode", "cors")
		req.Header.Set("Sec-Fetch-Dest", "empty")
		req.Header.Set("Priority", "u=1, i")

		httpResp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer func() {
			if closeErr := httpResp.Body.Close(); closeErr != nil {
				log.Printf("close response body: %s", closeErr)
			}
		}()

		body, err := readResponseBodyLimited(
			httpResp.Body,
			maxVKAPIResponseBytes,
			"VK API response",
		)
		if err != nil {
			return nil, err
		}

		err = json.Unmarshal(body, &resp)
		if err != nil {
			return nil, err
		}
		return resp, nil
	}

	// Token 1
	data := fmt.Sprintf("client_id=%s&token_type=messages&client_secret=%s&version=1&app_id=%s", creds.ClientID, creds.ClientSecret, creds.ClientID)
	resp, err := doRequest(data, "https://login.vk.ru/?act=get_anonym_token")
	if err != nil {
		return "", "", "", err
	}
	dataMap, ok := resp["data"].(map[string]interface{})
	if !ok {
		return "", "", "", responseShapeError("unexpected anon token response", resp)
	}
	token1, ok := dataMap["access_token"].(string)
	if !ok {
		return "", "", "", responseShapeError("missing access_token in response", resp)
	}

	vkDelayRandom(100, 150)

	// Token 1 -> getCallPreview
	data = fmt.Sprintf("vk_join_link=https://vk.com/call/join/%s&fields=photo_200&access_token=%s", link, token1)
	_, err = doRequest(data, "https://api.vk.ru/method/calls.getCallPreview?v=5.275&client_id="+creds.ClientID)
	if err != nil {
		log.Printf("[STREAM %d] [VK Auth] Warning: getCallPreview failed: %v", streamID, err)
	}

	vkDelayRandom(200, 400)

	// Token 2
	data = fmt.Sprintf("vk_join_link=https://vk.com/call/join/%s&name=%s&access_token=%s", link, escapedName, token1)
	urlAddr := fmt.Sprintf("https://api.vk.ru/method/calls.getAnonymousToken?v=5.275&client_id=%s", creds.ClientID)

	var token2 string
	for attempt := 0; ; attempt++ {
		resp, err = doRequest(data, urlAddr)
		if err != nil {
			return "", "", "", err
		}

		if errObj, hasErr := resp["error"].(map[string]interface{}); hasErr {
			captchaErr := ParseVkCaptchaError(errObj)
			if captchaErr != nil && captchaErr.IsCaptchaError() {
				log.Printf("[STREAM %d] [Captcha] VK captcha received (redirect: %t, image: %t, sid: %t)", streamID, captchaErr.RedirectURI != "", captchaErr.CaptchaImg != "", captchaErr.CaptchaSid != "")
				solveMode, hasSolveMode := captchaSolveModeForAttempt(attempt, manualCaptcha, autoCaptchaSliderPOC)
				if !hasSolveMode {
					log.Printf("[STREAM %d] [Captcha] No more solve modes available (attempt %d)", streamID, attempt+1)

					// Engage global lockout to protect API
					globalCaptchaLockout.Store(time.Now().Add(60 * time.Second).Unix())

					if connectedStreams.Load() == 0 {
						log.Printf("[STREAM %d] [FATAL] 0 connected streams and captcha solve modes exhausted.", streamID)
						return "", "", "", fmt.Errorf("FATAL_CAPTCHA_FAILED_NO_STREAMS")
					}

					return "", "", "", fmt.Errorf("CAPTCHA_WAIT_REQUIRED")
				}

				var successToken string
				var captchaKey string
				var solveErr error

				switch solveMode {
				case captchaSolveModeAuto:
					if captchaErr.SessionToken != "" && captchaErr.RedirectURI != "" {
						successToken, solveErr = solveVkCaptcha(ctx, captchaErr, streamID, client, profile, false)
						if solveErr != nil {
							log.Printf("[STREAM %d] [Captcha] Auto captcha failed: %v", streamID, solveErr)
						}
					} else {
						solveErr = fmt.Errorf("missing fields for auto solve")
					}
				case captchaSolveModeSliderPOC:
					if captchaErr.SessionToken != "" && captchaErr.RedirectURI != "" {
						successToken, solveErr = solveVkCaptcha(ctx, captchaErr, streamID, client, profile, true)
						if solveErr != nil {
							log.Printf("[STREAM %d] [Captcha] Auto captcha slider POC failed: %v", streamID, solveErr)
						}
					} else {
						solveErr = fmt.Errorf("missing fields for slider POC auto solve")
					}
				case captchaSolveModeManual:
					log.Printf("[STREAM %d] [Captcha] Triggering manual captcha fallback...", streamID)
					manualCtx, manualCancel := context.WithTimeout(ctx, 60*time.Second)
					if captchaErr.RedirectURI != "" {
						successToken, solveErr = solveCaptchaViaProxy(manualCtx, captchaErr.RedirectURI, dialer)
					} else if captchaErr.CaptchaImg != "" {
						captchaKey, solveErr = solveCaptchaViaHTTP(manualCtx, captchaErr.CaptchaImg)
					} else {
						solveErr = fmt.Errorf("no redirect_uri or captcha_img")
					}
					if manualCtx.Err() == context.DeadlineExceeded {
						solveErr = fmt.Errorf("manual captcha timed out after 60s")
					}
					manualCancel()
				}

				// If solving failed (auto or manual) or timed out
				if solveErr != nil {
					log.Printf("[STREAM %d] [Captcha] %s failed (attempt %d): %v", streamID, captchaSolveModeLabel(solveMode), attempt+1, solveErr)

					nextSolveMode, hasNextSolveMode := captchaSolveModeForAttempt(attempt+1, manualCaptcha, autoCaptchaSliderPOC)
					if hasNextSolveMode {
						log.Printf("[STREAM %d] [Captcha] Falling back to %s...", streamID, captchaSolveModeLabel(nextSolveMode))
						continue
					}

					// Engage global lockout to protect API
					globalCaptchaLockout.Store(time.Now().Add(60 * time.Second).Unix())

					// If we have 0 streams alive, this is fatal
					if connectedStreams.Load() == 0 {
						log.Printf("[STREAM %d] [FATAL] 0 connected streams and manual captcha failed/timed out.", streamID)
						return "", "", "", fmt.Errorf("FATAL_CAPTCHA_FAILED_NO_STREAMS")
					}

					return "", "", "", fmt.Errorf("CAPTCHA_WAIT_REQUIRED")
				}

				if captchaErr.CaptchaAttempt == "0" || captchaErr.CaptchaAttempt == "" {
					captchaErr.CaptchaAttempt = "1"
				}

				if captchaKey != "" {
					data = fmt.Sprintf("vk_join_link=https://vk.com/call/join/%s&name=%s&captcha_key=%s&captcha_sid=%s&access_token=%s",
						link, escapedName, neturl.QueryEscape(captchaKey), captchaErr.CaptchaSid, token1)
				} else {
					data = fmt.Sprintf("vk_join_link=https://vk.com/call/join/%s&name=%s&captcha_key=&captcha_sid=%s&is_sound_captcha=0&success_token=%s&captcha_ts=%s&captcha_attempt=%s&access_token=%s",
						link, escapedName, captchaErr.CaptchaSid, neturl.QueryEscape(successToken), captchaErr.CaptchaTs, captchaErr.CaptchaAttempt, token1)
				}
				continue
			}
			return "", "", "", fmt.Errorf("VK API error: %v", errObj)
		}

		respMap, okLoop := resp["response"].(map[string]interface{})
		if !okLoop {
			return "", "", "", responseShapeError("unexpected getAnonymousToken response", resp)
		}
		token2, okLoop = respMap["token"].(string)
		if !okLoop {
			return "", "", "", responseShapeError("missing token in response", resp)
		}
		break
	}

	vkDelayRandom(100, 150)

	// Token 3
	sessionData := fmt.Sprintf(`{"version":2,"device_id":"%s","client_version":1.1,"client_type":"SDK_JS"}`, uuid.New())
	data = fmt.Sprintf("session_data=%s&method=auth.anonymLogin&format=JSON&application_key=CGMMEJLGDIHBABABA", neturl.QueryEscape(sessionData))
	resp, err = doRequest(data, "https://calls.okcdn.ru/fb.do")
	if err != nil {
		return "", "", "", err
	}
	token3, ok := resp["session_key"].(string)
	if !ok {
		return "", "", "", responseShapeError("missing session_key in response", resp)
	}

	vkDelayRandom(100, 150)

	// Token 4 -> TURN Creds
	data = fmt.Sprintf("joinLink=%s&isVideo=false&protocolVersion=5&capabilities=2F7F&anonymToken=%s&method=vchat.joinConversationByLink&format=JSON&application_key=CGMMEJLGDIHBABABA&session_key=%s", link, token2, token3)
	resp, err = doRequest(data, "https://calls.okcdn.ru/fb.do")
	if err != nil {
		return "", "", "", err
	}

	tsRaw, ok := resp["turn_server"].(map[string]interface{})
	if !ok {
		return "", "", "", responseShapeError("missing turn_server in response", resp)
	}
	user, ok := tsRaw["username"].(string)
	if !ok {
		return "", "", "", fmt.Errorf("missing username in turn_server")
	}
	pass, ok := tsRaw["credential"].(string)
	if !ok {
		return "", "", "", fmt.Errorf("missing credential in turn_server")
	}
	urlsRaw, ok := tsRaw["urls"].([]interface{})
	if !ok || len(urlsRaw) == 0 {
		return "", "", "", fmt.Errorf("missing or empty urls in turn_server")
	}
	urlStr, ok := urlsRaw[0].(string)
	if !ok {
		return "", "", "", fmt.Errorf("turn server url is not a string")
	}

	clean := strings.Split(urlStr, "?")[0]
	address := strings.TrimPrefix(strings.TrimPrefix(clean, "turn:"), "turns:")

	return user, pass, address, nil
}

// endregion

func getYandexCreds(ctx context.Context, link string) (string, string, string, error) {
	const telemostConfHost = "cloud-api.yandex.ru"
	telemostConfPath := fmt.Sprintf("%s%s%s", "/telemost_front/v2/telemost/conferences/https%3A%2F%2Ftelemost.yandex.ru%2Fj%2F", link, "/connection?next_gen_media_platform_allowed=false")

	profile := getRandomProfile()
	name := generateName()

	type ConferenceResponse struct {
		URI                 string `json:"uri"`
		RoomID              string `json:"room_id"`
		PeerID              string `json:"peer_id"`
		ClientConfiguration struct {
			MediaServerURL string `json:"media_server_url"`
		} `json:"client_configuration"`
		Credentials string `json:"credentials"`
	}

	type PartMeta struct {
		Name        string `json:"name"`
		Role        string `json:"role"`
		Description string `json:"description"`
		SendAudio   bool   `json:"sendAudio"`
		SendVideo   bool   `json:"sendVideo"`
	}

	type PartAttrs struct {
		Name        string `json:"name"`
		Role        string `json:"role"`
		Description string `json:"description"`
	}

	type SdkInfo struct {
		Implementation string `json:"implementation"`
		Version        string `json:"version"`
		UserAgent      string `json:"userAgent"`
		HwConcurrency  int    `json:"hwConcurrency"`
	}

	type Capabilities struct {
		OfferAnswerMode             []string `json:"offerAnswerMode"`
		InitialSubscriberOffer      []string `json:"initialSubscriberOffer"`
		SlotsMode                   []string `json:"slotsMode"`
		SimulcastMode               []string `json:"simulcastMode"`
		SelfVadStatus               []string `json:"selfVadStatus"`
		DataChannelSharing          []string `json:"dataChannelSharing"`
		VideoEncoderConfig          []string `json:"videoEncoderConfig"`
		DataChannelVideoCodec       []string `json:"dataChannelVideoCodec"`
		BandwidthLimitationReason   []string `json:"bandwidthLimitationReason"`
		SdkDefaultDeviceManagement  []string `json:"sdkDefaultDeviceManagement"`
		JoinOrderLayout             []string `json:"joinOrderLayout"`
		PinLayout                   []string `json:"pinLayout"`
		SendSelfViewVideoSlot       []string `json:"sendSelfViewVideoSlot"`
		ServerLayoutTransition      []string `json:"serverLayoutTransition"`
		SdkPublisherOptimizeBitrate []string `json:"sdkPublisherOptimizeBitrate"`
		SdkNetworkLostDetection     []string `json:"sdkNetworkLostDetection"`
		SdkNetworkPathMonitor       []string `json:"sdkNetworkPathMonitor"`
		PublisherVp9                []string `json:"publisherVp9"`
		SvcMode                     []string `json:"svcMode"`
		SubscriberOfferAsyncAck     []string `json:"subscriberOfferAsyncAck"`
		SvcModes                    []string `json:"svcModes"`
		ReportTelemetryModes        []string `json:"reportTelemetryModes"`
		KeepDefaultDevicesModes     []string `json:"keepDefaultDevicesModes"`
	}

	type HelloPayload struct {
		ParticipantMeta        PartMeta     `json:"participantMeta"`
		ParticipantAttributes  PartAttrs    `json:"participantAttributes"`
		SendAudio              bool         `json:"sendAudio"`
		SendVideo              bool         `json:"sendVideo"`
		SendSharing            bool         `json:"sendSharing"`
		ParticipantID          string       `json:"participantId"`
		RoomID                 string       `json:"roomId"`
		ServiceName            string       `json:"serviceName"`
		Credentials            string       `json:"credentials"`
		CapabilitiesOffer      Capabilities `json:"capabilitiesOffer"`
		SdkInfo                SdkInfo      `json:"sdkInfo"`
		SdkInitializationID    string       `json:"sdkInitializationId"`
		DisablePublisher       bool         `json:"disablePublisher"`
		DisableSubscriber      bool         `json:"disableSubscriber"`
		DisableSubscriberAudio bool         `json:"disableSubscriberAudio"`
	}

	type HelloRequest struct {
		UID   string       `json:"uid"`
		Hello HelloPayload `json:"hello"`
	}

	type FlexUrls []string

	type WSSResponse struct {
		UID         string `json:"uid"`
		ServerHello struct {
			RtcConfiguration struct {
				IceServers []struct {
					Urls       FlexUrls `json:"urls"`
					Username   string   `json:"username,omitempty"`
					Credential string   `json:"credential,omitempty"`
				} `json:"iceServers"`
			} `json:"rtcConfiguration"`
		} `json:"serverHello"`
	}

	type WSSAck struct {
		UID string `json:"uid"`
		Ack struct {
			Status struct {
				Code string `json:"code"`
			} `json:"status"`
		} `json:"ack"`
	}

	type WSSData struct {
		ParticipantID string
		RoomID        string
		Credentials   string
		Wss           string
	}

	endpoint := "https://" + telemostConfHost + telemostConfPath
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	}
	client := &http.Client{
		Timeout:   20 * time.Second,
		Transport: tr,
	}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", "", "", err
	}

	applyBrowserProfile(req, profile)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Referer", "https://telemost.yandex.ru/")
	req.Header.Set("Origin", "https://telemost.yandex.ru")
	req.Header.Set("Client-Instance-Id", uuid.New().String())

	resp, err := client.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			log.Printf("close response body: %s", closeErr)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<10))
		return "", "", "", fmt.Errorf("GetConference: status=%s", resp.Status)
	}

	body, err := readResponseBodyLimited(
		resp.Body,
		maxConferenceResponseBytes,
		"conference response",
	)
	if err != nil {
		return "", "", "", err
	}
	var result ConferenceResponse
	if err = json.Unmarshal(body, &result); err != nil {
		return "", "", "", fmt.Errorf("decode conf: %v", err)
	}
	data := WSSData{
		ParticipantID: result.PeerID,
		RoomID:        result.RoomID,
		Credentials:   result.Credentials,
		Wss:           result.ClientConfiguration.MediaServerURL,
	}
	h := http.Header{}
	h.Set("Origin", "https://telemost.yandex.ru")
	h.Set("User-Agent", profile.UserAgent)

	websocketCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	dialer := websocket.Dialer{}
	var conn *websocket.Conn
	conn, resp, err = dialer.DialContext(websocketCtx, data.Wss, h)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return "", "", "", fmt.Errorf("ws dial: %w", err)
	}
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			log.Printf("close websocket: %s", closeErr)
		}
	}()

	req1 := HelloRequest{
		UID: uuid.New().String(),
		Hello: HelloPayload{
			ParticipantMeta: PartMeta{
				Name:        name,
				Role:        "SPEAKER",
				Description: "",
				SendAudio:   false,
				SendVideo:   false,
			},
			ParticipantAttributes: PartAttrs{
				Name:        name,
				Role:        "SPEAKER",
				Description: "",
			},
			SendAudio:   false,
			SendVideo:   false,
			SendSharing: false,

			ParticipantID: data.ParticipantID,
			RoomID:        data.RoomID,
			ServiceName:   "telemost",
			Credentials:   data.Credentials,
			SdkInfo: SdkInfo{
				Implementation: "browser",
				Version:        "5.15.0",
				UserAgent:      profile.UserAgent,
				HwConcurrency:  4,
			},
			SdkInitializationID:    uuid.New().String(),
			DisablePublisher:       false,
			DisableSubscriber:      false,
			DisableSubscriberAudio: false,
			CapabilitiesOffer: Capabilities{
				OfferAnswerMode:             []string{"SEPARATE"},
				InitialSubscriberOffer:      []string{"ON_HELLO"},
				SlotsMode:                   []string{"FROM_CONTROLLER"},
				SimulcastMode:               []string{"DISABLED"},
				SelfVadStatus:               []string{"FROM_SERVER"},
				DataChannelSharing:          []string{"TO_RTP"},
				VideoEncoderConfig:          []string{"NO_CONFIG"},
				DataChannelVideoCodec:       []string{"VP8"},
				BandwidthLimitationReason:   []string{"BANDWIDTH_REASON_DISABLED"},
				SdkDefaultDeviceManagement:  []string{"SDK_DEFAULT_DEVICE_MANAGEMENT_DISABLED"},
				JoinOrderLayout:             []string{"JOIN_ORDER_LAYOUT_DISABLED"},
				PinLayout:                   []string{"PIN_LAYOUT_DISABLED"},
				SendSelfViewVideoSlot:       []string{"SEND_SELF_VIEW_VIDEO_SLOT_DISABLED"},
				ServerLayoutTransition:      []string{"SERVER_LAYOUT_TRANSITION_DISABLED"},
				SdkPublisherOptimizeBitrate: []string{"SDK_PUBLISHER_OPTIMIZE_BITRATE_DISABLED"},
				SdkNetworkLostDetection:     []string{"SDK_NETWORK_LOST_DETECTION_DISABLED"},
				SdkNetworkPathMonitor:       []string{"SDK_NETWORK_PATH_MONITOR_DISABLED"},
				PublisherVp9:                []string{"PUBLISH_VP9_DISABLED"},
				SvcMode:                     []string{"SVC_MODE_DISABLED"},
				SubscriberOfferAsyncAck:     []string{"SUBSCRIBER_OFFER_ASYNC_ACK_DISABLED"},
				SvcModes:                    []string{"FALSE"},
				ReportTelemetryModes:        []string{"TRUE"},
				KeepDefaultDevicesModes:     []string{"TRUE"},
			},
		},
	}

	if isDebug {
		log.Printf("Sending WSS HELLO (credentials and room identifiers redacted)")
	}

	if err := conn.WriteJSON(req1); err != nil {
		return "", "", "", fmt.Errorf("ws write: %w", err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
		return "", "", "", fmt.Errorf("ws set read deadline: %w", err)
	}

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return "", "", "", fmt.Errorf("ws read: %w", err)
		}
		if isDebug {
			log.Printf("WSS recv: %d bytes (payload redacted)", len(msg))
		}

		var ack WSSAck
		if err := json.Unmarshal(msg, &ack); err == nil && ack.Ack.Status.Code != "" {
			continue
		}

		var resp WSSResponse
		if err := json.Unmarshal(msg, &resp); err == nil {
			ice := resp.ServerHello.RtcConfiguration.IceServers
			for _, s := range ice {
				for _, u := range s.Urls {
					if !strings.HasPrefix(u, "turn:") && !strings.HasPrefix(u, "turns:") {
						continue
					}
					if strings.Contains(u, "transport=tcp") {
						continue
					}
					clean := strings.Split(u, "?")[0]
					address := strings.TrimPrefix(strings.TrimPrefix(clean, "turn:"), "turns:")

					return s.Username, s.Credential, address, nil
				}
			}
		}
	}
}

func dtlsFunc(
	ctx context.Context,
	conn net.PacketConn,
	peer *net.UDPAddr,
	authentication dtlsauth.ClientAuthentication,
	clientAuthToken *sessionauth.Token,
) (net.Conn, error) {
	certificate, err := selfsign.GenerateSelfSigned()
	if err != nil {
		return nil, err
	}
	options, err := clientDTLSOptions(certificate, authentication)
	if err != nil {
		return nil, err
	}

	select {
	case handshakeSem <- struct{}{}:
		defer func() { <-handshakeSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	ctx1, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	dtlsConn, err := dtls.ClientWithOptions(
		conn,
		peer,
		options...,
	)
	if err != nil {
		return nil, err
	}

	if err := dtlsConn.HandshakeContext(ctx1); err != nil {
		_ = dtlsConn.Close()
		return nil, err
	}
	if clientAuthToken != nil {
		if err := sessionauth.Authenticate(ctx1, dtlsConn, *clientAuthToken); err != nil {
			_ = dtlsConn.Close()
			return nil, fmt.Errorf("authenticate client: %w", err)
		}
	}
	return dtlsConn, nil
}

func oneDtlsConnection(ctx context.Context, peer *net.UDPAddr, authentication dtlsauth.ClientAuthentication, clientAuthToken *sessionauth.Token, listenConn net.PacketConn, localPeer *localPeerPin, inboundChan <-chan *UDPPacket, connchan chan<- net.PacketConn, okchan chan<- struct{}, streamID int) error {
	if !waitContextDelay(ctx, time.Duration(rand.Intn(400)+100)*time.Millisecond) {
		return ctx.Err()
	}

	dtlsctx, dtlscancel := context.WithCancel(ctx)
	conn1, conn2 := connutil.AsyncPacketPipe()
	defer func() { _ = conn1.Close() }()
	defer dtlscancel()
	go func() {
		for {
			select {
			case <-dtlsctx.Done():
				return
			case connchan <- conn2:
			}
		}
	}()
	dtlsConn, err1 := dtlsFunc(dtlsctx, conn1, peer, authentication, clientAuthToken)
	if err1 != nil {
		return fmt.Errorf("failed to connect DTLS: %w", err1)
	}
	defer func() {
		if closeErr := dtlsConn.Close(); closeErr != nil {
			log.Printf("[STREAM %d] failed to close DTLS connection: %s", streamID, closeErr)
		}
		log.Printf("[STREAM %d] Closed DTLS connection\n", streamID)
	}()
	log.Printf("[STREAM %d] Established DTLS connection!\n", streamID)

	if okchan != nil {
		go func() {
			select {
			case okchan <- struct{}{}:
			case <-dtlsctx.Done():
			}
		}()
	}

	wg := sync.WaitGroup{}
	wg.Add(2)
	context.AfterFunc(dtlsctx, func() {
		if err := dtlsConn.SetDeadline(time.Now()); err != nil {
			log.Printf("[STREAM %d] Warning: SetDeadline failed: %v", streamID, err)
		}
	})

	go func() {
		defer wg.Done()
		defer dtlscancel()
		for {
			if dtlsctx.Err() != nil {
				return
			}
			select {
			case <-dtlsctx.Done():
				return
			case pkt, ok := <-inboundChan:
				if !ok {
					return
				}
				packetSize := pkt.N
				n, writeErr := dtlsConn.Write(pkt.Data[:packetSize])
				packetPool.Put(pkt)
				if writeErr != nil || n != packetSize {
					if dtlsctx.Err() == nil {
						log.Printf("[STREAM %d] failed to write DTLS packet: wrote %d/%d: %v", streamID, n, packetSize, writeErr)
					}
					return
				}
			}
		}
	}()

	go func() {
		defer wg.Done()
		defer dtlscancel()
		buf := make([]byte, udpPacketBufferSize)
		for {
			n, err1 := dtlsConn.Read(buf)
			if err1 != nil {
				return
			}

			// Send back to the active WG client
			if peerAddr := localPeer.Current(); peerAddr != nil {
				if _, err := listenConn.WriteTo(buf[:n], peerAddr); err != nil {
					log.Printf("[STREAM %d] failed to forward packet to local peer: %v", streamID, err)
				}
			}
		}
	}()

	wg.Wait()
	if err := dtlsConn.SetDeadline(time.Time{}); err != nil {
		log.Printf("[STREAM %d] Failed to clear DTLS deadline: %s", streamID, err)
	}
	return nil
}

type connectedUDPConn struct {
	*net.UDPConn
}

func (c *connectedUDPConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	return c.Write(p)
}

type turnParams struct {
	host               string
	port               string
	link               string
	udp                bool
	getCreds           getCredsFunc
	dtlsAuthentication dtlsauth.ClientAuthentication
	clientAuthToken    *sessionauth.Token
}

// bridgeTURNPackets forwards packets between a long-lived DTLS packet pipe and
// one TURN allocation. Both readers must exit before this function returns: the
// packet pipe is reused by the next TURN allocation, so leaving an old reader
// behind would let it consume and drop packets after reconnect.
func bridgeTURNPackets(ctx context.Context, relayConn, pipeConn net.PacketConn, peer net.Addr, streamID int) {
	bridgeCtx, bridgeCancel := context.WithCancel(ctx)
	defer bridgeCancel()

	var internalPipeAddr atomic.Value
	var wg sync.WaitGroup
	wg.Add(2)

	deadlineDone := make(chan struct{})
	stopDeadline := context.AfterFunc(bridgeCtx, func() {
		defer close(deadlineDone)
		now := time.Now()
		if err := relayConn.SetDeadline(now); err != nil {
			log.Printf("[STREAM %d] failed to interrupt TURN relay: %s", streamID, err)
		}
		if err := pipeConn.SetReadDeadline(now); err != nil {
			log.Printf("[STREAM %d] failed to interrupt DTLS pipe reader: %s", streamID, err)
		}
	})

	go func() {
		defer wg.Done()
		defer bridgeCancel()
		buf := make([]byte, udpPacketBufferSize)
		for {
			if bridgeCtx.Err() != nil {
				return
			}
			n, addr, err := pipeConn.ReadFrom(buf)
			if err != nil || bridgeCtx.Err() != nil {
				return
			}
			if addr != nil {
				internalPipeAddr.Store(addr)
			}
			if _, err = relayConn.WriteTo(buf[:n], peer); err != nil {
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		defer bridgeCancel()
		buf := make([]byte, udpPacketBufferSize)
		for {
			if bridgeCtx.Err() != nil {
				return
			}
			n, _, err := relayConn.ReadFrom(buf)
			if err != nil || bridgeCtx.Err() != nil {
				return
			}
			addr := internalPipeAddr.Load()
			if addr == nil {
				continue
			}
			if packetAddr, ok := addr.(net.Addr); ok {
				if _, err = pipeConn.WriteTo(buf[:n], packetAddr); err != nil {
					return
				}
			}
		}
	}()

	wg.Wait()
	bridgeCancel()
	if !stopDeadline() {
		<-deadlineDone
	}
	if err := relayConn.SetDeadline(time.Time{}); err != nil {
		log.Printf("[STREAM %d] failed to clear TURN relay deadline: %s", streamID, err)
	}
	if err := pipeConn.SetReadDeadline(time.Time{}); err != nil {
		log.Printf("[STREAM %d] failed to clear DTLS pipe deadline: %s", streamID, err)
	}
}

func oneTurnConnection(ctx context.Context, turnParams *turnParams, peer *net.UDPAddr, conn2 net.PacketConn, streamID int, c chan<- error) {
	var err error
	defer func() { c <- err }()
	if !waitContextDelay(ctx, time.Duration(rand.Intn(400)+100)*time.Millisecond) {
		err = ctx.Err()
		return
	}
	user, pass, urlTarget, err1 := turnParams.getCreds(ctx, turnParams.link, streamID)
	if err1 != nil {
		err = fmt.Errorf("failed to get TURN credentials: %s", err1)
		return
	}
	urlhost, urlport, err1 := net.SplitHostPort(urlTarget)
	if err1 != nil {
		err = fmt.Errorf("failed to parse TURN server address: %s", err1)
		return
	}
	if turnParams.host != "" {
		urlhost = turnParams.host
	}
	if turnParams.port != "" {
		urlport = turnParams.port
	}
	var turnServerAddr string
	turnServerAddr = net.JoinHostPort(urlhost, urlport)
	turnServerUDPAddr, err1 := net.ResolveUDPAddr("udp", turnServerAddr)
	if err1 != nil {
		err = fmt.Errorf("failed to resolve TURN server address: %s", err1)
		return
	}
	turnServerAddr = turnServerUDPAddr.String()
	var cfg *turn.ClientConfig
	var turnConn net.PacketConn
	var d net.Dialer
	ctx1, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if turnParams.udp {
		conn, err2 := net.DialUDP("udp", nil, turnServerUDPAddr) // nolint: noctx
		if err2 != nil {
			err = fmt.Errorf("failed to connect to TURN server: %s", err2)
			return
		}
		defer func() {
			if err1 = conn.Close(); err1 != nil {
				err = fmt.Errorf("failed to close TURN server connection: %s", err1)
				return
			}
		}()
		turnConn = &connectedUDPConn{conn}
	} else {
		conn, err2 := d.DialContext(ctx1, "tcp", turnServerAddr)
		if err2 != nil {
			err = fmt.Errorf("failed to connect to TURN server: %s", err2)
			return
		}
		defer func() {
			if err1 = conn.Close(); err1 != nil {
				err = fmt.Errorf("failed to close TURN server connection: %s", err1)
				return
			}
		}()
		turnConn = turn.NewSTUNConn(conn)
	}
	var addrFamily turn.RequestedAddressFamily
	if peer.IP.To4() != nil {
		addrFamily = turn.RequestedAddressFamilyIPv4
	} else {
		addrFamily = turn.RequestedAddressFamilyIPv6
	}

	cfg = &turn.ClientConfig{
		STUNServerAddr:         turnServerAddr,
		TURNServerAddr:         turnServerAddr,
		Conn:                   turnConn,
		Net:                    newDirectNet(),
		Username:               user,
		Password:               pass,
		RequestedAddressFamily: addrFamily,
		LoggerFactory:          logging.NewDefaultLoggerFactory(),
	}

	client, err1 := turn.NewClient(cfg)
	if err1 != nil {
		err = fmt.Errorf("failed to create TURN client: %s", err1)
		return
	}
	defer client.Close()

	err1 = client.Listen()
	if err1 != nil {
		err = fmt.Errorf("failed to listen: %s", err1)
		return
	}

	relayConn, err1 := client.Allocate()
	recordTURNAllocationResult(streamID, err1)
	if err1 != nil {
		err = fmt.Errorf("failed to allocate: %s", err1)
		return
	}

	// Safely track active streams globally
	connectedStreams.Add(1)
	defer func() {
		connectedStreams.Add(-1)
		if err1 := relayConn.Close(); err1 != nil {
			err = fmt.Errorf("failed to close TURN allocated connection: %s", err1)
		}
	}()

	if isDebug {
		log.Printf("[STREAM %d] relayed-address=%s", streamID, relayConn.LocalAddr().String())
	}

	bridgeTURNPackets(ctx, relayConn, conn2, peer, streamID)
}

func oneDtlsConnectionLoop(ctx context.Context, peer *net.UDPAddr, authentication dtlsauth.ClientAuthentication, clientAuthToken *sessionauth.Token, listenConn net.PacketConn, localPeer *localPeerPin, inboundChan <-chan *UDPPacket, connchan chan<- net.PacketConn, okchan chan<- struct{}, streamID int) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
			err := oneDtlsConnection(ctx, peer, authentication, clientAuthToken, listenConn, localPeer, inboundChan, connchan, okchan, streamID)
			if err != nil {
				if errors.Is(err, sessionauth.ErrRejected) {
					log.Printf("[STREAM %d] FATAL_CLIENT_AUTH_REJECTED: server rejected the configured client token", streamID)
					if globalAppCancel != nil {
						globalAppCancel()
					}
					return
				}
				if time.Now().Unix() < globalCaptchaLockout.Load() && strings.Contains(err.Error(), "context deadline exceeded") {
					continue
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Duration(10+rand.Intn(20)) * time.Second):
				}
			}
		}
	}
}

func oneTurnConnectionLoop(ctx context.Context, turnParams *turnParams, peer *net.UDPAddr, connchan <-chan net.PacketConn, t <-chan time.Time, streamID int) {
	for {
		select {
		case <-ctx.Done():
			return
		case conn2 := <-connchan:
			select {
			case <-t:
			case <-ctx.Done():
				return
			}
			c := make(chan error)
			go oneTurnConnection(ctx, turnParams, peer, conn2, streamID, c)

			if err := <-c; err != nil {
				if isFatalCaptchaError(err) {
					log.Printf("[STREAM %d] Fatal manual captcha error. Shutting down application.", streamID)
					if globalAppCancel != nil {
						globalAppCancel()
					}
					return
				}
				if strings.Contains(err.Error(), "CAPTCHA_WAIT_REQUIRED") {
					if !strings.Contains(err.Error(), "global lockout active") {
						log.Printf("[STREAM %d] Backing off for 60 seconds to avoid IP ban...", streamID)
						select {
						case <-ctx.Done():
							return
						case <-time.After(60 * time.Second):
						}
					} else {
						lockoutEnd := globalCaptchaLockout.Load()
						sleepDuration := time.Until(time.Unix(lockoutEnd, 0))
						if sleepDuration < 0 {
							sleepDuration = 5 * time.Second
						}
						select {
						case <-ctx.Done():
							return
						case <-time.After(sleepDuration):
						}
					}
				} else {
					log.Printf("[STREAM %d] %s", streamID, err)
					if !waitContextDelay(ctx, 2*time.Second) {
						return
					}
				}
			}
		}
	}
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	globalAppCancel = cancel
	defer cancel()
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-signalChan
		log.Printf("Terminating...\n")
		cancel()
		select {
		case <-signalChan:
		case <-time.After(5 * time.Second):
		}
		log.Fatalf("Exit...\n")
	}()

	host := flag.String("turn", "", "override TURN server ip")
	port := flag.String("port", "", "override TURN port")
	listen := flag.String("listen", "127.0.0.1:9000", "listen on ip:port")
	unsafeListenNonLoopback := flag.Bool(
		"unsafe-listen-non-loopback",
		false,
		"allow the local UDP/TCP proxy to listen outside a literal loopback address",
	)
	vklink := flag.String("vk-link", "", "VK calls invite link \"https://vk.com/call/join/...\"")
	yalink := flag.String("yandex-link", "", "Yandex telemost invite link \"https://telemost.yandex.ru/j/...\"")
	peerAddr := flag.String("peer", "", "peer server address (host:port)")
	n := flag.Int("n", 0, "connections to TURN (default 1 for UDP forwarding, 10 for VK VLESS, 1 for Yandex)")
	streamsPerCred := flag.Int("streams-per-cred", defaultStreamsPerCache, "TURN streams that share one credential cache")
	udp := flag.Bool("udp", false, "connect to TURN with UDP")
	unsafeUDPMultipath := flag.Bool(
		"unsafe-udp-multipath",
		false,
		"allow multiple non-VLESS UDP paths even though separate backend source ports cause WireGuard endpoint roaming",
	)
	noDTLS := flag.Bool("no-dtls", false, "unsupported compatibility flag; exits with an error")
	vlessMode := flag.Bool("vless", false, "VLESS mode: forward TCP connections (for VLESS) instead of UDP packets")
	vlessBond := flag.Bool("vless-bond", false, "VLESS bond mode: packet-level multipath across TURN/DTLS streams; requires -vless")
	vlessBondProtocol := flag.String("vless-bond-protocol", "auto", "VLESS bond control protocol: auto, v1, or v2")
	dnsMode := flag.String("dns", "auto", "VK resolver mode: auto (alias of udp) or udp")
	dnsServers := flag.String("dns-servers", "", "comma-separated DNS resolvers for VK auth, with optional ports")
	wrap := flag.Bool("wrap", false, "unsupported compatibility flag; exits with an error")
	wrapKey := flag.String("wrap-key", "", "unsupported compatibility flag; exits with an error")
	genWrapKey := flag.Bool("gen-wrap-key", false, "unsupported compatibility flag; exits with an error")
	dtlsServerFingerprint := flag.String("dtls-server-fingerprint", "", "required SHA-256 fingerprint of the DTLS server leaf certificate")
	dtlsInsecureSkipVerify := flag.Bool("dtls-insecure-skip-verify", false, "disable DTLS server authentication (unsafe compatibility override)")
	clientAuthTokenFile := flag.String("client-auth-token-file", "", "private 64-hex-character token file used to authenticate this client")
	unsafeDisableClientAuth := flag.Bool("unsafe-disable-client-auth", false, "skip client authentication for migration to an explicitly unauthenticated server")
	debugFlag := flag.Bool("debug", false, "enable debug logging")
	manualCaptchaFlag := flag.Bool("manual-captcha", false, "skip auto captcha solving, use manual mode immediately")
	tlsProfileFlag := flag.String("tls-profile", "", "tls-client profile for VK auth/captcha (e.g. confirmed_android_2, mesh_android, chrome_146); env VK_TURN_TLS_PROFILE overrides")
	diagnosticOptions := diagnostics.RegisterFlags(flag.CommandLine)
	tcputil.RegisterTuningFlags()
	flag.Parse()
	tlsClientProfileName = *tlsProfileFlag
	if err := validateClientCompatibilityFlags(*noDTLS, *dnsMode, *wrap, *wrapKey, *genWrapKey); err != nil {
		log.Fatalf("%s", err)
	}
	if err := validateClientListenAddress(*listen, *unsafeListenNonLoopback); err != nil {
		log.Fatalf("invalid client listen address: %s", err)
	}
	dtlsAuthentication, dtlsAuthenticationErr := dtlsauth.NewClientAuthentication(*dtlsServerFingerprint, *dtlsInsecureSkipVerify)
	if dtlsAuthenticationErr != nil {
		log.Fatalf("invalid DTLS authentication: %s", dtlsAuthenticationErr)
	}
	log.Printf("DTLS server authentication: %s", dtlsAuthentication.Description())
	if dtlsAuthentication.Insecure() {
		log.Printf("WARNING: DTLS server authentication is disabled; the connection is vulnerable to an active man-in-the-middle")
	}
	var clientAuthToken *sessionauth.Token
	if *unsafeDisableClientAuth {
		if *clientAuthTokenFile != "" {
			log.Fatalf("-client-auth-token-file and -unsafe-disable-client-auth cannot be used together")
		}
		log.Printf("WARNING: DTLS client authentication is disabled by explicit unsafe override")
	} else {
		token, tokenErr := sessionauth.LoadTokenFile(*clientAuthTokenFile)
		if tokenErr != nil {
			log.Fatalf("invalid client authentication token: %s", tokenErr)
		}
		clientAuthToken = &token
		log.Printf("DTLS client authentication: enabled")
	}
	if err := tcputil.ValidateTuning(); err != nil {
		log.Fatalf("invalid transport tuning: %s", err)
	}
	log.Printf("tuning: %s", tcputil.TuningSummary())
	diagnosticConfig, diagnosticErr := diagnosticOptions.Config()
	if diagnosticErr != nil {
		log.Fatalf("invalid diagnostics configuration: %s", diagnosticErr)
	}
	bondProtocolMode, protocolErr := parseVLESSBondProtocolMode(*vlessBondProtocol)
	if protocolErr != nil {
		log.Fatalf("%s", protocolErr)
	}
	if err := validateClientVLESSFlags(*vlessMode, *vlessBond, *n); err != nil {
		log.Fatalf("%s", err)
	}
	streamsPerCache = normalizeStreamsPerCredential(*streamsPerCred)
	resolvers := defaultDNSResolvers
	if strings.TrimSpace(*dnsServers) != "" {
		parsedResolvers, parseErr := parseDNSServers(*dnsServers)
		if parseErr != nil {
			log.Fatalf("bad -dns-servers: %s", parseErr)
		}
		resolvers = parsedResolvers
	}
	if *peerAddr == "" {
		log.Panicf("Need peer address!")
	}
	peer, err := net.ResolveUDPAddr("udp", *peerAddr)
	if err != nil {
		panic(err)
	}
	if (*vklink == "") == (*yalink == "") {
		log.Panicf("Need either vk-link or yandex-link!")
	}

	isDebug = *debugFlag
	manualCaptcha = *manualCaptchaFlag
	autoCaptchaSliderPOC = !manualCaptcha

	var link string
	var getCreds getCredsFunc
	if *vklink != "" {
		parts := strings.Split(*vklink, "join/")
		link = parts[len(parts)-1]

		dialer := dnsdialer.New(
			dnsdialer.WithResolvers(resolvers...),
			// Mobile networks frequently black-hole one public resolver. Race
			// the small resolver set instead of paying a sequential timeout for
			// every unavailable server.
			dnsdialer.WithStrategy(dnsdialer.Race{}),
			dnsdialer.WithTimeout(1200*time.Millisecond),
			// Respect short-lived TURN/VK DNS answers. A ten-hour floor pinned
			// stale relay addresses until process restart after an endpoint move.
			dnsdialer.WithCache(100, time.Second, 5*time.Minute),
		)

		getCreds = func(ctx context.Context, s string, streamID int) (string, string, string, error) {
			return getVkCredsCached(ctx, s, streamID, dialer)
		}
		if *n <= 0 && *vlessMode {
			*n = 10
		} else if *n <= 0 {
			*n = 1
		}
	} else {
		parts := strings.Split(*yalink, "j/")
		link = parts[len(parts)-1]
		getCreds = func(ctx context.Context, s string, streamID int) (string, string, string, error) {
			return getYandexCreds(ctx, s)
		}
		if *n <= 0 {
			*n = 1
		}
	}
	if idx := strings.IndexAny(link, "/?#"); idx != -1 {
		link = link[:idx]
	}
	requestedPaths := *n
	*n = normalizeUDPPathCount(*vlessMode, *n, *unsafeUDPMultipath)
	if *n != requestedPaths {
		log.Printf(
			"non-VLESS UDP multipath requested with %d paths; using one path to prevent WireGuard endpoint roaming (use -unsafe-udp-multipath only for legacy testing)",
			requestedPaths,
		)
	} else if !*vlessMode && *unsafeUDPMultipath && *n > 1 {
		log.Printf("WARNING: unsafe non-VLESS UDP multipath enabled; backend endpoint roaming can cause loss and reordering")
	}

	params := &turnParams{
		host:               *host,
		port:               *port,
		link:               link,
		udp:                *udp,
		getCreds:           getCreds,
		dtlsAuthentication: dtlsAuthentication,
		clientAuthToken:    clientAuthToken,
	}
	if _, err := diagnostics.Start(ctx, diagnosticConfig, &metrics.Process); err != nil {
		log.Fatalf("start diagnostics: %s", err)
	}

	if *vlessMode {
		runVLESSMode(ctx, params, peer, *listen, *n, *vlessBond, bondProtocolMode)
		return
	}

	listenConn, err := net.ListenPacket("udp", *listen)
	if err != nil {
		log.Panicf("Failed to listen: %s", err)
	}
	context.AfterFunc(ctx, func() {
		if closeErr := listenConn.Close(); closeErr != nil {
			log.Printf("Failed to close local connection: %s", closeErr)
		}
	})

	numStreams := *n
	if numStreams <= 0 {
		numStreams = 1
	}

	// Shared Worker Pool Queue for Aggregation
	inboundChan := make(chan *UDPPacket, 2000)
	localPeer := newLocalPeerPin(defaultLocalPeerRebindIdle)

	go func() {
		for {
			pktIface := packetPool.Get()
			pkt, ok := pktIface.(*UDPPacket)
			if !ok {
				log.Printf("packetPool returned unexpected type: %T", pktIface)
				continue
			}
			nRead, addr, err := listenConn.ReadFrom(pkt.Data)
			if err != nil {
				packetPool.Put(pkt)
				if ctx.Err() == nil {
					log.Printf("local UDP listener failed: %s", err)
					cancel()
				}
				return
			}

			// Keep return traffic pinned while the current peer is active.
			// A new source port may take over only after the bounded idle
			// interval; switching on every packet would let another local (or,
			// with an unsafe bind address, remote) sender steal the flow.
			if !localPeer.Accept(addr, time.Now()) {
				packetPool.Put(pkt)
				continue
			}

			pkt.N = nRead

			select {
			case inboundChan <- pkt:
			default:
				// Drop the packet only if the global queue is completely full
				metrics.Process.QueueDropped()
				packetPool.Put(pkt)
			}
		}
	}()

	wg1 := sync.WaitGroup{}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	okchan := make(chan struct{})
	connchan := make(chan net.PacketConn)
	wg1.Add(1)
	go func() {
		defer wg1.Done()
		oneDtlsConnectionLoop(ctx, peer, params.dtlsAuthentication, params.clientAuthToken, listenConn, localPeer, inboundChan, connchan, okchan, 0)
	}()
	wg1.Add(1)
	go func() {
		defer wg1.Done()
		oneTurnConnectionLoop(ctx, params, peer, connchan, ticker.C, 0)
	}()

	select {
	case <-okchan:
	case <-ctx.Done():
	}

	for i := 1; i < numStreams; i++ {
		cchan := make(chan net.PacketConn)
		wg1.Add(1)
		go func(streamID int) {
			defer wg1.Done()
			oneDtlsConnectionLoop(ctx, peer, params.dtlsAuthentication, params.clientAuthToken, listenConn, localPeer, inboundChan, cchan, nil, streamID)
		}(i)
		wg1.Add(1)
		go func(streamID int) {
			defer wg1.Done()
			oneTurnConnectionLoop(ctx, params, peer, cchan, ticker.C, streamID)
		}(i)
	}

	wg1.Wait()
}

// sessionPool manages a pool of smux sessions for round-robin TCP distribution.
type sessionPool struct {
	mu       sync.RWMutex
	sessions []pooledSmuxSession
	counter  atomic.Uint64
}

type pooledSmuxSession interface {
	OpenStream() (*smux.Stream, error)
	IsClosed() bool
	NumStreams() int
	Close() error
}

func (p *sessionPool) add(s pooledSmuxSession) {
	p.mu.Lock()
	p.sessions = append(p.sessions, s)
	metrics.Process.SessionOpened()
	p.mu.Unlock()
}

func (p *sessionPool) remove(s pooledSmuxSession) {
	p.mu.Lock()
	for i, sess := range p.sessions {
		if sess == s {
			p.sessions = append(p.sessions[:i], p.sessions[i+1:]...)
			metrics.Process.SessionClosed()
			break
		}
	}
	p.mu.Unlock()
}

// pickLeastLoaded returns the live session currently carrying the fewest smux
// streams, so a new TCP connection avoids a session whose TURN/DTLS path has
// stalled (head-of-line) and isn't draining. With a single session it behaves
// identically to round-robin selection; with N sessions it spreads load by
// actual occupancy instead of a counter.
// Round-robin (via the shared counter) breaks ties so equal-load sessions still
// rotate. Closed sessions are skipped.
func (p *sessionPool) pickLeastLoaded() pooledSmuxSession {
	candidates := p.candidates()
	if len(candidates) == 0 {
		return nil
	}
	return candidates[0]
}

func (p *sessionPool) candidates() []pooledSmuxSession {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := len(p.sessions)
	if n == 0 {
		return nil
	}
	start := int(p.counter.Add(1) % uint64(n))
	candidates := make([]pooledSmuxSession, 0, n)
	for i := 0; i < n; i++ {
		session := p.sessions[(start+i)%n]
		if !session.IsClosed() {
			candidates = append(candidates, session)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].NumStreams() < candidates[j].NumStreams()
	})
	return candidates
}

type pooledStreamResult struct {
	stream *smux.Stream
	err    error
}

func openPooledStream(
	ctx context.Context,
	session pooledSmuxSession,
	timeout time.Duration,
) (*smux.Stream, error) {
	result := make(chan pooledStreamResult)
	abandoned := make(chan struct{})
	go func() {
		stream, err := session.OpenStream()
		select {
		case result <- pooledStreamResult{stream: stream, err: err}:
		case <-abandoned:
			if stream != nil {
				_ = stream.Close()
			}
		}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case opened := <-result:
		close(abandoned)
		return opened.stream, opened.err
	case <-ctx.Done():
		close(abandoned)
		_ = session.Close()
		return nil, ctx.Err()
	case <-timer.C:
		close(abandoned)
		// smux OpenStream can wait for its own 30-second protocol timeout.
		// Closing the stalled session bounds the attempt and lets the pool fail
		// over immediately; the session owner will establish a replacement.
		_ = session.Close()
		return nil, fmt.Errorf("smux stream open timed out after %s", timeout)
	}
}

func (p *sessionPool) openStream(ctx context.Context, wait time.Duration) (*smux.Stream, error) {
	return p.openStreamWithAttemptTimeout(ctx, wait, time.Second)
}

func (p *sessionPool) openStreamWithAttemptTimeout(
	ctx context.Context,
	wait time.Duration,
	maxAttempt time.Duration,
) (*smux.Stream, error) {
	if wait <= 0 {
		return nil, fmt.Errorf("smux stream wait must be positive")
	}
	if maxAttempt <= 0 {
		return nil, fmt.Errorf("smux stream attempt timeout must be positive")
	}
	deadline := time.Now().Add(wait)
	var lastErr error
	for {
		for _, session := range p.candidates() {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				break
			}
			attemptTimeout := maxAttempt
			if remaining < attemptTimeout {
				attemptTimeout = remaining
			}
			stream, err := openPooledStream(ctx, session, attemptTimeout)
			if err == nil {
				return stream, nil
			}
			lastErr = err
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			if lastErr != nil {
				return nil, fmt.Errorf("all active smux sessions rejected the stream: %w", lastErr)
			}
			return nil, fmt.Errorf("no active smux sessions")
		}
		poll := 25 * time.Millisecond
		if remaining < poll {
			poll = remaining
		}
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (p *sessionPool) count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.sessions)
}

var defaultDNSResolvers = []string{
	"77.88.8.8:53",
	"77.88.8.1:53",
	"8.8.8.8:53",
	"8.8.4.4:53",
	"1.1.1.1:53",
	"1.0.0.1:53",
}

func validateClientVLESSFlags(vlessMode, vlessBond bool, streamCount int) error {
	if vlessBond && !vlessMode {
		return fmt.Errorf("-vless-bond requires -vless")
	}
	if vlessMode && streamCount < 0 {
		return fmt.Errorf("VLESS session count must not be negative")
	}
	if vlessMode && vlessBond && streamCount > tcputil.MaxBondPaths {
		return fmt.Errorf("VLESS bond session count must not exceed %d", tcputil.MaxBondPaths)
	}
	return nil
}

func normalizeVLESSSessionCount(streamCount int) int {
	if streamCount <= 0 {
		return 1
	}
	return streamCount
}

func normalizeUDPPathCount(vlessMode bool, pathCount int, unsafeMultipath bool) int {
	if !vlessMode && pathCount > 1 && !unsafeMultipath {
		return 1
	}
	return pathCount
}

func validateClientListenAddress(address string, unsafeNonLoopback bool) error {
	host, _, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return nil
	}
	if unsafeNonLoopback {
		return nil
	}
	return fmt.Errorf(
		"listen host must be a literal loopback IP; use -unsafe-listen-non-loopback only for a deliberately exposed proxy",
	)
}

func normalizeStreamsPerCredential(streams int) int {
	if streams <= 0 {
		return defaultStreamsPerCache
	}
	return streams
}

func parseDNSServers(raw string) ([]string, error) {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	servers := make([]string, 0, len(parts))
	for _, part := range parts {
		addr := strings.TrimSpace(part)
		if addr == "" {
			continue
		}
		normalized := addr
		if _, _, err := net.SplitHostPort(addr); err != nil {
			normalized = net.JoinHostPort(addr, "53")
		}
		host, port, err := net.SplitHostPort(normalized)
		if err != nil {
			return nil, fmt.Errorf("bad resolver %q: %w", addr, err)
		}
		if host == "" {
			return nil, fmt.Errorf("bad resolver %q: empty host", addr)
		}
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return nil, fmt.Errorf("bad resolver %q: invalid port", addr)
		}
		servers = append(servers, normalized)
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("no DNS servers provided")
	}
	return servers, nil
}

func validateClientCompatibilityFlags(noDTLS bool, dnsMode string, wrap bool, wrapKey string, generateWrapKey bool) error {
	if noDTLS {
		return fmt.Errorf("-no-dtls is not implemented in this build")
	}
	switch strings.ToLower(strings.TrimSpace(dnsMode)) {
	case "", "auto", "udp":
	case "doh":
		return fmt.Errorf("-dns=doh is not implemented in this build")
	default:
		return fmt.Errorf("unsupported -dns mode %q (expected auto or udp)", dnsMode)
	}
	if wrap || wrapKey != "" || generateWrapKey {
		return fmt.Errorf("WRAP compatibility mode is not implemented in this build")
	}
	return nil
}

func generateBondID() (string, error) {
	var id [16]byte
	if _, err := cryptorand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

// runVLESSMode implements TCP forwarding with round-robin across N TURN sessions.
// Without vlessBond, one TCP connection is pinned to one smux session, while
// different TCP connections are distributed across the active TURN/DTLS pool.
func runVLESSMode(
	ctx context.Context,
	tp *turnParams,
	peer *net.UDPAddr,
	listenAddr string,
	numSessions int,
	vlessBond bool,
	protocolMode vlessBondProtocolMode,
) {
	if vlessBond {
		runVLESSBondMode(ctx, tp, peer, listenAddr, numSessions, protocolMode)
		return
	}

	numSessions = normalizeVLESSSessionCount(numSessions)
	pool := &sessionPool{}
	log.Printf("vless mode: enabled")
	log.Printf("vless bond: %s", enabledText(vlessBond))

	// Start N session maintainers with staggered startup
	var wgMaint sync.WaitGroup
	for i := 0; i < numSessions; i++ {
		wgMaint.Add(1)
		go func(id int) {
			defer wgMaint.Done()
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(id) * 300 * time.Millisecond):
			}
			maintainVLESSSession(ctx, tp, peer, id, pool)
		}(i)
	}

	// Wait for at least one session
	log.Printf("VLESS mode: waiting for sessions to connect (total: %d)...", numSessions)
	for {
		select {
		case <-ctx.Done():
			wgMaint.Wait()
			return
		case <-time.After(100 * time.Millisecond):
		}
		if pool.count() > 0 {
			break
		}
	}

	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Panicf("TCP listen: %s", err)
	}
	context.AfterFunc(ctx, func() { _ = listener.Close() })
	log.Printf("VLESS mode: listening on %s (least-loaded across %d sessions)", listenAddr, numSessions)

	var wgConn sync.WaitGroup
	for {
		tcpConn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				wgConn.Wait()
				wgMaint.Wait()
				return
			default:
			}
			log.Printf("TCP accept error: %s", err)
			continue
		}

		wgConn.Add(1)
		go func(tc net.Conn) {
			defer wgConn.Done()
			defer func() { _ = tc.Close() }()
			stream, err := pool.openStream(ctx, 3*time.Second)
			if err != nil {
				log.Printf("smux open stream error: %s", err)
				return
			}
			defer func() { _ = stream.Close() }()
			pipe(ctx, tc, stream)
		}(tcpConn)
	}
}

func runVLESSBondMode(
	ctx context.Context,
	tp *turnParams,
	peer *net.UDPAddr,
	listenAddr string,
	numSessions int,
	protocolMode vlessBondProtocolMode,
) {
	runSupervisedVLESSBondMode(ctx, tp, peer, listenAddr, numSessions, protocolMode)
}

func enabledText(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

// maintainVLESSSession keeps one TURN+DTLS+KCP+smux session alive, reconnecting on failure.
func maintainVLESSSession(ctx context.Context, tp *turnParams, peer *net.UDPAddr, id int, pool *sessionPool) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		smuxSess, cleanup, err := createSmuxSession(ctx, tp, peer, id)
		if err != nil {
			if errors.Is(err, sessionauth.ErrRejected) {
				log.Printf("[session %d] FATAL_CLIENT_AUTH_REJECTED: server rejected the configured client token", id)
				if globalAppCancel != nil {
					globalAppCancel()
				}
				return
			}
			if isFatalCaptchaError(err) {
				log.Printf("[session %d] fatal captcha error; shutting down application", id)
				if globalAppCancel != nil {
					globalAppCancel()
				}
				return
			}
			log.Printf("[session %d] setup error: %s, retrying...", id, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}

		pool.add(smuxSess)
		log.Printf("[session %d] connected (active: %d)", id, pool.count())

		for !smuxSess.IsClosed() {
			select {
			case <-ctx.Done():
				pool.remove(smuxSess)
				cleanup()
				return
			case <-time.After(1 * time.Second):
			}
		}

		pool.remove(smuxSess)
		cleanup()
		if ctx.Err() != nil {
			return
		}
		metrics.Process.SessionReconnected()
		log.Printf("[session %d] disconnected (active: %d), reconnecting...", id, pool.count())

		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func maintainVLESSBondPath(
	ctx context.Context,
	tp *turnParams,
	peer *net.UDPAddr,
	id int,
	hello tcputil.BondHello,
	bonded *tcputil.BondedPacketConn,
	setupEvents chan<- vlessBondPathSetupEvent,
) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		dtlsConn, cleanup, err := createDTLSConnection(ctx, tp, peer, id)
		if err != nil {
			if errors.Is(err, sessionauth.ErrRejected) {
				log.Printf("[bond path %d] FATAL_CLIENT_AUTH_REJECTED: server rejected the configured client token", id)
				if globalAppCancel != nil {
					globalAppCancel()
				}
				return
			}
			if isFatalCaptchaError(err) {
				log.Printf("[bond path %d] fatal captcha error; shutting down application", id)
				if globalAppCancel != nil {
					globalAppCancel()
				}
				return
			}
			log.Printf("[bond path %d] setup error: %s, retrying...", id, err)
			if !waitContextDelay(ctx, vlessBondPathRetryDelay(3*time.Second)) {
				return
			}
			continue
		}
		if ctx.Err() != nil {
			cleanup()
			return
		}

		if err := tcputil.WriteBondHelloConfig(dtlsConn, hello); err != nil {
			log.Printf("[bond path %d] hello error: %s, retrying...", id, err)
			reportVLESSBondPathSetupError(setupEvents, id, fmt.Errorf("write bond hello: %w", err))
			cleanup()
			if !waitContextDelay(ctx, vlessBondPathRetryDelay(3*time.Second)) {
				return
			}
			continue
		}
		if hello.Version == tcputil.BondProtocolV2 {
			if err := tcputil.ReadBondHelloAck(dtlsConn); err != nil {
				log.Printf("[bond path %d] V2 acknowledgement error: %s, retrying...", id, err)
				reportVLESSBondPathSetupError(setupEvents, id, fmt.Errorf("read bond V2 acknowledgement: %w", err))
				cleanup()
				if !waitContextDelay(ctx, vlessBondPathRetryDelay(3*time.Second)) {
					return
				}
				continue
			}
		}
		if ctx.Err() != nil {
			cleanup()
			return
		}

		done := bonded.AddConn(dtlsConn, cleanup)
		log.Printf("[bond path %d] connected (active: %d)", id, bonded.Count())

		select {
		case <-ctx.Done():
			return
		case <-done:
			if ctx.Err() != nil {
				return
			}
			metrics.Process.PathReconnected()
			log.Printf("[bond path %d] disconnected (active: %d), reconnecting...", id, bonded.Count())
		}

		if !waitContextDelay(ctx, vlessBondPathRetryDelay(2*time.Second)) {
			return
		}
	}
}

func reportVLESSBondPathSetupError(events chan<- vlessBondPathSetupEvent, pathID int, err error) {
	if events == nil || err == nil {
		return
	}
	select {
	case events <- vlessBondPathSetupEvent{pathID: pathID, err: err}:
	default:
	}
}

// createSmuxSession establishes a full TURN+DTLS+KCP+smux pipeline and returns
// the smux session along with a cleanup function to tear down all layers.
func createSmuxSession(ctx context.Context, tp *turnParams, peer *net.UDPAddr, id int) (*smux.Session, func(), error) {
	dtlsConn, cleanup, err := createDTLSConnection(ctx, tp, peer, id)
	if err != nil {
		return nil, nil, err
	}

	cleanupFns := []func(){cleanup}
	cleanupAll := func() {
		for i := len(cleanupFns) - 1; i >= 0; i-- {
			cleanupFns[i]()
		}
	}

	// Create KCP session over DTLS
	kcpSess, err := tcputil.NewKCPOverDTLS(dtlsConn, false)
	if err != nil {
		cleanupAll()
		return nil, nil, fmt.Errorf("KCP session: %w", err)
	}
	cleanupFns = append(cleanupFns, func() { _ = kcpSess.Close() })
	log.Printf("KCP session established")

	// Create smux client session over KCP
	smuxSess, err := smux.Client(kcpSess, tcputil.DefaultSmuxConfig())
	if err != nil {
		cleanupAll()
		return nil, nil, fmt.Errorf("smux client: %w", err)
	}
	cleanupFns = append(cleanupFns, func() { _ = smuxSess.Close() })
	log.Printf("smux session established")

	return smuxSess, cleanupAll, nil
}

func createDTLSConnection(ctx context.Context, tp *turnParams, peer *net.UDPAddr, id int) (net.Conn, func(), error) {
	var cleanupFns []func()
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			for i := len(cleanupFns) - 1; i >= 0; i-- {
				cleanupFns[i]()
			}
		})
	}

	// 1. Get TURN credentials
	user, pass, rawURL, err := tp.getCreds(ctx, tp.link, id)
	if err != nil {
		return nil, nil, fmt.Errorf("get TURN creds: %w", err)
	}
	urlhost, urlport, err := net.SplitHostPort(rawURL)
	if err != nil {
		return nil, nil, fmt.Errorf("parse TURN addr: %w", err)
	}
	if tp.host != "" {
		urlhost = tp.host
	}
	if tp.port != "" {
		urlport = tp.port
	}
	turnServerAddr := net.JoinHostPort(urlhost, urlport)
	turnServerUDPAddr, err := net.ResolveUDPAddr("udp", turnServerAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve TURN addr: %w", err)
	}
	turnServerAddr = turnServerUDPAddr.String()
	log.Printf("[session %d] TURN server IP: %s", id, turnServerUDPAddr.IP)

	// 2. Connect to TURN server
	var turnConn net.PacketConn
	ctx1, cancel1 := context.WithTimeout(ctx, 5*time.Second)
	defer cancel1()
	if tp.udp {
		c, err1 := net.DialUDP("udp", nil, turnServerUDPAddr)
		if err1 != nil {
			return nil, nil, fmt.Errorf("dial TURN (udp): %w", err1)
		}
		cleanupFns = append(cleanupFns, func() { _ = c.Close() })
		turnConn = &connectedUDPConn{c}
	} else {
		var d net.Dialer
		c, err1 := d.DialContext(ctx1, "tcp", turnServerAddr)
		if err1 != nil {
			return nil, nil, fmt.Errorf("dial TURN (tcp): %w", err1)
		}
		cleanupFns = append(cleanupFns, func() { _ = c.Close() })
		turnConn = turn.NewSTUNConn(c)
	}

	// 3. Create TURN client and allocate relay
	var addrFamily turn.RequestedAddressFamily
	if peer.IP.To4() != nil {
		addrFamily = turn.RequestedAddressFamilyIPv4
	} else {
		addrFamily = turn.RequestedAddressFamilyIPv6
	}
	cfg := &turn.ClientConfig{
		STUNServerAddr:         turnServerAddr,
		TURNServerAddr:         turnServerAddr,
		Conn:                   turnConn,
		Net:                    newDirectNet(),
		Username:               user,
		Password:               pass,
		RequestedAddressFamily: addrFamily,
		LoggerFactory:          logging.NewDefaultLoggerFactory(),
	}
	turnClient, err := turn.NewClient(cfg)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("create TURN client: %w", err)
	}
	cleanupFns = append(cleanupFns, func() { turnClient.Close() })
	if err = turnClient.Listen(); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("TURN listen: %w", err)
	}
	relayConn, err := turnClient.Allocate()
	recordTURNAllocationResult(id, err)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("TURN allocate: %w", err)
	}
	cleanupFns = append(cleanupFns, func() { _ = relayConn.Close() })
	log.Printf("relayed-address=%s", relayConn.LocalAddr().String())

	// 4. Establish DTLS over TURN relay
	certificate, err := selfsign.GenerateSelfSigned()
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("generate cert: %w", err)
	}
	dtlsPC := &relayPacketConn{relay: relayConn, peer: peer}
	dtlsOptions, err := clientDTLSOptions(certificate, tp.dtlsAuthentication)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("DTLS authentication config: %w", err)
	}
	dtlsConn, err := dtls.ClientWithOptions(dtlsPC, peer, dtlsOptions...)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("DTLS client create: %w", err)
	}
	ctx2, cancel2 := context.WithTimeout(ctx, 30*time.Second)
	defer cancel2()
	if err = dtlsConn.HandshakeContext(ctx2); err != nil {
		_ = dtlsConn.Close()
		cleanup()
		return nil, nil, fmt.Errorf("DTLS handshake: %w", err)
	}
	if tp.clientAuthToken != nil {
		if err = sessionauth.Authenticate(ctx2, dtlsConn, *tp.clientAuthToken); err != nil {
			_ = dtlsConn.Close()
			cleanup()
			return nil, nil, fmt.Errorf("authenticate client: %w", err)
		}
	}
	cleanupFns = append(cleanupFns, func() { _ = dtlsConn.Close() })
	connectedStreams.Add(1)
	cleanupFns = append(cleanupFns, func() { connectedStreams.Add(-1) })
	log.Printf("DTLS connection established")

	return dtlsConn, cleanup, nil
}

// relayPacketConn wraps a TURN relay PacketConn to direct all writes to the peer.
type relayPacketConn struct {
	relay net.PacketConn
	peer  net.Addr
}

func (r *relayPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	return r.relay.ReadFrom(b)
}

func (r *relayPacketConn) WriteTo(b []byte, _ net.Addr) (int, error) {
	return r.relay.WriteTo(b, r.peer)
}

func (r *relayPacketConn) Close() error                       { return r.relay.Close() }
func (r *relayPacketConn) LocalAddr() net.Addr                { return r.relay.LocalAddr() }
func (r *relayPacketConn) SetDeadline(t time.Time) error      { return r.relay.SetDeadline(t) }
func (r *relayPacketConn) SetReadDeadline(t time.Time) error  { return r.relay.SetReadDeadline(t) }
func (r *relayPacketConn) SetWriteDeadline(t time.Time) error { return r.relay.SetWriteDeadline(t) }

// pipe copies data bidirectionally between two connections.
func pipe(ctx context.Context, c1, c2 net.Conn) {
	if err := tcputil.Pipe(ctx, c1, c2); err != nil && ctx.Err() == nil {
		log.Printf("pipe: %v", err)
	}
}

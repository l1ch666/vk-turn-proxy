package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	neturl "net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/bschaatsbergen/dnsdialer"
)

const (
	maxCaptchaRequestBytes  = 1 << 20
	maxCaptchaResponseBytes = 8 << 20
	localCaptchaEntryQuery  = "_vkturn_entry=1"
)

var allowedCaptchaDomainSuffixes = []string{
	"vk.com",
	"vk.ru",
	"vk-cdn.net",
	"userapi.com",
	"okcdn.ru",
	"mycdn.me",
}

type browserCommand struct {
	name string
	args []string
}

type captchaEndpoint struct {
	listener net.Listener
	host     string
	origin   string
	prefix   string
	baseURL  string
}

func newCaptchaEndpoint() (*captchaEndpoint, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for local captcha: %w", err)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("generate local captcha nonce: %w", err)
	}
	host := listener.Addr().String()
	origin := "http://" + host
	prefix := "/" + hex.EncodeToString(nonce[:])
	return &captchaEndpoint{
		listener: listener,
		host:     host,
		origin:   origin,
		prefix:   prefix,
		baseURL:  origin + prefix,
	}, nil
}

func (endpoint *captchaEndpoint) urlForPath(path string) string {
	if path == "" {
		path = "/"
	}
	if path[0] != '/' {
		path = "/" + path
	}
	return endpoint.baseURL + path
}

func isLoopbackHTTPURL(parsed *neturl.URL) bool {
	if parsed == nil || parsed.User != nil || !strings.EqualFold(parsed.Scheme, "http") {
		return false
	}
	ip := net.ParseIP(parsed.Hostname())
	return ip != nil && ip.IsLoopback() && parsed.Port() != ""
}

func isAllowedCaptchaTarget(target *neturl.URL) bool {
	if target == nil || !strings.EqualFold(target.Scheme, "https") || target.User != nil {
		return false
	}
	if port := target.Port(); port != "" && port != "443" {
		return false
	}
	hostname := strings.ToLower(strings.TrimSuffix(target.Hostname(), "."))
	if hostname == "" || net.ParseIP(hostname) != nil {
		return false
	}
	for _, suffix := range allowedCaptchaDomainSuffixes {
		if hostname == suffix || strings.HasSuffix(hostname, "."+suffix) {
			return true
		}
	}
	return false
}

func secureCaptchaHandler(next http.Handler, expectedHost string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || !net.ParseIP(host).IsLoopback() || !strings.EqualFold(r.Host, expectedHost) {
			http.Error(w, "loopback access required", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		r.Body = http.MaxBytesReader(w, r.Body, maxCaptchaRequestBytes)
		next.ServeHTTP(w, r)
	})
}

func localCaptchaURLForTarget(targetURL *neturl.URL, endpoint *captchaEndpoint) string {
	targetPath := targetURL.Path
	if targetPath == "" {
		targetPath = "/"
	}
	localURL := &neturl.URL{
		Scheme:   "http",
		Host:     endpoint.host,
		Path:     endpoint.prefix + targetPath,
		RawQuery: targetURL.RawQuery,
	}
	return localURL.String()
}

// localCaptchaEntryURLForTarget keeps the upstream challenge query (which can
// contain a reusable session token) out of the terminal/app log and browser
// history. The loopback handler replaces this harmless marker server-side.
func localCaptchaEntryURLForTarget(targetURL *neturl.URL, endpoint *captchaEndpoint) string {
	localURL, err := neturl.Parse(localCaptchaURLForTarget(targetURL, endpoint))
	if err != nil {
		return endpoint.urlForPath("/")
	}
	localURL.RawQuery = localCaptchaEntryQuery
	return localURL.String()
}

func targetOrigin(targetURL *neturl.URL) string {
	return targetURL.Scheme + "://" + targetURL.Host
}

func sameCaptchaOrigin(left, right *neturl.URL) bool {
	if left == nil || right == nil {
		return false
	}
	return strings.EqualFold(left.Scheme, right.Scheme) &&
		strings.EqualFold(left.Hostname(), right.Hostname()) &&
		effectiveHTTPSPort(left) == effectiveHTTPSPort(right)
}

func effectiveHTTPSPort(target *neturl.URL) string {
	if port := target.Port(); port != "" {
		return port
	}
	if strings.EqualFold(target.Scheme, "https") {
		return "443"
	}
	return ""
}

func allowedGenericProxyMethod(method string, sameOrigin bool) bool {
	switch method {
	case http.MethodGet, http.MethodHead:
		return true
	case http.MethodPost:
		return sameOrigin
	default:
		return false
	}
}

func isSafeLocalRedirectPath(raw string) bool {
	if raw == "" || raw[0] != '/' {
		return false
	}
	if len(raw) > 1 && (raw[1] == '/' || raw[1] == '\\') {
		return false
	}
	return true
}

func rewriteProxyRedirectLocation(raw string, targetURL *neturl.URL, endpoint *captchaEndpoint) (string, bool) {
	if isSafeLocalRedirectPath(raw) {
		return endpoint.urlForPath(raw), true
	}

	parsed, err := neturl.Parse(raw)
	if err != nil {
		return "", false
	}
	if !strings.EqualFold(parsed.Scheme, targetURL.Scheme) || !strings.EqualFold(parsed.Host, targetURL.Host) {
		return "", false
	}

	return localCaptchaURLForTarget(parsed, endpoint), true
}

func rewriteProxyHeaderURL(raw string, targetURL *neturl.URL, endpoint *captchaEndpoint) string {
	if raw == "" {
		return raw
	}
	parsed, err := neturl.Parse(raw)
	if err != nil {
		return raw
	}
	if parsed.Scheme != "http" || !strings.EqualFold(parsed.Host, endpoint.host) {
		return raw
	}
	parsed.Scheme = targetURL.Scheme
	parsed.Host = targetURL.Host
	parsed.Path = strings.TrimPrefix(parsed.Path, endpoint.prefix)
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	parsed.RawPath = ""
	return parsed.String()
}

func rewriteProxyRequest(req *http.Request, targetURL *neturl.URL, endpoint *captchaEndpoint) {
	req.URL.Scheme = targetURL.Scheme
	req.URL.Host = targetURL.Host
	if req.URL.Path == "" {
		req.URL.Path = targetURL.Path
	}
	req.Host = targetURL.Host

	req.Header.Del("Accept-Encoding")
	req.Header.Del("TE") // Disable transfer encoding compression
	for _, headerName := range []string{"Origin", "Referer"} {
		if rewritten := rewriteProxyHeaderURL(req.Header.Get(headerName), targetURL, endpoint); rewritten != "" {
			req.Header.Set(headerName, rewritten)
		} else {
			req.Header.Del(headerName)
		}
	}
}

func rewriteGenericProxyRequest(
	req *http.Request,
	targetURL, primaryTarget *neturl.URL,
	endpoint *captchaEndpoint,
) {
	req.URL.Path = targetURL.Path
	if req.URL.Path == "" {
		req.URL.Path = "/"
	}
	req.URL.RawQuery = targetURL.RawQuery
	rewriteProxyRequest(req, targetURL, endpoint)
	if sameCaptchaOrigin(targetURL, primaryTarget) {
		return
	}
	for _, headerName := range []string{
		"Authorization",
		"Cookie",
		"Origin",
		"Proxy-Authorization",
		"Referer",
	} {
		req.Header.Del(headerName)
	}
}

func extractSuccessToken(body []byte) string {
	var payload struct {
		Response struct {
			SuccessToken string `json:"success_token"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return payload.Response.SuccessToken
}

func rewriteProxyCookies(header http.Header) {
	cookies := (&http.Response{Header: header}).Cookies()
	if len(cookies) == 0 {
		return
	}
	header.Del("Set-Cookie")
	for _, cookie := range cookies {
		cookie.Domain = ""
		cookie.Secure = false
		cookie.Partitioned = false
		if cookie.SameSite == http.SameSiteNoneMode {
			cookie.SameSite = http.SameSiteLaxMode
		}
		header.Add("Set-Cookie", cookie.String())
	}
}

func rewriteCaptchaHTML(html string, targetURL *neturl.URL, endpoint *captchaEndpoint) string {
	localOrigin := endpoint.baseURL
	upstreamOrigin := targetOrigin(targetURL)
	html = strings.ReplaceAll(html, upstreamOrigin, localOrigin)

	script := fmt.Sprintf(`
<script>
(function() {
    var localOrigin = %q;
    var upstreamOrigin = %q;

    function rewriteUrl(urlStr) {
        if (!urlStr || typeof urlStr !== 'string') return urlStr;
        if (urlStr.indexOf(localOrigin) === 0) return urlStr;
        if (urlStr.indexOf(upstreamOrigin) === 0) return localOrigin + urlStr.slice(upstreamOrigin.length);
        if (urlStr.indexOf('//') === 0) {
            return localOrigin + '/generic_proxy?proxy_url=' + encodeURIComponent('https:' + urlStr);
        }
        if (urlStr.indexOf('http://') === 0 || urlStr.indexOf('https://') === 0) {
            return localOrigin + '/generic_proxy?proxy_url=' + encodeURIComponent(urlStr);
        }
        if (urlStr.indexOf('/') === 0) return localOrigin + urlStr;
        return urlStr;
    }

    function rewriteElementAttr(el, attr) {
        if (!el || !el.getAttribute) return;
        var value = el.getAttribute(attr);
        if (!value) return;
        var rewritten = rewriteUrl(value);
        if (rewritten !== value) {
            el.setAttribute(attr, rewritten);
        }
    }

    function rewriteDocument(root) {
        if (!root || !root.querySelectorAll) return;
        root.querySelectorAll('[href]').forEach(function(el) { rewriteElementAttr(el, 'href'); });
        root.querySelectorAll('[src]').forEach(function(el) { rewriteElementAttr(el, 'src'); });
        root.querySelectorAll('form[action]').forEach(function(el) { rewriteElementAttr(el, 'action'); });
    }

    function handleSuccessToken(token) {
        if (!token) return;
        fetch(localOrigin + '/local-captcha-result', {
            method: 'POST',
            headers: {'Content-Type': 'application/x-www-form-urlencoded'},
            body: 'token=' + encodeURIComponent(token)
        }).then(function() {
            document.body.innerHTML = '<h2 style="text-align:center;margin-top:20vh">Done! You can close the page.</h2>';
            setTimeout(function() { window.close(); }, 300);
        }).catch(function() {});
    }

    var origOpen = XMLHttpRequest.prototype.open;
    XMLHttpRequest.prototype.open = function() {
        if (arguments[1] && typeof arguments[1] === 'string') {
            this._origUrl = arguments[1];
            arguments[1] = rewriteUrl(arguments[1]);
        }
        return origOpen.apply(this, arguments);
    };

    var origSend = XMLHttpRequest.prototype.send;
    XMLHttpRequest.prototype.send = function() {
        var xhr = this;
        if (this._origUrl && this._origUrl.indexOf('captchaNotRobot.check') !== -1) {
            xhr.addEventListener('load', function() {
                try {
                    var data = JSON.parse(xhr.responseText);
                    if (data.response && data.response.success_token) {
                        handleSuccessToken(data.response.success_token);
                    }
                } catch (e) {}
            });
        }
        return origSend.apply(this, arguments);
    };

    var origFetch = window.fetch;
    if (origFetch) {
        window.fetch = function() {
            var url = arguments[0];
            var isObj = (typeof url === 'object' && url && url.url);
            var urlStr = isObj ? url.url : url;
            var origUrlStr = urlStr;

            if (typeof urlStr === 'string') {
                urlStr = rewriteUrl(urlStr);
                arguments[0] = urlStr;
            }

            var p = origFetch.apply(this, arguments);
            if (typeof origUrlStr === 'string' && origUrlStr.indexOf('captchaNotRobot.check') !== -1) {
                p.then(function(response) {
                    return response.clone().json();
                }).then(function(data) {
                    if (data.response && data.response.success_token) {
                        handleSuccessToken(data.response.success_token);
                    }
                }).catch(function() {});
            }
            return p;
        };
    }

    document.addEventListener('submit', function(event) {
        if (event.target && event.target.action) {
            event.target.action = rewriteUrl(event.target.action);
        }
    }, true);

    document.addEventListener('click', function(event) {
        var target = event.target && event.target.closest ? event.target.closest('a[href]') : null;
        if (target && target.href) {
            target.href = rewriteUrl(target.href);
        }
    }, true);

    var origFormSubmit = HTMLFormElement.prototype.submit;
    HTMLFormElement.prototype.submit = function() {
        if (this.action) {
            this.action = rewriteUrl(this.action);
        }
        return origFormSubmit.apply(this, arguments);
    };

    var origWindowOpen = window.open;
    if (origWindowOpen) {
        window.open = function(url) {
            if (typeof url === 'string') {
                arguments[0] = rewriteUrl(url);
            }
            return origWindowOpen.apply(this, arguments);
        };
    }

    rewriteDocument(document);
    if (document.documentElement && window.MutationObserver) {
        new MutationObserver(function(mutations) {
            mutations.forEach(function(mutation) {
                if (mutation.type === 'attributes' && mutation.target) {
                    rewriteElementAttr(mutation.target, mutation.attributeName);
                    return;
                }
                mutation.addedNodes.forEach(function(node) {
                    if (node.nodeType === 1) {
                        rewriteDocument(node);
                    }
                });
            });
        }).observe(document.documentElement, {
            subtree: true,
            childList: true,
            attributes: true,
            attributeFilter: ['href', 'src', 'action']
        });
    }
})();
</script>
`, localOrigin, upstreamOrigin)

	switch {
	case strings.Contains(html, "</head>"):
		return strings.Replace(html, "</head>", script+"</head>", 1)
	case strings.Contains(html, "</body>"):
		return strings.Replace(html, "</body>", script+"</body>", 1)
	default:
		return html + script
	}
}

func newCaptchaProxyTransport(dialer *dnsdialer.Dialer) *http.Transport {
	transport := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     false,
	}
	if dialer != nil {
		transport.DialContext = dialer.DialContext
	}
	return transport
}

// captchaProxyLogger wraps the manual-captcha proxy transport to dump the
// request bodies of captchaNotRobot.* calls under -debug. When the user solves
// the captcha manually in the WebView, this captures the exact, VK-accepted
// payload (sensor format/values, params, sequence) — the ground truth needed to
// fix the automatic solver. session_token is redacted (one-time, but sensitive).
type captchaProxyLogger struct {
	base http.RoundTripper
}

func (c captchaProxyLogger) RoundTrip(req *http.Request) (*http.Response, error) {
	if isDebug && req.Body != nil && strings.Contains(req.URL.Path, "captchaNotRobot") {
		if body, err := io.ReadAll(req.Body); err == nil {
			_ = req.Body.Close()
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
			dump := redactSessionToken(body)
			const maxLen = 6000
			if len(dump) > maxLen {
				dump = dump[:maxLen] + "...(truncated)"
			}
			log.Printf("[Captcha Proxy] real browser %s body: %s", req.URL.Path, dump)
		}
	}
	return c.base.RoundTrip(req)
}

// redactSessionToken keeps the request shape useful for debugging while
// removing one-time challenge material and reusable credentials.
func redactSessionToken(body []byte) string {
	sensitive := map[string]struct{}{
		"access_token":  {},
		"answer":        {},
		"client_secret": {},
		"credential":    {},
		"hash":          {},
		"password":      {},
		"session_token": {},
		"success_token": {},
		"token":         {},
	}
	parts := strings.Split(string(body), "&")
	for i, p := range parts {
		key, _, found := strings.Cut(p, "=")
		decodedKey, err := neturl.QueryUnescape(key)
		if err != nil {
			decodedKey = key
		}
		if _, ok := sensitive[strings.ToLower(decodedKey)]; ok && found {
			parts[i] = key + "=***"
		}
	}
	return strings.Join(parts, "&")
}

func waitForCaptchaResult(ctx context.Context, keyCh <-chan string) (string, error) {
	select {
	case key, ok := <-keyCh:
		if !ok {
			return "", errors.New("captcha result channel closed")
		}
		return key, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// runCaptchaServerAndWait opens the browser and waits for the solution token or
// context cancellation. The loopback listeners are always released before the
// function returns, including timeout and application-shutdown paths.
func runCaptchaServerAndWait(
	ctx context.Context,
	endpoint *captchaEndpoint,
	handler http.Handler,
	captchaURL string,
	keyCh <-chan string,
	logPrefix string,
) (key string, err error) {
	if err := ctx.Err(); err != nil {
		_ = endpoint.listener.Close()
		return "", err
	}

	rootMux := http.NewServeMux()
	rootMux.Handle(endpoint.prefix+"/", http.StripPrefix(endpoint.prefix, handler))
	srv := &http.Server{
		Handler:           secureCaptchaHandler(rootMux, endpoint.host),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}

	go func() {
		if serveErr := srv.Serve(endpoint.listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Printf("%s: %s", logPrefix, serveErr)
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if shutdownErr := srv.Shutdown(shutdownCtx); shutdownErr != nil {
			_ = srv.Close()
			if err == nil {
				err = shutdownErr
			}
		}
	}()

	fmt.Println("\n==============================================")
	fmt.Println("ACTION REQUIRED: MANUAL CAPTCHA SOLVING NEEDED")
	fmt.Println("Open this URL in your browser: " + captchaURL)
	fmt.Println("==============================================")
	fmt.Println()

	openBrowser(captchaURL)
	return waitForCaptchaResult(ctx, keyCh)
}

// notifyKey pushes the key string to the given channel without blocking
func notifyKey(keyCh chan<- string, key string) {
	if key != "" {
		select {
		case keyCh <- key:
		default:
		}
	}
}

func solveCaptchaViaHTTP(ctx context.Context, captchaImg string) (string, error) {
	endpoint, err := newCaptchaEndpoint()
	if err != nil {
		return "", err
	}
	keyCh := make(chan string, 1)
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<!DOCTYPE html>
<html><head>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>body{font-family:sans-serif;text-align:center;padding:20px}
img{max-width:100%%;margin:16px 0}
input{font-size:24px;padding:12px;width:80%%;box-sizing:border-box}
button{font-size:24px;padding:12px 32px;margin-top:12px;cursor:pointer}</style>
</head><body>
<h2>Solve the Captcha</h2>
<img src="%s" alt="captcha"/>
<form onsubmit="fetch(%q,{method:'POST',headers:{'Content-Type':'application/x-www-form-urlencoded'},body:'key='+encodeURIComponent(document.getElementById('k').value)}).then(()=>{document.body.innerHTML='<h2>Done!</h2>';setTimeout(function(){window.close();}, 300);});return false;">
<br><input id="k" type="text" autofocus placeholder="Text from image"/>
<br><button type="submit">Submit</button>
</form></body></html>`, html.EscapeString(captchaImg), endpoint.urlForPath("/solve"))
	})

	mux.HandleFunc("/solve", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != endpoint.origin {
			http.Error(w, "invalid origin", http.StatusForbidden)
			return
		}
		notifyKey(keyCh, r.FormValue("key"))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<!DOCTYPE html><html><body><h2>Done!</h2></body></html>`)
	})

	captchaURL := endpoint.urlForPath("/")
	return runCaptchaServerAndWait(ctx, endpoint, mux, captchaURL, keyCh, "captcha HTTP server error")
}

func solveCaptchaViaProxy(ctx context.Context, redirectURI string, dialer *dnsdialer.Dialer) (string, error) {
	keyCh := make(chan string, 1)

	targetURL, err := neturl.Parse(redirectURI)
	if err != nil {
		return "", fmt.Errorf("invalid redirect URI: %v", err)
	}
	if !isAllowedCaptchaTarget(targetURL) {
		return "", fmt.Errorf("captcha redirect target is not an allowed VK HTTPS origin")
	}
	endpoint, err := newCaptchaEndpoint()
	if err != nil {
		return "", err
	}

	var transport http.RoundTripper = captchaProxyLogger{base: newCaptchaProxyTransport(dialer)}

	proxy := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(req *httputil.ProxyRequest) {
			rewriteProxyRequest(req.Out, targetURL, endpoint)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			var networkError net.Error
			log.Printf(
				"[Captcha Proxy] ERROR for %s %s: type=%T timeout=%t",
				r.Method,
				r.URL.Path,
				err,
				errors.As(err, &networkError) && networkError.Timeout(),
			)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = fmt.Fprintf(
				w,
				`<!DOCTYPE html><html><body style="font-family:sans-serif;padding:20px"><h2>Captcha proxy error</h2><p>%s %s</p><p>%s</p></body></html>`,
				html.EscapeString(r.Method),
				html.EscapeString(r.URL.Path),
				"upstream request failed",
			)
		},
		ModifyResponse: func(res *http.Response) error {
			rewriteProxyCookies(res.Header)

			if res.StatusCode >= 300 && res.StatusCode < 400 {
				if loc := res.Header.Get("Location"); loc != "" {
					log.Printf("[Captcha Proxy] processing upstream redirect")
					if rewritten, ok := rewriteProxyRedirectLocation(loc, targetURL, endpoint); ok {
						res.Header.Set("Location", rewritten)
					} else {
						res.Header.Del("Location")
					}
				}
			}

			contentType := res.Header.Get("Content-Type")
			contentEncoding := res.Header.Get("Content-Encoding")
			log.Printf("[Captcha Proxy] %s %d | Content-Type: %q, Encoding: %q", res.Request.Method, res.StatusCode, contentType, contentEncoding)

			shouldInspectBody := strings.Contains(contentType, "text/html") ||
				strings.Contains(contentType, "application/xhtml+xml") ||
				strings.Contains(res.Request.URL.Path, "captchaNotRobot.check")

			if !shouldInspectBody {
				return nil
			}

			reader := res.Body
			if res.Header.Get("Content-Encoding") == "gzip" {
				gzReader, err := gzip.NewReader(res.Body)
				if err != nil {
					return fmt.Errorf("decode gzip captcha response: %w", err)
				}
				reader = gzReader
				defer func() {
					if err := gzReader.Close(); err != nil {
						log.Printf("failed to close gzip reader: %v", err)
					}
				}()
			}

			bodyBytes, err := io.ReadAll(io.LimitReader(reader, maxCaptchaResponseBytes+1))
			if err != nil {
				return err
			}
			if len(bodyBytes) > maxCaptchaResponseBytes {
				return fmt.Errorf("captcha proxy response exceeds %d bytes", maxCaptchaResponseBytes)
			}
			if err := res.Body.Close(); err != nil {
				return err
			}

			if strings.Contains(res.Request.URL.Path, "captchaNotRobot.check") {
				notifyKey(keyCh, extractSuccessToken(bodyBytes))
			}

			if strings.Contains(contentType, "text/html") {
				for _, headerName := range []string{
					"Content-Security-Policy",
					"Content-Security-Policy-Report-Only",
					"X-Content-Security-Policy",
					"X-WebKit-CSP",
					"Cross-Origin-Opener-Policy",
					"Cross-Origin-Embedder-Policy",
					"Cross-Origin-Resource-Policy",
					"X-Frame-Options",
					"Strict-Transport-Security",
					"Alt-Svc",
				} {
					res.Header.Del(headerName)
				}

				bodyBytes = []byte(rewriteCaptchaHTML(string(bodyBytes), targetURL, endpoint))
				res.Header.Del("Content-Encoding")
			}

			res.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			res.ContentLength = int64(len(bodyBytes))
			res.Header.Set("Content-Length", fmt.Sprint(len(bodyBytes)))

			return nil
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/local-captcha-result", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != endpoint.origin {
			http.Error(w, "invalid origin", http.StatusForbidden)
			return
		}
		notifyKey(keyCh, r.FormValue("token"))
		_, _ = fmt.Fprint(w, "ok")
	})

	mux.HandleFunc("/generic_proxy", func(w http.ResponseWriter, r *http.Request) {
		targetAuthURL := r.URL.Query().Get("proxy_url")
		targetParsed, err := neturl.Parse(targetAuthURL)
		if err != nil || !isAllowedCaptchaTarget(targetParsed) {
			http.Error(w, "URL is not an allowed VK HTTPS origin", http.StatusBadRequest)
			return
		}
		sameOrigin := sameCaptchaOrigin(targetParsed, targetURL)
		if !allowedGenericProxyMethod(r.Method, sameOrigin) {
			http.Error(w, "method not allowed for generic captcha proxy", http.StatusMethodNotAllowed)
			return
		}
		genericReverse := &httputil.ReverseProxy{
			Transport: transport,
			Rewrite: func(req *httputil.ProxyRequest) {
				rewriteGenericProxyRequest(req.Out, targetParsed, targetURL, endpoint)
			},
			ModifyResponse: func(response *http.Response) error {
				if !sameOrigin {
					// Cookies from CDN/auxiliary origins must not be collapsed
					// into the local proxy origin shared with the VK page.
					response.Header.Del("Set-Cookie")
				}
				return nil
			},
		}
		genericReverse.ServeHTTP(w, r)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[Captcha Proxy] HTTP %s %s", r.Method, r.URL.Path)
		if r.URL.RawQuery == localCaptchaEntryQuery {
			r.URL.RawQuery = targetURL.RawQuery
		}
		if r.URL.Path == "/" && targetURL.Path != "" && targetURL.Path != "/" && r.URL.RawQuery == "" {
			log.Printf("[Captcha Proxy] redirecting to captcha entry path")
			http.Redirect(w, r, localCaptchaEntryURLForTarget(targetURL, endpoint), http.StatusTemporaryRedirect)
			return
		}
		proxy.ServeHTTP(w, r)
	})

	return runCaptchaServerAndWait(
		ctx,
		endpoint,
		mux,
		localCaptchaEntryURLForTarget(targetURL, endpoint),
		keyCh,
		"proxy HTTP server error",
	)
}

func openBrowser(url string) {
	parsed, err := neturl.Parse(url)
	if err != nil || !isLoopbackHTTPURL(parsed) {
		log.Printf("refusing to open non-local captcha URL")
		return
	}
	for _, cmd := range browserOpenCommands(runtime.GOOS, url) {
		if err := exec.Command(cmd.name, cmd.args...).Start(); err == nil {
			return
		}
	}
}

func browserOpenCommands(goos string, url string) []browserCommand {
	switch goos {
	case "windows":
		return []browserCommand{{name: "rundll32.exe", args: []string{"url.dll,FileProtocolHandler", url}}}
	case "darwin":
		return []browserCommand{{name: "open", args: []string{url}}}
	case "linux":
		return []browserCommand{
			{name: "xdg-open", args: []string{url}},
			{name: "gio", args: []string{"open", url}},
		}
	case "android":
		return []browserCommand{
			{name: "termux-open-url", args: []string{url}},
			{name: "/system/bin/am", args: []string{"start", "-a", "android.intent.action.VIEW", "-d", url}},
			{name: "am", args: []string{"start", "-a", "android.intent.action.VIEW", "-d", url}},
			{name: "xdg-open", args: []string{url}},
		}
	case "ios":
		return []browserCommand{
			{name: "open", args: []string{url}},
			{name: "uiopen", args: []string{url}},
		}
	}
	return nil
}

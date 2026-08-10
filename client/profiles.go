package main

// Profile is a paired User-Agent and Client Hints set. One profile is used for
// every request of a single VK authentication chain, so the advertised browser
// identity stays self-consistent instead of looking synthetic to bot detection.
type Profile struct {
	UserAgent       string
	SecChUa         string
	SecChUaMobile   string
	SecChUaPlatform string
}

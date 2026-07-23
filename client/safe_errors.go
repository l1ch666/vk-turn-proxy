package main

import (
	"fmt"
	"sort"
	"strings"
)

// responseShapeError reports enough structure to diagnose an API schema change
// without copying tokens, credentials, URLs, or captcha payloads into logs.
func responseShapeError(message string, response map[string]interface{}) error {
	keys := make([]string, 0, len(response))
	for key := range response {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return fmt.Errorf("%s (response fields: %s)", message, strings.Join(keys, ", "))
}

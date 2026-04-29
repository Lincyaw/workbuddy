// Package sessionref centralises URL formatting for "view session" links so
// the reporter (which posts the URL into GitHub comments) and the dashboard
// JSON API (which surfaces the URL on issue/session listings) cannot drift.
package sessionref

import "net/url"

// BuildURL returns the canonical workers/<workerID>/sessions/<sessionID> URL
// rooted at baseURL, or an empty string when any required input is missing.
func BuildURL(baseURL, workerID, sessionID string) string {
	if baseURL == "" || sessionID == "" || workerID == "" {
		return ""
	}
	return baseURL + "/workers/" + url.PathEscape(workerID) + "/sessions/" + url.PathEscape(sessionID)
}

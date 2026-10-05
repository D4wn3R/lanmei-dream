package websearch

import (
	"io"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
)

var sensitiveQuery = regexp.MustCompile(`(?i)(-----BEGIN .*PRIVATE KEY|\b(?:sk-|ghp_|github_pat_)[a-z0-9_-]{12,}|\bAKIA[A-Z0-9]{16}\b|\bBearer\s+\S+|(?:api[_ -]?key|password|passwd|token|secret|cookie|密码|密钥|身份证|手机号|账号)\s*[:：=]|\beyJ[a-z0-9_-]+\.[a-z0-9_-]+\.[a-z0-9_-]+|\b1[3-9][0-9]{9}\b|[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,})`)

func validQuery(query string) bool {
	if !utf8.ValidString(query) || utf8.RuneCountInString(query) < 1 || utf8.RuneCountInString(query) > 200 || sensitiveQuery.MatchString(query) {
		return false
	}
	for _, r := range query {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

func cleanText(value string, limit int) string {
	z := html.NewTokenizer(strings.NewReader(value))
	var b strings.Builder
	skip := false
	for {
		switch z.Next() {
		case html.ErrorToken:
			if z.Err() != io.EOF {
				return ""
			}
			text := strings.Map(func(r rune) rune {
				if unicode.IsSpace(r) {
					return ' '
				}
				if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
					return -1
				}
				return r
			}, b.String())
			runes := []rune(strings.Join(strings.Fields(text), " "))
			if len(runes) > limit {
				runes = runes[:limit]
			}
			return string(runes)
		case html.StartTagToken:
			name, _ := z.TagName()
			if string(name) == "script" || string(name) == "style" {
				skip = true
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			if string(name) == "script" || string(name) == "style" {
				skip = false
			}
		case html.TextToken:
			if !skip {
				b.Write(z.Text())
				b.WriteByte(' ')
			}
		}
	}
}

func publicURL(raw string) string {
	if len(raw) > 2048 {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Hostname() == "" {
		return ""
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if !strings.Contains(host, ".") && !strings.Contains(host, ":") {
		return ""
	}
	for _, suffix := range []string{"localhost", ".localhost", ".local", ".internal", ".lan", ".home", ".test", ".invalid"} {
		if host == suffix || strings.HasSuffix(host, suffix) {
			return ""
		}
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
			netip.MustParsePrefix("100.64.0.0/10").Contains(ip) {
			return ""
		}
	} else {
		// 拒绝非标准数值 IP（如 127.1、十六进制写法），不做 DNS 或链接访问。
		if strings.HasPrefix(host, "0x") || strings.Trim(host, "0123456789.") == "" {
			return ""
		}
	}
	// 已知搜索引擎跳转包装无法可靠确认落点时直接舍弃。
	if ((host == "bing.com" || strings.HasSuffix(host, ".bing.com")) && strings.HasPrefix(u.Path, "/ck/")) ||
		((host == "baidu.com" || strings.HasSuffix(host, ".baidu.com")) && u.Path == "/link") ||
		((host == "google.com" || strings.HasSuffix(host, ".google.com")) && u.Path == "/url") ||
		((host == "duckduckgo.com" || strings.HasSuffix(host, ".duckduckgo.com")) && strings.HasPrefix(u.Path, "/l/")) {
		return ""
	}
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	return u.String()
}

func cleanResults(response SearchResponse, limit int) SearchResponse {
	cleaned := make([]SearchResult, 0, min(len(response.Results), limit))
	seen := make(map[string]bool)
	for _, result := range response.Results {
		result.URL = publicURL(result.URL)
		result.Title = cleanText(result.Title, 160)
		result.Snippet = cleanText(result.Snippet, 600)
		result.Engine = cleanText(result.Engine, 32)
		if result.URL == "" || result.Title == "" || seen[result.URL] {
			continue
		}
		seen[result.URL] = true
		cleaned = append(cleaned, result)
		if len(cleaned) == limit {
			break
		}
	}
	if len(cleaned) == 0 && response.Status != "no_results" {
		return failure("invalid_response")
	}
	response.Results = cleaned
	return response
}

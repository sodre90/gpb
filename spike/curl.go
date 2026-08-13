package main

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

type CapturedRequest struct {
	Method  string
	URL     string
	Headers http.Header
	Body    string
}

func (r *CapturedRequest) Cookie() string {
	if c := r.Headers.Get("Cookie"); c != "" {
		return c
	}
	return ""
}

func (r *CapturedRequest) CookieNames() []string {
	var names []string
	for _, pair := range strings.Split(r.Cookie(), ";") {
		name, _, found := strings.Cut(strings.TrimSpace(pair), "=")
		if found && name != "" {
			names = append(names, name)
		}
	}
	return names
}

func ParseCurl(src string) (*CapturedRequest, error) {
	tokens, err := tokenize(src)
	if err != nil {
		return nil, err
	}
	if len(tokens) == 0 || tokens[0] != "curl" {
		return nil, errors.New(`does not start with "curl" — paste the whole "Copy as cURL (bash)" output`)
	}

	req := &CapturedRequest{Headers: http.Header{}}
	for i := 1; i < len(tokens); i++ {
		token := tokens[i]
		next := func() (string, error) {
			if i+1 >= len(tokens) {
				return "", fmt.Errorf("flag %s has no value", token)
			}
			i++
			return tokens[i], nil
		}

		switch token {
		case "-H", "--header":
			value, err := next()
			if err != nil {
				return nil, err
			}
			name, headerValue, found := strings.Cut(value, ":")
			if !found {
				return nil, fmt.Errorf("malformed header %q", value)
			}
			req.Headers.Add(strings.TrimSpace(name), strings.TrimSpace(headerValue))
		case "-b", "--cookie":
			value, err := next()
			if err != nil {
				return nil, err
			}
			req.Headers.Set("Cookie", value)
		case "-X", "--request":
			value, err := next()
			if err != nil {
				return nil, err
			}
			req.Method = value
		case "-d", "--data", "--data-raw", "--data-binary", "--data-ascii":
			value, err := next()
			if err != nil {
				return nil, err
			}
			req.Body = value
		case "--compressed", "-s", "--silent", "-k", "--insecure", "-L", "--location", "-i", "--include", "-v", "--verbose", "-g", "--globoff":
		case "-o", "--output", "-A", "--user-agent", "-e", "--referer", "--connect-timeout", "--max-time", "--retry":
			if _, err := next(); err != nil {
				return nil, err
			}
		default:
			if strings.HasPrefix(token, "-") {
				continue
			}
			if req.URL == "" {
				req.URL = token
			}
		}
	}

	if req.URL == "" {
		return nil, errors.New("no URL found in curl command")
	}
	if req.Method == "" {
		if req.Body != "" {
			req.Method = http.MethodPost
		} else {
			req.Method = http.MethodGet
		}
	}
	return req, nil
}

func tokenize(src string) ([]string, error) {
	var (
		tokens  []string
		current strings.Builder
		started bool
	)
	flush := func() {
		if started {
			tokens = append(tokens, current.String())
			current.Reset()
			started = false
		}
	}

	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '\\' && i+1 < len(src) && (src[i+1] == '\n' || src[i+1] == '\r'):
			i += 2
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			flush()
			i++
		case c == '\'':
			end := strings.IndexByte(src[i+1:], '\'')
			if end < 0 {
				return nil, errors.New("unterminated single quote")
			}
			started = true
			current.WriteString(src[i+1 : i+1+end])
			i += end + 2
		case c == '$' && i+1 < len(src) && src[i+1] == '\'':
			text, next, err := readAnsiCQuoted(src, i+2)
			if err != nil {
				return nil, err
			}
			started = true
			current.WriteString(text)
			i = next
		case c == '"':
			text, next, err := readDoubleQuoted(src, i+1)
			if err != nil {
				return nil, err
			}
			started = true
			current.WriteString(text)
			i = next
		default:
			started = true
			current.WriteByte(c)
			i++
		}
	}
	flush()
	return tokens, nil
}

func readAnsiCQuoted(src string, start int) (string, int, error) {
	var out strings.Builder
	for i := start; i < len(src); {
		switch src[i] {
		case '\'':
			return out.String(), i + 1, nil
		case '\\':
			if i+1 >= len(src) {
				return "", 0, errors.New("dangling escape in $'...' string")
			}
			escape := src[i+1]
			i += 2
			switch escape {
			case 'n':
				out.WriteByte('\n')
			case 't':
				out.WriteByte('\t')
			case 'r':
				out.WriteByte('\r')
			case 'a':
				out.WriteByte(7)
			case 'b':
				out.WriteByte(8)
			case 'f':
				out.WriteByte(12)
			case 'v':
				out.WriteByte(11)
			case '0':
				out.WriteByte(0)
			case 'x':
				value, next, err := readHex(src, i, 2)
				if err != nil {
					return "", 0, err
				}
				out.WriteByte(byte(value))
				i = next
			case 'u':
				value, next, err := readHex(src, i, 4)
				if err != nil {
					return "", 0, err
				}
				out.WriteRune(rune(value))
				i = next
			default:
				out.WriteByte(escape)
			}
		default:
			out.WriteByte(src[i])
			i++
		}
	}
	return "", 0, errors.New("unterminated $'...' string")
}

func readHex(src string, start, maxDigits int) (int64, int, error) {
	end := start
	for end < len(src) && end-start < maxDigits && isHexDigit(src[end]) {
		end++
	}
	if end == start {
		return 0, 0, errors.New("empty hex escape")
	}
	value, err := strconv.ParseInt(src[start:end], 16, 32)
	if err != nil {
		return 0, 0, err
	}
	return value, end, nil
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func readDoubleQuoted(src string, start int) (string, int, error) {
	var out strings.Builder
	for i := start; i < len(src); {
		switch src[i] {
		case '"':
			return out.String(), i + 1, nil
		case '\\':
			if i+1 >= len(src) {
				return "", 0, errors.New("dangling escape in double-quoted string")
			}
			out.WriteByte(src[i+1])
			i += 2
		default:
			out.WriteByte(src[i])
			i++
		}
	}
	return "", 0, errors.New("unterminated double quote")
}

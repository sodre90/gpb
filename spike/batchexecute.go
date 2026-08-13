package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

const responsePrefix = ")]}'"

type Call struct {
	RPCID   string
	Payload string
}

type Frame struct {
	Tag     string
	RPCID   string
	Payload string
}

func DecodeRequestBody(body string) (calls []Call, token string, err error) {
	form, err := url.ParseQuery(body)
	if err != nil {
		return nil, "", fmt.Errorf("body is not form-encoded: %w", err)
	}
	token = form.Get("at")

	raw := form.Get("f.req")
	if raw == "" {
		return nil, token, errors.New("no f.req field in body — is this really a batchexecute request?")
	}

	var outer [][][]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &outer); err != nil {
		return nil, token, fmt.Errorf("f.req is not the expected [[[...]]] shape: %w", err)
	}
	if len(outer) == 0 {
		return nil, token, errors.New("f.req outer array is empty")
	}

	for _, entry := range outer[0] {
		if len(entry) < 2 {
			continue
		}
		var call Call
		if err := json.Unmarshal(entry[0], &call.RPCID); err != nil {
			continue
		}
		_ = json.Unmarshal(entry[1], &call.Payload)
		calls = append(calls, call)
	}
	if len(calls) == 0 {
		return nil, token, errors.New("f.req contained no decodable calls")
	}
	return calls, token, nil
}

func EncodeRequestBody(calls []Call, token string) string {
	entries := make([]any, 0, len(calls))
	for _, call := range calls {
		entries = append(entries, []any{call.RPCID, call.Payload, nil, "generic"})
	}
	encoded, _ := json.Marshal([]any{entries})

	form := url.Values{}
	form.Set("f.req", string(encoded))
	if token != "" {
		form.Set("at", token)
	}
	return form.Encode()
}

func RetargetURL(captured string, rpcID string, reqID int) (string, error) {
	parsed, err := url.Parse(captured)
	if err != nil {
		return "", err
	}
	query := parsed.Query()
	query.Set("rpcids", rpcID)
	query.Set("_reqid", strconv.Itoa(reqID))
	query.Set("rt", "c")
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func DecodeResponse(raw string) ([]Frame, error) {
	body := strings.TrimSpace(raw)
	if !strings.HasPrefix(body, responsePrefix) {
		return nil, fmt.Errorf("response lacks the %q guard prefix — got %.80q", responsePrefix, body)
	}
	body = strings.TrimPrefix(body, responsePrefix)

	var frames []Frame
	for cursor := 0; cursor < len(body); {
		cursor += skipChunkHeader(body[cursor:])
		if cursor >= len(body) {
			break
		}

		decoder := json.NewDecoder(strings.NewReader(body[cursor:]))
		var rows [][]json.RawMessage
		if err := decoder.Decode(&rows); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return frames, fmt.Errorf("chunk at offset %d is not a JSON array of frames: %w", cursor, err)
		}
		cursor += int(decoder.InputOffset())
		frames = append(frames, framesFromRows(rows)...)
	}
	return frames, nil
}

func skipChunkHeader(text string) int {
	cursor := 0
	for cursor < len(text) && unicode.IsSpace(rune(text[cursor])) {
		cursor++
	}

	digitsEnd := cursor
	for digitsEnd < len(text) && text[digitsEnd] >= '0' && text[digitsEnd] <= '9' {
		digitsEnd++
	}
	if digitsEnd == cursor || digitsEnd >= len(text) || text[digitsEnd] != '\n' {
		return cursor
	}

	for digitsEnd < len(text) && unicode.IsSpace(rune(text[digitsEnd])) {
		digitsEnd++
	}
	return digitsEnd
}

func framesFromRows(rows [][]json.RawMessage) []Frame {
	var frames []Frame
	for _, row := range rows {
		if len(row) == 0 {
			continue
		}
		var frame Frame
		if err := json.Unmarshal(row[0], &frame.Tag); err != nil {
			continue
		}
		if len(row) > 1 {
			_ = json.Unmarshal(row[1], &frame.RPCID)
		}
		if len(row) > 2 {
			_ = json.Unmarshal(row[2], &frame.Payload)
		}
		frames = append(frames, frame)
	}
	return frames
}

func PrettyJSON(raw string) string {
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return raw
	}
	formatted, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return raw
	}
	return string(formatted)
}
